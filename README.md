<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img src="assets/logo.svg" alt="podcd" width="200">
  </picture>
</p>

<p align="center">A Git-driven reconciler for Linux workloads.</p>
<p align="center"><a href="https://podcd.github.io/podcd/">Documentation</a></p>

podcd reconciles a Linux host to match what is declared in Git.

- Git defines the desired state.
- A local agent reads that state and identifies the host.
- The agent compiles the resulting configuration.
- Quadlet and systemd manage the running containers.

## Get started

Install the binary.

```bash
V=2.3.0; A=amd64   # or arm64
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz"
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz.sha256"
sha256sum -c "podcd_${V}_linux_${A}.tar.gz.sha256" && tar -xzf "podcd_${V}_linux_${A}.tar.gz"
sudo install -m 0755 podcd /usr/local/bin/podcd
```

### Demo deploy

`bootstrap.sh` installs Podman, downloads and verifies the release, points the agent at the example repository (whose `local` Host runs one nginx), and starts the agent as a systemd user service.

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

Stops all managed applications and removes the agent service. `--purge-state` also deletes `~/.local/state/podcd`; `--purge-config` also deletes `~/.config/podcd`.

Building from source: `make build`, then the same bootstrap with `--binaries ./dist`.

### Production deploy

#### 1. Set up the GitOps repository

> [!TIP]
> `podcd lint` checks the files without fetching anything. `podcd validate` compiles for this host, which pulls Git and reads any secret the host's workloads reference.

