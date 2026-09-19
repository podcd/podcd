---
id: agent
title: The agent configuration
---

`podcd` requires two things:

- **`agent.yaml`**, on the host, which will tell `podcd` which host this machine is, which Git repository to reconcile, and where the agent keeps its files. It is meant to be fire-and-forget: set up once per host, then left alone.
- **IaC in Git** (local repos also supported) - the [configuration model](model.md). What should run, on which hosts, with which configuration. Everything that is part of the fleet's declared intent lives here, where it is reviewed and versioned.

## agent.yaml

`podcd config create` writes this file with every field documented and its default shown commented out; the reference below is that output.
It lives at `~/.config/podcd/agent.yaml` (or `$PODCD_CONFIG`, or `/etc/podcd/agent.yaml`, looked up in that order).

- You can alternatively point `podcd config create --path <>` to generate it elsewhere.
- `podcd run --config <>` needs to be pointed to this path if you decide to put the agent-config elsewhere.

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

# Container runtime: podman (rootless, via Quadlet) or docker (via Compose).
# runtime: podman

# The Git repository this host reconciles against.
repository:
  # A short name for this repository; it appears in logs and status.
  # name: infrastructure
  # Where to fetch from: https://, ssh (git@host:path) or a local path.
  url: git@github.com:podcd/podcd-gitops.git
  # A branch, a tag or a commit. A tag or commit pins the host.
  revision: main
  # Read only this subdirectory of the repository.
  # path: ""
  # Values file(s), relative to this repository's tree (path, if set), for
  # {{ .Values }} templating of *.tpl files - a host-local fallback beneath
  # what Host, Group and Environment documents declare. Repeated files
  # merge, later ones winning per key.
  # values: []
  # Disable host key / TLS verification. Visible here on purpose, rather
  # than an environment variable nobody sees.
  # insecure: false
  # A private repository needs a read credential. The token is a secret
  # reference (env: or file:), never a literal in this file; it is resolved
  # on every fetch. GitLab deploy tokens have their own username.
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
# Re-read on every lookup so rotation needs no restart.
# envFile: /home/podcd/.config/podcd/agent.env

# Remove applications that Git no longer declares. On by default; leaving
# orphans running is its own kind of drift.
# prune: true

# text or json.
# logFormat: text
```

## Editing it

`podcd config set` edits the file in place. Fields are addressed by their yaml path:

```bash
podcd config set revision v1.4.0                 # alias for repository.revision
podcd config set interval 30s
podcd config set repository.auth.token env:GITOPS_TOKEN
podcd config set repository.values.0 values/common.yaml   # a list grows one item at a time
# fields that only make sense together are set together and validated once:
podcd config set vault.address=https://vault.example.com vault.roleId=env:VAULT_ROLE_ID vault.secretId=env:VAULT_SECRET_ID
podcd config view
```

