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

On a Debian or Ubuntu VM:

```bash
make build
sudo ./deploy/bootstrap.sh \
  --repo-url https://git.example.com/infrastructure.git \
  --repo-path examples \
  --user podcd \
  --host prod-web-01 \
  --binaries ./dist
```

This installs Podman, creates an unprivileged `podcd` user, enables lingering so workloads come back after reboot, and starts the agent as a systemd user service.

From then on, the host reconciles itself automatically.

### Useful commands

```bash
podcd status      # what is running here and when it last reconciled
podcd plan        # show changes without making them
podcd reconcile   # apply the current Git desired state
podcd health      # probe application health
podcd logs api    # recent output for one application
podcd validate    # compile config and check for errors
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
  name: prod-web-01
spec:
  environment: production
  groups:
    - web
  applications:
    - debug-tools
  excludeApplications:
    - frontend
  overrides:
    api:
      env:
        LOG_LEVEL: debug
```

### Application example

```yaml
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: api
spec:
  image: docker.io/library/nginx:latest
  ports:
    - host: 8080
      container: 8080
      hostIP: 127.0.0.1
  env:
    APP_ENV: production
  secretEnv:
    DATABASE_PASSWORD: env:API_DATABASE_PASSWORD
  restartPolicy: always
  healthcheck:
    http:
      port: 8080
      path: /health
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
    url: https://git.example.com/infrastructure.git
    revision: main
  - name: applications
    url: https://git.example.com/applications.git
    revision: main
    path: clusters/prod
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
