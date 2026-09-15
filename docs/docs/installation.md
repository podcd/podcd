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

Building from source instead: `make build` puts the binary in `dist/`.

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

This stops and removes every application podcd manages, then stops, disables and removes `podcd-agent.service` - the equivalent of running `systemctl --user disable --now podcd-agent.service` and deleting the unit file by hand. `--purge-state` also deletes `~/.local/state/podcd` (checkouts, played manifests, state.json); `--purge-config` also deletes `~/.config/podcd` (the repository URL, and any secrets in `agent.env`) - both are opt-in and skipped without them.

Building from source instead: `make build`, then the same bootstrap with `--binaries ./dist`.

## Production deploy

### 1. Set up the GitOps repository

:::tip
`podcd validate` requires Secrets as references (`env:NAME`, `file:path`). See [Secrets](configuration/secrets.md).
:::

Set up a repository defining at least a `Host` and an `Application` or `Pod`. See [`examples/`](https://github.com/podcd/podcd/tree/main/examples) in the podcd repository, or [podcd/podcd-gitops](https://github.com/podcd/podcd-gitops). The files are the **desired state**: podcd reads the repository, resolves the configuration for a host, validates it, and reconciles the result with the workloads running on that host. There is no prescribed directory structure - organize the Git repository however you like. The [configuration model](configuration/model.md) describes the documents.

- `podcd init` scaffolds a basic podcd-gitops repository.
- `podcd create` generates a document per kind from flags.
- `podcd lint` checks it.

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

#### Private repositories

Give the agent a read credential in `agent.yaml`. A GitHub or GitLab deploy token over HTTPS:

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

### 2. Deploy podcd

podcd needs these files on the host:

```text
~/.config/podcd/agent.yaml                    agent config
~/.config/podcd/agent.env                     secrets, 0600, never in Git
~/.config/systemd/user/podcd-agent.service    the agent's own unit (podcd install)
```

In essence the following needs to happen:

1. Download the `podcd` binary.
2. `podcd install`: generates a systemd service for the user.
3. `podcd config create --host <host> --repo-url <repo> --revision <target_revision>`
4. `systemctl --user daemon-reload && systemctl --user enable --now podcd-agent.service`

`bootstrap.sh` is no more than a helper utility for exactly that.

#### 2.1 Automated setup

The same bootstrap as the demo, with a dedicated service user, your repository and a pinned release. One command per host; it is idempotent.

```bash
curl -fsSL https://raw.githubusercontent.com/podcd/podcd/main/deploy/bootstrap.sh | sudo bash -s -- \
  --release-version 2.3.0 \
  --user podcd \
  --revision main \
  --host hostname \  # ideally you leave the host out and use machine names in your gitops repo
  --repo-url git@github.com:you/gitops.git
```

It detects the distribution (apt on Debian/Ubuntu, dnf on the RHEL family), installs Podman, creates the `podcd` service user (no login shell; add `--allow-user-login` only if you want one), grants it a subordinate uid range, enables lingering so applications come back after a reboot, downloads the release you named and verifies its checksum, writes the agent config, installs the user service and starts it.

On SELinux-enforcing hosts (the RHEL default) bind mounts need the `Z` (or `z`) option so the container may read them - `volumes: [{source: /srv/data, destination: /data, options: Z}]`; ConfigMap and Secret volumes on Pods are labelled by podman itself.

Everything the agent owns is under its user's home, `/home/podcd`:

```text
~/.config/podcd/agent.yaml          agent config
~/.config/podcd/agent.env           secrets, 0600, never in Git
~/.config/containers/systemd/       the Quadlet units podcd wrote
~/.local/state/podcd/               checkouts, played manifests, state.json
```

#### 2.2 Manual setup

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

### 3. Secrets

Anything an application needs that must not be in Git - a database password, a deploy token - is a *reference* in Git and a value on the host. See [Secrets](configuration/secrets.md) for `env:`, `file:` and `vault:` references and how to provision them.

### 4. Status

The service user by default has no login shell, so run commands through `sudo -u`, from its home (rootless Podman refuses to run from a directory it cannot read):

```bash
sudo -u podcd bash -lc 'cd && podcd status'
sudo -u podcd bash -lc 'cd && podcd health'
sudo journalctl _UID="$(id -u podcd)" -u podcd-agent --user -f
# systemctl --user for another user needs its runtime dir spelled out:
sudo -u podcd XDG_RUNTIME_DIR="/run/user/$(id -u podcd)" systemctl --user status podcd-agent.service
```

`status` shows the last successful and failed reconcile and, per application, the unit state, the image and the last health result. `health` exits non-zero if anything is unhealthy, which makes it a usable check for your monitoring; add `-o json` to the commands for a scraper.

## Container image

```bash
make image                                              # builds podcd:$(VERSION) from Containerfile
podman run --rm -v /repo:/repo:ro,Z ghcr.io/podcd/podcd:latest lint /repo
```

The image runs as a fixed non-root UID (1000); rootless Podman remaps that into your subordinate uid/gid range the same way it does for any other container, nothing special to configure there. On SELinux-enforcing hosts (RHEL, Fedora), add `:z` (shared) or `:Z` (private) to any bind mount as above, or the mount is denied - Debian/Ubuntu/Alpine don't need this since they don't enforce SELinux.

The image carries `git`, `podman` and `systemctl`, so every podcd command runs in it, including `run`/`reconcile` - but those manage *the host's* rootless Podman and systemd `--user` session, so from inside a container they need the host's Podman socket and systemd user bus bind-mounted in.
