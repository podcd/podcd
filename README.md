<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img src="assets/logo.svg" alt="podcd" width="200">
  </picture>
</p>

<p align="center">A Git-driven reconciler for Linux workloads.</p>

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

Install the binary.

```bash
V=2.0.0; A=amd64   # or arm64
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz"
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz.sha256"
sha256sum -c "podcd_${V}_linux_${A}.tar.gz.sha256" && tar -xzf "podcd_${V}_linux_${A}.tar.gz"
sudo install -m 0755 podcd /usr/local/bin/podcd
```

### Demo deploy

`bootstrap.sh` will attempt to install Podman, enable lingering so the user's services survive logout, downloads a release and verifies its checksum, points the agent at the example repository (whose `local` Host runs one nginx), and starts the agent as a systemd user service.

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/main/deploy/bootstrap.sh | sudo bash -s -- \
  --repo-url https://github.com/podcd/podcd.git --repo-path examples --host local
```

`--user` is omitted, so the script sets up the invoking user. Within a minute:

```bash
podcd status                          # last reconcile, and podcd-local running
curl -s http://127.0.0.1:8080/ | head -3
journalctl --user -u podcd-agent -f   # see pull, plan, apply
```

To see the loop by hand rather than wait for it, `podcd plan` shows what would change and `podcd reconcile` does it; a second `reconcile` reports `nothing to do`.

Tear down when done:

```bash
podcd teardown --purge-state --purge-config -y
```

This stops and removes every application podcd manages, then stops, disables and removes `podcd-agent.service`, it is the equivalent of running `systemctl --user disable --now podcd-agent.service` and deleting the unit file by hand. `--purge-state` also deletes `~/.local/state/podcd` (checkouts, played manifests, state.json); `--purge-config` also deletes `~/.config/podcd` (the repository URL, and any secrets in `agent.env`) - both are opt-in and skipped without them.

Building from source instead: `make build`, then the same bootstrap with `--binaries ./dist`.

For the demo variant of this, use your own user in place of `podcd`, skip step 2's `useradd`, and point step 4 at `https://github.com/podcd/podcd.git` with `--repo-path examples --host local`.

### Production deploy

#### 1. Setup GitOps repository

> [!TIP]
> `podcd validate` requires Secrets as references (`env:NAME`, `file:path`).

