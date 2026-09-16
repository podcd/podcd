---
id: secrets
title: Secrets
---

## How it works

podcd manages secrets itself — it does not use `podman secret`. A value is never in Git and never in `state.json`. References are resolved by the agent on the host at reconcile time; `podcd plan` shows a hash of the resolved values, not the values themselves.

How a resolved secret reaches the container depends on the workload kind:

**`Application` (container):** resolved values are written to a 0600 env file at `~/.local/state/podcd/env/<app>.env`. The generated Quadlet unit references it with `EnvironmentFile=`. The container receives the secrets as **environment variables**. There is no file-mount path for `Application` secrets.

**`Pod` (kube manifest):** the entire Kubernetes manifest - including `Secret` resources with their `stringData` resolved to plaintext - is written to `~/.local/state/podcd/kube/<app>.yaml` (0600) and played by `podman kube play`. Kubernetes `Secret` volume mounts work as podman implements them, so secrets **can be mounted as files** inside pod containers.

A secret in Git should be a *reference* to a value. References are resolved on the host, at reconcile time. `podcd plan` shows them as a hash; logs and `state.json` never carry them.

A reference is `scheme:locator`. Three schemes exist:

| Scheme | Resolves from |
|---|---|
| `env:NAME` | the agent's environment - in practice `~/.config/podcd/agent.env`, which systemd loads for the agent and which the agent re-reads on every lookup |
| `file:path` | a file under `secretsDir` (or an absolute path); for values delivered by another tool |
| `vault:...` | HashiCorp Vault KV, with the `vault:` section in `agent.yaml` |

## Where references go

On an `Application`, under `secretEnv:` - **not** `env:`, which copies text verbatim:

```yaml
spec:
  secretEnv:
    DATABASE_PASSWORD: env:DATABASE_PASSWORD
    API_TOKEN: vault:prod/api/API_TOKEN@secret
```

In a `Secret` document, under `stringData:`. `data:` (base64 of plaintext) is rejected outright, and every `stringData` entry must look like a reference:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: api-secrets
stringData:
  DATABASE_PASSWORD: env:DATABASE_PASSWORD
```

## `env:` - the agent.env file

A secret referenced as `env:DATABASE_PASSWORD` is looked up in the agent's environment, which systemd loads from `~podcd/.config/podcd/agent.env`:

```bash
sudo -u podcd bash -lc 'umask 077 && printf "DATABASE_PASSWORD=%s\n" "$(cat /path/to/secret)" >> ~/.config/podcd/agent.env'
```

The agent re-reads that file on every lookup, so no restart is needed: rotating a value changes the application's spec hash and the next reconcile restarts the application.

## `file:` - a directory of secrets

`file:` references read files under `secretsDir` instead (set it in `agent.yaml`); use that for values delivered by another tool - a secrets agent writing one file per secret, say.

## `vault:` - HashiCorp Vault

With a `vault:` section in `agent.yaml`, references resolve against Vault (KV v1 or v2), authenticated with AppRole or a token. Both of these name the same value - the key is always the last segment:

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

## Repository credentials

A private repository's read credential is setup as a reference as well - `auth.token: env:GITOPS_TOKEN` in `agent.yaml`. See [private repositories](../installation.md#private-repositories).