Set up a repository with at least a `Host` and a `Pod`. See [./examples](./examples/) or [podcd/podcd-gitops.git](https://github.com/podcd/podcd-gitops.git). No prescribed directory structure.

- `podcd init` scaffolds a minimal repository.
- `podcd create` generates documents.
- `podcd lint` checks them.

For a private repository, add a read credential in `agent.yaml`. See [installation docs](https://podcd.github.io/podcd/docs/installation#private-repositories).

#### 2. Deploy podcd

```bash
podcd config create --host <hostname> --repo-url <url> --revision main
podcd install
systemctl --user daemon-reload && systemctl --user enable --now podcd-agent.service
```

`podcd config create` writes `~/.config/podcd/agent.yaml`. `podcd install` writes the systemd user service. Add secrets to `~/.config/podcd/agent.env` (0600, never in Git).

For a fresh machine that also needs Podman installed and a dedicated service user, `bootstrap.sh` handles it. One command per host; idempotent:

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/main/deploy/bootstrap.sh | sudo bash -s -- \
  --release-version 2.3.0 \
  --user podcd \
  --revision main \
  --repo-url git@github.com:you/gitops.git
```

On SELinux-enforcing hosts (RHEL default) bind mounts need the `Z` option: `volumes: [{source: /srv/data, destination: /data, options: Z}]`.

#### 3. Secrets

Anything that must stay out of Git is declared as an `ExternalSecret` naming a key in a `SecretStore`, and fetched on the host while the workload is compiled. Three backends are supported: the agent's environment (`env`), files on the host (`file`), and HashiCorp Vault (`vault`).

See [Secrets](https://podcd.github.io/podcd/docs/configuration/secrets).

#### 4. Status

When running as a dedicated service user with no login shell:

```bash
sudo -u podcd bash -lc 'cd && podcd status'
sudo -u podcd bash -lc 'cd && podcd health'
sudo journalctl _UID="$(id -u podcd)" -u podcd-agent --user -f
sudo -u podcd XDG_RUNTIME_DIR="/run/user/$(id -u podcd)" systemctl --user status podcd-agent.service
```

`status` shows the last reconcile and per-application unit state. `health` exits non-zero if anything is unhealthy; add `-o json` for a scraper.

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
    # Values file(s), relative to this repository's tree (path, if set), for
    # {{ .Values }} templating of *.tpl files - a host-local fallback
    # beneath what Host, Group and Environment documents declare. Repeated
    # files merge, later ones winning per key.
    # values: []
    # Disable host key / TLS verification. Visible here on purpose, rather
    # than an environment variable nobody sees.
    # insecure: false
    # A private repository needs a read credential. The token is a secret
    # reference (env: or file:), never a literal in this file; it is
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

```

Fields are addressed by their yaml path:

```bash
podcd config set revision v1.4.0                 # alias for repositories.0.revision
podcd config set interval 30s
podcd config set repositories.0.auth.token env:GITOPS_TOKEN
podcd config view
```

### Useful commands

```bash
podcd status      # what is running here and when it last reconciled
podcd plan        # show changes without making them
podcd reconcile   # apply the current Git desired state
podcd health      # report application health; non-zero exit if anything is unhealthy
podcd logs api    # recent output for one application (--tail N)
podcd validate    # compile config and check for errors
podcd lint        # check repository files, for every host, without fetching
podcd get         # what a repository defines (agent.yaml's, or --repo NAME|DIR|URL)
podcd init        # scaffold a minimal repository: one host, one nginx
podcd create      # print a document for a kind
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
make image                                              # builds podcd:$(VERSION) from Containerfile
podman run --rm -v /repo:/repo:ro,Z ghcr.io/podcd/podcd:latest lint /repo
```

The image runs as a fixed non-root UID (1000); rootless Podman remaps that into your subordinate uid/gid range the same way it does for any other container, nothing special to configure here.

On SELinux-enforcing hosts (RHEL, Fedora), add `:z` (shared) or `:Z` (private) to any bind mount as above, or the mount is denied - Debian/Ubuntu/Alpine don't need this since they don't enforce SELinux.

The image carries `git`, `podman` and `systemctl`, so every podcd command runs in it, including `run`/`reconcile` - but those manage *the host's* rootless Podman and systemd `--user` session, so from inside a container they need the host's Podman socket and systemd user bus bind-mounted in.

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
- `Pod`: a workload definition referenced by name

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

### Pod example

A workload is a plain Kubernetes `Pod`, played by podman through a Quadlet `.kube` unit. Pods are validated before anything is written: referenced ConfigMaps and Secrets must exist or be marked optional, and host ports must not clash.

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
      livenessProbe:
        httpGet: { path: /ready, port: 9200 }
```

podman runs the `livenessProbe` and reports the verdict; podcd reads it back and never probes anything itself. `readinessProbe` and `startupProbe` are ignored by podman. Networks have no Kubernetes field, so podcd takes them as an annotation - `io.podcd.networks: "edge,monitoring"` - and turns them into the unit's `Network=` lines.

Secrets are declared in Git as `ExternalSecret` + `SecretStore` documents and fetched while the workload is compiled. The pod references them by name via `secretKeyRef` or `envFrom: secretRef` as normal Kubernetes API. See [Secrets](https://podcd.github.io/podcd/docs/configuration/secrets).

### Inheritance and merge rules

Precedence runs from lowest to highest:

```text
Pod -> Environment override -> Group overrides -> Host override
```

Groups are applied in the order listed by the host, so later entries win. An override is a strategic merge patch against the Pod, so it follows Kubernetes' own rules: containers merge by `name`, ports by `containerPort`, environment variables by `name`, and a plain list is replaced wholesale.

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

### Values templating

Overrides parametrize one application by its name as key, an override can only name an application every host in that layer actually runs.

Values templating parametrizes the documents as well, so several hosts can share one `Pod` definition and each fill in the parts that differ.

A template is a file named `*.tpl` (e.g. `edge-api.yaml.tpl`). A template is rendered for each host against that host's values and only then decoded.

Templates may render any deployable kind - `Pod`, `ConfigMap`, `Secret` - but not `Host`, `Group` or `Environment`, since those are what decide a host's values in the first place.

```yaml
# apps/edge-api.yaml.tpl
apiVersion: v1
kind: Pod
metadata:
  name: edge-api
spec:
  containers:
    - name: edge-api
      image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
      resources:
        limits:
          memory: "{{ default \"256Mi\" .Values.resources.memory }}"
```

#### Where values come from

A `Host`, `Group` or `Environment` document names its own values files, merged into that host with exactly the precedence overrides already use - environment, then each group in the host's listed order, then the host itself, host winning:

```yaml
# environments/prod.yaml
apiVersion: gitops.podcd.io/v1
kind: Environment
metadata:
  name: prod
spec:
  applications: [edge-api]
  values:
    - values/prod.yaml   # repeatable; later files in the list win per key
```

Example values:

```yaml
# values/prod.yaml
image:
  repository: registry.example.com/edge-api
  tag: "1.4.0"
resources:
  memory: 512M
```

`agent.yaml`'s `repositories[].values` adds a lowest-precedence layer, resolved on the host. Use it for host-local fallbacks; prefer declaring values in `Host`/`Group`/`Environment` documents.

```yaml
repositories:
  - name: gitops
    url: https://github.com/your-user/podcd-gitops.git
    revision: main
    values:
      - values/common.yaml
```
See [`podcd-gitops/multi-env`](https://github.com/podcd/podcd-gitops/tree/main/multi-env) for a complete example.

#### Functions

Go's `text/template`, plus:

| Function | Use |
|---|---|
| `default DEF VAL` | `VAL` if set, else `DEF`. For anything genuinely optional. |
| `required MSG VAL` | `VAL` if set, else fails the render with `MSG`. Per host: a host that never selects the application never renders its template, so only hosts that actually need the value have to supply it. |
| `upper`, `lower`, `trim` | The obvious string transforms. |
| `trimPrefix P S`, `trimSuffix SUF S` | Strip a fixed prefix/suffix from `S`. |
| `replace OLD NEW S` | `strings.ReplaceAll`. |
| `quote V` | Go-quote a value, for embedding it as a JSON/YAML string literal. |


## Testing

```bash
make test           # unit tests; also runs podman's Quadlet generator over rendered units
make test-e2e       # real podman, quadlet and systemd on this machine (starts containers)
make lint           # golangci-lint
```

## Security

Report security issues through [GitHub's private vulnerability reporting](https://github.com/podcd/podcd/security/advisories/new).

## Contributing

See [Contributing](docs/docs/community/contribution.md).

## License

MIT. See [LICENSE](LICENSE).