Setup a repository defining at least a `Host` and an `Application` or `Pod`. See [./examples](./examples/) or [podcd/podcd-gitops.git](https://github.com/podcd/podcd-gitops.git)
The files are the **desired state**. podcd reads the repository, resolves the configuration for a host, validates it, and reconciles the result with the workloads running on that host.
There is no prescribed directory structure. Organize the Git repository however you like.

* `podcd init` scaffolds a basic podcd-gitops repository.
* `podcd create` to generate a document per kind from flags.
* `podcd lint` to lint.

```bash
podcd lint                 # the current directory
podcd lint apps.yaml hosts.yaml
podcd lint --host prod-web-01 ./gitops
```

`podcd get` shows what a repository defines - by default every repository in `agent.yaml`, fetched as the agent fetches them. `--repo` narrows it to one: a repository name from `agent.yaml`, a local directory, or a git URL (tried in that order):

```bash
podcd get                          # every document, with its file and line
podcd get hosts                    # NAME  ENVIRONMENT  GROUPS  APPLICATIONS  SOURCE
podcd get application api -o yaml  # one document, as written
podcd get all --repo gitops        # one of the repositories in agent.yaml
podcd get all --repo ./gitops      # a checkout you are editing
podcd get pods --repo https://github.com/podcd/podcd-gitops.git
```

For a private repository, give the agent a read credential in `agent.yaml`.
A GitHub or GitLab deploy token over HTTPS:

```yaml
repositories:
  - name: gitops
    url: https://gitlab.com/your-user/gitops.git
    revision: main
    auth:
      username: gitlab+deploy-token-42   # GitLab deploy tokens have usernames; PATs and GitHub tokens can omit this
      token: env:GITOPS_TOKEN
```

Or an SSH deploy key:

```yaml
repositories:
  - name: gitops
    url: git@github.com:your-user/gitops.git
    auth:
      sshKeyPath: /home/podcd/.ssh/deploy_key        # used with IdentitiesOnly
      sshKnownHostsPath: /home/podcd/.ssh/known_hosts # optional
```

```bash
sudo -u podcd bash -lc 'ssh-keygen -t ed25519 -N "" -f ~/.ssh/deploy_key && ssh-keyscan github.com >> ~/.ssh/known_hosts'
# add ~/.ssh/deploy_key.pub as a read-only deploy key
```

#### 2. Deploy podcd

podcd needs to setup these files:
```text
~/.config/podcd/agent.yaml            agent config
~/.config/podcd/agent.env             secrets, 0600, never in Git
~/.config/containers/systemd/podcd-agent.
```

And in essence the following needs to happen:
1. Download `podcd` binary.
2. `podcd install`: generates a systemd service for the user
3. `podcd config create --host <host> --repo-url <repo> --revision <target_revision>`
4. `systemctl --user daemon-reload && systemctl --user enable --now podcd-agent.service`

`bootstrap.sh` is not more than a helper utility.

##### 2.1 Automated setup

The same bootstrap as the demo, with a dedicated service user, your repository and a pinned release.
One command per host; it is idempotent.

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/main/deploy/bootstrap.sh | sudo bash -s -- \
  --release-version 2.0.0 \
  --user podcd \
  --revision main \
  --host hostname \  # ideally you leave the host out and use machine names in your gitops repo
  --repo-url git@github.com:you/gitops.git

```

It detects the distribution (apt on Debian/Ubuntu, dnf on the RHEL family), installs Podman, creates the `podcd` service user (no login shell; add `--allow-user-login` only if you want one), grants it a subordinate uid range, enables lingering so applications come back after a reboot, downloads the release you named and verifies its checksum, writes the agent config, installs the user service and starts it.

On SELinux-enforcing hosts (the RHEL default) bind mounts need the `Z` (or `z`) option so the container may read them - `volumes: [{source: /srv/data, destination: /data, options: Z}]`;ConfigMap and Secret volumes on Pods are labelled by podman itself.

Everything the agent owns is under its user `/home/podcd`:

```text
~/.config/podcd/agent.yaml          agent config
~/.config/podcd/agent.env           secrets, 0600, never in Git
~/.config/containers/systemd/       the Quadlet units podcd wrote
~/.local/state/podcd/               checkouts, played manifests, state.json
```

##### 2.2 Manual setup

```bash
# 1.  Packages: rootless Podman 4.4+ (with Quadlet), git
apt-get install -y podman uidmap dbus-user-session git   # Debian/Ubuntu
dnf install -y podman shadow-utils git                    # RHEL family
test -x /usr/libexec/podman/quadlet || echo "this podman has no Quadlet; 4.4+ is required"
```
```bash
# 2.  The service user: no login shell, its own subordinate uid range for rootless containers.
#     lingering so its services start at boot.
useradd --create-home --shell /usr/sbin/nologin podcd     # /sbin/nologin on RHEL
grep -q '^podcd:' /etc/subuid || usermod --add-subuids 200000-265535 --add-subgids 200000-265535 podcd
loginctl enable-linger podcd
```
```bash
# 3.  The agent config and the secrets file
sudo -Hu podcd podcd config create --host hostname --repo-url git@github.com:you/gitops.git --revision main
sudo -Hu podcd bash -c 'umask 077 && touch ~/.config/podcd/agent.env'
```
```bash
# 3.1 or directly as the podcd user
podcd config create --host hostname --repo-url git@github.com:you/gitops.git --revision main
touch ~/.config/podcd/agent.env
# edit/provision secrets in.
```
```bash
# 4.  The user service.
sudo -Hu podcd podcd install
sudo -Hu podcd XDG_RUNTIME_DIR="/run/user/$(id -u podcd)" systemctl --user daemon-reload
sudo -Hu podcd XDG_RUNTIME_DIR="/run/user/$(id -u podcd)" systemctl --user enable --now podcd-agent.service
```
```bash
# 4.1 directly as the user
podcd install
systemctl --user daemon-reload
systemctl --user enable --now podcd-agent.service
```

#### 3. Secrets

A secret referenced in Git as `env:DATABASE_PASSWORD` is looked up in the agent's environment, which systemd loads from `~podcd/.config/podcd/agent.env`:

```bash
sudo -u podcd bash -lc 'umask 077 && printf "DATABASE_PASSWORD=%s\n" "$(cat /path/to/secret)" >> ~/.config/podcd/agent.env'
```

The agent re-reads that file on every lookup, so no restart is needed: rotating a value changes the application's spec hash and the next reconcile restarts the application. `file:` references read files under `secretsDir` instead; use that for values delivered by another tool.

**HashiCorp Vault.** With a `vault:` section in `agent.yaml`, references resolve against Vault (KV v1 or v2), authenticated with AppRole or a token. Both of these name the same value - the key is always the last segment:

```text
vault:secret/prod/api/DATABASE_PASSWORD     mount first, as in `vault kv get`
vault:prod/api/DATABASE_PASSWORD@secret     mount after @
```

```yaml
vault:
  address: https://vault.example.com
  roleId: env:VAULT_ROLE_ID       # from agent.env - Vault credentials are references too
  secretId: env:VAULT_SECRET_ID
  # token: env:VAULT_TOKEN        # alternative to AppRole
  # namespace: team-a             # Vault Enterprise
  # caCert: /etc/pki/vault-ca.pem
  # kvVersion: 2
```

The agent logs in when it first needs a value, re-logs-in when the token is rejected, and caches reads briefly so a reconcile with many keys from one path is one round trip. Only static KV values make sense here: a dynamic credential that changed on every read would restart the application on every reconcile.

#### 4. Status

The service user by default has no login shell, so run commands through `sudo -u`, from its home (rootless Podman refuses to run from a directory it cannot read):

```bash
sudo -u podcd bash -lc 'cd && podcd status'
sudo -u podcd bash -lc 'cd && podcd health'
sudo journalctl _UID="$(id -u podcd)" -u podcd-agent --user -f
# systemctl --user for another user needs its runtime dir spelled out:
sudo -u podcd XDG_RUNTIME_DIR="/run/user/$(id -u podcd)" systemctl --user status podcd-agent.service
```

`status` shows the last successful and failed reconcile and, per application, the unit state, the image and the last health result. `health` exits non-zero if anything is unhealthy, which makes it a usable check for your monitoring; add `-o json` to the commands for a scraper.

### Configuration file

```yaml
# podcd agent configuration.
# Which Host document this machine is. Empty means the system hostname (with
# any domain stripped). PODCD_HOST in the environment overrides both.
host: local

# How often to pull and reconcile.
# interval: 1m0s

# Random extra delay on top of interval, so a fleet does not hit Git in
# lockstep.
# jitter: 10s

# Wait after a failed reconcile; doubles on each further failure.
# retryInterval: 15s

# Cap for the retry backoff.
# maxRetryInterval: 10m0s

# Container runtime. Only podman (rootless, via Quadlet) is implemented.
# runtime: podman

# One or more repositories, composed into a single desired state. A name
# defined twice across them is an error, not a race.
repositories:
  # A short name for this repository; it appears in logs and status.
  - name: infrastructure
    # Where to fetch from: https://, ssh (git@host:path) or a local path.
    url: git@github.com:podcd/podcd-gitops.git
    # A branch, a tag or a commit. A tag or commit pins the host.
    revision: main
    # Read only this subdirectory of the repository.
    # path: ""
    # Disable host key / TLS verification. Visible here on purpose, rather
    # than an environment variable nobody sees.
    # insecure: false
    # A private repository needs a read credential. The token is a secret
    # reference (env:, file:, vault:), never a literal in this file; it is
    # resolved on every fetch. GitLab deploy tokens have their own username.
    # auth:
    #   username: gitlab+deploy-token-42
    #   token: env:GITOPS_TOKEN
    #   sshKeyPath: ~/.ssh/deploy_key
    #   sshKnownHostsPath: ~/.ssh/known_hosts

# Where the agent keeps checkouts, played manifests and state.json.
# stateDir: /home/podcd/.local/state/podcd

# Where Quadlet units are written. Must be a directory systemd --user reads.
# unitDir: /home/podcd/.config/containers/systemd

# Root for relative file: secret references.
# secretsDir: ""

# KEY=value file read for env: references (and loaded by the systemd unit).
# 0600, never in Git; re-read on every lookup so rotation needs no restart.
# envFile: /home/podcd/.config/podcd/agent.env

# Remove applications that Git no longer declares. On by default; leaving
# orphans running is its own kind of drift.
# prune: true

# text or json.
# logFormat: text

# HashiCorp Vault, for vault:<mount>/<path>/<key> (or <path>/<key>@<mount>)
# references. Vault's own credentials are references as well, so they come
# from the env file rather than from this file.
# vault:
#   address: https://vault.example.com
#   roleId: env:VAULT_ROLE_ID
#   secretId: env:VAULT_SECRET_ID
```

Fields are addressed by their yaml path:

```bash
podcd config set revision v1.4.0                 # alias for repositories.0.revision
podcd config set interval 30s
podcd config set repositories.0.auth.token env:GITOPS_TOKEN
# fields that only make sense together are set together and validated once:
podcd config set vault.address=https://vault.example.com vault.roleId=env:VAULT_ROLE_ID vault.secretId=env:VAULT_SECRET_ID
podcd config view
```

### Useful commands

```bash
podcd status      # what is running here and when it last reconciled
podcd plan        # show changes without making them
podcd reconcile   # apply the current Git desired state
podcd health      # probe application health; non-zero exit if anything is unhealthy
podcd logs api    # recent output for one application (--tail N)
podcd validate    # compile config and check for errors
podcd lint        # check repository files, for every host, without fetching
podcd get         # what a repository defines (agent.yaml's, or --repo NAME|DIR|URL)
podcd init        # scaffold a minimal repository: one host, one nginx
podcd create      # print a document for a kind, built from flags
podcd install     # write the systemd user service file for the agent
podcd uninstall   # stop the agent's systemd user service and remove its unit file
podcd prune       # stop and remove applications directly, without consulting Git (--all, or by name)
podcd teardown    # prune --all, then uninstall; --purge-state and --purge-config to also delete local state/config
podcd config      # view, create or edit the agent config file
```

### Shell completion

```bash
# bash
podcd completion bash | sudo tee /etc/bash_completion.d/podcd > /dev/null
# zsh
mkdir -p ~/.zsh/completions && podcd completion zsh > ~/.zsh/completions/_podcd   # then add ~/.zsh/completions to fpath before compinit
```

fish and PowerShell: `podcd completion --help`.

### Container image

```bash
make image                                       # builds podcd:$(VERSION) from Containerfile
podman run --rm -v /repo:/repo:ro,Z podcd:dev lint /repo
```

The image runs as a fixed non-root UID (1000); rootless Podman remaps that into your subordinate uid/gid range the same way it does for any other container, nothing special to configure there. On SELinux-enforcing hosts (RHEL, Fedora), add `:z` (shared) or `:Z` (private) to any bind mount as above, or the mount is denied - Debian/Ubuntu/Alpine don't need this since they don't enforce SELinux.

## State and History

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
  image: docker.io/library/nginx@sha256:72ba65eb42c10344912a84ff42408db7d34f2feb642204570ab8fc5ffd29f1d3
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

A host's application list can also name a plain Kubernetes `Pod`.
Pods are validated by the same rules as applications:

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
      image: docker.io/library/nginx@sha256:72ba65eb42c10344912a84ff42408db7d34f2feb642204570ab8fc5ffd29f1d3
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

Repositories are composed into one desired state. A name defined twice across them is an error, not a race; `revision` may be a branch, a tag or a commit.

```yaml
repositories:
  - name: infrastructure
    url: https://github.com/podcd/podcd.git
    revision: main
    path: examples
  - name: applications
    url: https://github.com/your-user/applications.git
    revision: v1.4.0
```

## Testing

```bash
make test           # unit tests; also runs podman's Quadlet generator over rendered units
make test-e2e       # real podman, quadlet and systemd on this machine (starts containers)
make lint           # golangci-lint
```

## Security

podcd is designed to keep the host-side trust boundary simple and explicit. The agent reads its config pointing to its git repos, resolves secrets locally, and writes generated unit files and runtime state in a controlled location.

Before enabling a deployment, validate the rendered configuration and review the plan output. The project expects deterministic behavior and clear failures when configuration is ambiguous or invalid.

## Contributing

Contributions are welcome. If you are working on a change, keep the scope focused and validate the relevant tests before submitting.

## License

This project is licensed under the MIT License. See the license in the repository for full details.
