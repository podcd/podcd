---
id: installation
title: Installation
---

## Install the binary

```bash
V=2.3.0; A=amd64   # or arm64
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz"
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz.sha256"
sha256sum -c "podcd_${V}_linux_${A}.tar.gz.sha256" && tar -xzf "podcd_${V}_linux_${A}.tar.gz"
sudo install -m 0755 podcd /usr/local/bin/podcd
```

You can alternatively build from source instead.

## Demo deploy

`bootstrap.sh` will attempt to install Podman, enable lingering so the user's services survive logout, download a release and verify its checksum, point the agent at the example repository (whose `local` Host runs one nginx), and start the agent as a systemd user service.

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

This stops and removes every application podcd manages, then stops, disables and removes `podcd-agent.service`.

## Production deploy

### 1. Set up the GitOps repository

:::tip
`podcd validate` requires Secrets as references (`env:NAME`, `file:path`). See [Secrets](configuration/secrets.md).
:::

Set up a repository with at least a `Host` and an `Application` or `Pod`. See [`examples/`](https://github.com/podcd/podcd/tree/main/examples) or [podcd/podcd-gitops](https://github.com/podcd/podcd-gitops). No prescribed directory structure. See the [configuration model](configuration/model.md).

- `podcd init` scaffolds a minimal repository.
- `podcd create` generates documents.
- `podcd lint` checks them.

```bash
podcd lint                          # current directory
podcd lint --host prod-web-01 ./gitops
podcd get all --repo ./gitops       # inspect what a checkout defines
```

#### Private repositories

Add a read credential in `agent.yaml`. HTTPS token:

```yaml
repositories:
  - name: gitops
    url: https://gitlab.com/your-user/gitops.git
    revision: main
    auth:
      username: gitlab+deploy-token-42   # GitLab tokens have usernames; GitHub PATs can omit this
      token: env:GITOPS_TOKEN
```

SSH deploy key:

```yaml
repositories:
  - name: gitops
    url: git@github.com:your-user/gitops.git
    auth:
      sshKeyPath: ~/.ssh/deploy_key
      sshKnownHostsPath: ~/.ssh/known_hosts
```

```bash
ssh-keygen -t ed25519 -N "" -f ~/.ssh/deploy_key && ssh-keyscan github.com >> ~/.ssh/known_hosts
# add ~/.ssh/deploy_key.pub as a read-only deploy key on your host.
```

### 2. Deploy podcd

podcd needs these files on the host:

```text
~/.config/podcd/agent.yaml                    agent config
~/.config/podcd/agent.env                     secrets, 0600, never in Git
~/.config/systemd/user/podcd-agent.service    the agent's own unit (podcd install)
```

- `podcd config create` writes `~/.config/podcd/agent.yaml`.
- `podcd install` writes `~/.config/systemd/user/podcd-agent.service`. - Add secrets to `~/.config/podcd/agent.env` before or after starting the agent.

The following will setup the agent config, provision and enable the service.

```bash
podcd config create --host <this_is_optional> --repo-url <local/ssh/url> --revision main
podcd install
systemctl --user daemon-reload && systemctl --user enable --now podcd-agent.service
```

For a fresh machine that also needs Podman installed and a dedicated service user, `bootstrap.sh` handles all of that. The script is idempotent.

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/main/deploy/bootstrap.sh | sudo bash -s -- \
  --release-version 2.3.0 \
  --user podcd \
  --revision main \
  --repo-url git@github.com:you/gitops.git
```

Installs Podman (apt or dnf), creates the `podcd` service user with a subordinate uid range (add `--allow-user-login` if needed), enables lingering, downloads and verifies the release, writes the agent config, and starts the service.

On SELinux-enforcing hosts (RHEL default) bind mounts need the `Z` option: `volumes: [{source: /srv/data, destination: /data, options: Z}]`.

Files the agent owns:

```text
~/.config/podcd/agent.yaml          agent config
~/.config/podcd/agent.env           secrets, 0600, never in Git
~/.config/containers/systemd/       Quadlet units
~/.local/state/podcd/               checkouts, state.json
```

### 3. Secrets

Anything that must not be in Git is a *reference* in Git and a value on the host. See [Secrets](configuration/secrets.md) for `env:`, `file:` and `vault:` references.

### 4. Status

When running as a dedicated service user with no login shell:

```bash
sudo -u podcd bash -lc 'cd && podcd status'
sudo -u podcd bash -lc 'cd && podcd health'
sudo journalctl _UID="$(id -u podcd)" -u podcd-agent --user -f
sudo -u podcd XDG_RUNTIME_DIR="/run/user/$(id -u podcd)" systemctl --user status podcd-agent.service
```

`status` shows the last reconcile and per-application unit state. `health` exits non-zero if anything is unhealthy; add `-o json` for a scraper.

## Container image

```bash
make image                                              # builds podcd:$(VERSION) from Containerfile
podman run --rm -v /repo:/repo:ro,Z ghcr.io/podcd/podcd:latest lint /repo
```

The image carries `git`, `podman` and `systemctl`, so every podcd command runs in it, including `run`/`reconcile` - but those manage *the host's* rootless Podman and systemd `--user` session, so from inside a container they need the host's Podman socket and systemd user bus bind-mounted in.
