# podcd

A Git-driven reconciler for Linux workloads.

podcd is a small system for declaring what should be running on a VM in Git, then reconciling the host to match that desired state continuously.
It is designed for ordinary Linux hosts.

The model is intentionally simple:

- Git defines the desired state.
- A local agent reads that state and identifies the host.
- The agent compiles the resulting configuration.
- Quadlet and systemd manage the running containers.

```
Git desired state
  -> agent pull + host match + plan
  -> Quadlet units
  -> running app
```

podcd is for teams that want disciplined, declarative deployment on regular Linux VMs while keeping the operational model lightweight and auditable.

## Why podcd?

podcd is built around a few principles:

- Declarative configuration in Git
- Host-specific reconciliation with explicit inheritance
- Rootless Podman workloads managed by systemd
- Deterministic compilation and clear validation errors
- A small, operationally simple control plane

This is not Kubernetes, it is a focused GitOps model for Linux hosts that need predictable service deployment without a cluster control plane.

## Get started

### Quick install

On a Debian or Ubuntu VM, you can install the latest published release directly with:

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/refs/heads/main/deploy/bootstrap.sh | sudo bash -s -- \
  --repo-url https://github.com/podcd/podcd.git \
  --repo-path examples \
  --host local \
  --user podcd
```

This downloads the latest release binaries from GitHub, installs Podman, creates or reuses a dedicated `podcd` service user, enables lingering so workloads come back after reboot, and starts the agent as a systemd user service.

```bash
sudo podcd
```

By default, the bootstrap script creates a non-login system account for the agent. The service account is not a human login account; its shell is `/usr/sbin/nologin` unless you explicitly opt in with `--allow-user-login`.

If you want the service user to be able to log in as a shell, use:

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/refs/heads/main/deploy/bootstrap.sh | sudo bash -s -- \
  --repo-url https://github.com/podcd/podcd.git \
  --repo-path examples \
  --user podcd \
  --host local \
  --allow-user-login
```

If you want a dedicated service account, pass `--user`; the script will create that user if it does not already exist, or reuse it if it does. If you prefer the secure default, omit `--allow-user-login` and leave the account locked down. If you omit `--user` entirely, the script uses the current user (or the invoking `sudo` user when run as root).

If you already have the binary installed, the service can also be written directly with:

```bash
podcd install
podcd install -y
```

The first form prompts before overwriting an existing service file; `-y` forces the overwrite.

The agent config file is also managed directly by the CLI:

```bash
podcd config path
podcd config create --repo-url https://github.com/podcd/podcd.git --repo-path examples --host local
```

This writes the default config to `~/.config/podcd/agent.yaml` unless `PODCD_CONFIG` or `--path` is set.

The actual agent configuration file looks like this:

```yaml
# ~/.config/podcd/agent.yaml
host: prod-web-01
interval: 60s
jitter: 10s
runtime: podman

repositories:
  - name: gitops
    url: https://github.com/your-user/gitops.git
    revision: main
    path: .

stateDir: /home/podcd/.local/state/podcd
unitDir: /home/podcd/.config/containers/systemd
secretsDir: /home/podcd/.local/share/podcd/secrets
envFile: /home/podcd/.config/podcd/agent.env
prune: true
logFormat: text
```

This is the file the agent reads to decide which Git repository to reconcile.

### Configure against your own GitOps repository

If your desired state lives in your own `podcd-gitops.git` repository, point the agent at that repo instead of the example checkout:

```bash
# As the podcd user
podcd config create \
  --path /home/podcd/.config/podcd/agent.yaml \
  --host vm-prod \
  --repo-url https://github.com/your-user/podcd-gitops.git \
  --repo-path . \
  --revision main

podcd validate
podcd plan
```

If you are invoking `podcd` as the service user from another directory, always switch into the user's home first. The current working directory is inherited from the shell, and rootless Podman refuses to run in a directory the `podcd` user cannot access. i.e:
```bash
sudo -u podcd bash -lc 'cd /home/podcd && podcd validate'
sudo -u podcd bash -lc 'cd /home/podcd && podcd plan'
```

Once the config is in place, install the user service file for that same account and enable the agent:

```bash
# As the podcd user
cd /home/podcd && podcd install -y
systemctl --user daemon-reload
systemctl --user enable --now podcd-agent.service
```

