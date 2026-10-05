---
id: installation
title: Installation
---

## Install the binary

```bash
V=$(curl -fsSLI https://github.com/podcd/podcd/releases/latest \
  | sed -n 's/^[Ll]ocation:.*\/tag\/v\([^[:space:]]*\).*/\1/p' \
  | tr -d '\r')

A=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')

curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz"
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz.sha256"

sha256sum -c "podcd_${V}_linux_${A}.tar.gz.sha256" &&
  tar -xzf "podcd_${V}_linux_${A}.tar.gz" &&
  sudo install -m 0755 podcd /usr/local/bin/podcd
```

Or build from source: `make build`.

## Demo deploy

`bootstrap.sh` installs Podman, enables lingering, downloads and verifies a release, points the agent at the example repository (the `local` Host defines one nginx), and starts the agent as a systemd user service.

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/main/deploy/bootstrap.sh | sudo bash -s -- \
  --repo-url https://github.com/podcd/podcd.git --repo-path examples --host local
```

Without `--user`, the script sets up the invoking user and within a minute:

```bash
podcd status                          # last reconcile, and podcd-local running
curl -s http://127.0.0.1:8080/ | head -3
journalctl --user -u podcd-agent -f   # see pull, plan, apply
```

`podcd plan` shows what would change, `podcd reconcile` executes a manual reconcile.
Tear down when done:

```bash
podcd teardown --purge-state --purge-config -y
```

## Production deploy

### Set up the GitOps repository

A repository needs at least a `Host` and a `Pod`; there is no prescribed directory structure.

See the [configuration model](configuration/model.md), [`examples/`](https://github.com/podcd/podcd/tree/main/examples) or [podcd/podcd-gitops](https://github.com/podcd/podcd-gitops). 

You could use `podcd init` to scaffold a podcd gitops repository as well as `podcd create` to output a single podcd manifest.

```bash
podcd lint                          # current directory
podcd lint --host prod-web-01 ./gitops
podcd get all --repo ./gitops       # inspect what a checkout defines
```

#### Private repositories

Add a read credential in `agent.yaml`. HTTPS token:

```yaml
repository:
  name: gitops
  url: https://gitlab.com/your-user/gitops.git
  revision: main
  auth:
    username: gitlab+deploy-token-42   # GitLab tokens have usernames; GitHub PATs can omit this
    token: env:GITOPS_TOKEN
```

SSH deploy key:

```yaml
repository:
  name: gitops
  url: git@github.com:your-user/gitops.git
  auth:
    sshKeyPath: ~/.ssh/deploy_key
    sshKnownHostsPath: ~/.ssh/known_hosts
```

```bash
ssh-keygen -t ed25519 -N "" -f ~/.ssh/deploy_key && ssh-keyscan github.com >> ~/.ssh/known_hosts
# add ~/.ssh/deploy_key.pub as a read-only deploy key on your host.
```

### Deploy the agent

```bash
podcd config create --repo-url <local/ssh/url> --revision main   # --host defaults to the hostname
podcd install
systemctl --user daemon-reload && systemctl --user enable --now podcd-agent.service
```

- `podcd config create` writes `~/.config/podcd/agent.yaml` (`--path` elsewhere), with `envFile` pointing at `agent.env` beside it (`--env-file` elsewhere). 
  - A config without `envFile` is refused. Put [secrets](configuration/secrets.md) there, before or after starting the agent.
- `podcd install` writes `~/.config/systemd/user/podcd-agent.service`, which runs `podcd run --config <config>` (`--config` for a config elsewhere).

### `bootstrap.sh` helper script

For a fresh machine that also needs Podman and a dedicated service user. Idempotent.

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/main/deploy/bootstrap.sh | sudo bash -s -- \
  --user podcd \
  --revision main \
  --repo-url git@github.com:you/gitops.git
```

Installs Podman (apt or dnf), creates the `podcd` service user with a subordinate uid range (add `--allow-user-login` if needed), enables lingering, downloads and verifies the latest release, writes the agent config, and starts the service.

### Status

When running as a dedicated service user with no login shell:

```bash
sudo -u podcd bash -lc 'cd && podcd status'
sudo -u podcd bash -lc 'cd && podcd health'
sudo journalctl _UID="$(id -u podcd)" -u podcd-agent --user -f
sudo -u podcd XDG_RUNTIME_DIR="/run/user/$(id -u podcd)" systemctl --user status podcd-agent.service
```

## Container image

```bash
make image                                              # builds podcd:$(VERSION) from Containerfile
podman run --rm -v /repo:/repo:ro,Z ghcr.io/podcd/podcd:latest lint /repo
```

The image runs as non-root UID 1000 and carries `git`, `podman` and `systemctl`; reconciling from inside it needs the host's Podman socket and systemd user bus bind-mounted in.