That keeps the service account, its config, and its runtime state all rooted under `/home/podcd` rather than the root account's home directory.

### Manual build and bootstrap

```bash
make build
mv dist/podcd-agent /usr/local/bin
mv dist/podcd /usr/local/bin

```

From then on, the host reconciles itself automatically.

### Useful commands

```bash
podcd status      # what is running here and when it last reconciled
podcd plan        # show changes without making them
podcd reconcile   # apply the current Git desired state
podcd health      # probe application health
podcd logs local  # recent output for one application
podcd validate    # compile config and check for errors
podcd install     # write the systemd user service file for the agent
```

## How it works

The agent stores local state in `~/.local/state/podcd/state.json`. This file records the agent identity, last revisions, last successful and failed reconciliation, and per-application history including the previous deployment.

That state is metadata only and is safe to delete. The agent can reconstruct its state from Git and the host.

## Configuration model

podcd reads documents from one or more Git repositories. All config documents share the same API version:

```yaml
apiVersion: gitops.podcd.io/v1
```

The core document types are:

- `Environment`: values that apply broadly to a whole environment
- `Group`: a role or machine-purpose definition
- `Host`: a specific VM, including its environment, groups, and overrides
- `Application`: a reusable application definition referenced by name

### Host example

```yaml
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: local
spec:
  environment: local
  groups:
    - local
```

### Application example

```yaml
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: local
spec:
  image: docker.io/library/nginx:latest
  ports:
    - host: 8080
      container: 80
      hostIP: 127.0.0.1
  restartPolicy: always
  healthcheck:
    http:
      port: 8080
      path: /
```

### Pod manifests

Pod definitions follow the same rules as applications. They are validated for things such as:

- immutable image references unless explicitly allowed
- referenced ConfigMaps and Secrets that must exist or be optional
- host port conflicts
- the first readiness or liveness probe on a published port becoming the health check

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: metrics
spec:
  containers:
    - name: collector
      image: docker.io/library/nginx:latest
      envFrom:
        - configMapRef: { name: metrics-config }
      ports:
        - containerPort: 9200
          hostPort: 9200
      readinessProbe:
        httpGet: { path: /ready, port: 9200 }
```

Secrets in Git hold references, not plaintext values. The `data:` field is rejected, and every `stringData` entry must resolve to either `env:NAME` or `file:path`. Values are resolved on the host, written to a 0600 file outside the unit directory, and displayed as a hash in `podcd plan`.

Overrides for a Pod use strategic merge semantics. Containers merge by name and ports merge by `containerPort`, which matches the expectation of a Pod author:

```yaml
overrides:
  metrics:
    spec:
      containers:
        - name: shipper
          args: ["--target", "http://127.0.0.1:9200", "--verbose"]
```

### Inheritance and merge rules

Precedence runs from lowest to highest:

```text
Application -> Environment override -> Group overrides -> Host override
```

Groups are applied in the order listed by the host, so later entries win. Scalar values and map entries are overridden key-by-key; lists such as ports, volumes, and command arguments are replaced wholesale because appending to a port list has no sane meaning.

The final application set is the union of what the environment, groups, and host request, minus any exclusions from the host, sorted by name.

Two compiler runs on the same commit produce the same bytes. Ambiguity is treated as an error, not a guess. Examples include:

- an application defined twice across repositories
- a reference to a missing application
- an unknown group
- a host with no `Host` document
- a misspelled field
- two applications contending for the same host port

Each failure names the offending file and explains the issue clearly.

### Multiple repositories

```yaml
repositories:
  - name: infrastructure
    url: https://github.com/podcd/podcd.git
    revision: main
    path: examples
```

## Testing

```bash
make test           # unit tests
make test-e2e       # podman, quadlet, systemd validation on the local machine
```

## Security

podcd is designed to keep the host-side trust boundary simple and explicit. The agent reads its config pointing to its git repos, resolves secrets locally, and writes generated unit files and runtime state in a controlled location.

Before enabling a deployment, validate the rendered configuration and review the plan output. The project expects deterministic behavior and clear failures when configuration is ambiguous or invalid.

## Contributing

Contributions are welcome. If you are working on a change, keep the scope focused and validate the relevant tests before submitting.

## License

This project is licensed under the MIT License. See the license in the repository for full details.
