---
id: secrets
title: Secrets
---

## How it works

Secrets should not be in Git as plaintext. Instead, you declare **where** a secret comes from, and the agent fetches and resolves it on the host at reconcile time before building the workload manifest.

The mechanism is a pair of documents in your gitops repository:

| Document | What it does |
|---|---|
| `SecretStore` | Configures a backend: the agent's environment, a files directory, or HashiCorp Vault |
| `ExternalSecret` | Names one or more keys to fetch from a store and assemble into a `v1/Secret` |

At each reconcile the agent runs a **provision phase** before compiling workloads: it reads every `ExternalSecret`, fetches the named keys from the corresponding `SecretStore`, and produces a resolved `v1/Secret` with the actual values. That secret is then bundled into the pod's kube manifest (alongside its ConfigMaps), so `podman kube play` sees a complete, playable document. Change a value in agent.env, and the next reconcile detects the manifest difference and restarts the pod.

The resolved values are written to `~/.local/state/podcd/secrets/<name>.yaml` (0600) between runs so a restart does not force a Vault round-trip before the first reconcile.

---

## `env` store - secrets from agent.env

The `env` store reads named variables from the agent's environment and from `~/.config/podcd/agent.env`, which is loaded by the systemd unit.

```yaml
# In Git: declare the store and what to fetch
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: local
spec:
  provider:
    env: {}   # no configuration needed

---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: db-creds
spec:
  secretStoreRef:
    name: local
  target:
    name: db-creds        # the Secret name the Pod will reference
  data:
    - secretKey: PASSWORD
      remoteRef:
        key: DB_PASSWORD  # the environment variable name
    - secretKey: USERNAME
      remoteRef:
        key: DB_USER
```

On the host, add the values to `~/.config/podcd/agent.env` (0600, never in Git):

```bash
sudo -u podcd bash -lc 'umask 077 && cat >> ~/.config/podcd/agent.env' <<'EOF'
DB_PASSWORD=supersecret
DB_USER=app
EOF
```

The agent re-reads the file on every reconcile, so rotating a value needs no restart.

Reference the secret from a Pod or Application:

```yaml
# envFrom injects every key as an environment variable
spec:
  containers:
    - name: api
      envFrom:
        - secretRef:
            name: db-creds

# or select individual keys
spec:
  containers:
    - name: api
      env:
        - name: DB_PASSWORD
          valueFrom:
            secretKeyRef:
              name: db-creds
              key: PASSWORD
```

---

## `file` store - secrets delivered by another tool

The `file` store reads one file per key from a directory. Use it when a secrets agent (like `agent` from HashiCorp, or a sidecar) writes individual files.

```yaml
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: secrets-dir
spec:
  provider:
    file:
      dir: /run/secrets/myapp   # absolute, or relative to secretsDir in agent.yaml

---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: db-creds
spec:
  secretStoreRef:
    name: secrets-dir
  target:
    name: db-creds
  data:
    - secretKey: PASSWORD
      remoteRef:
        key: db_password        # filename under dir
```

`dataFrom` fetches every file in a directory at once:

```yaml
  dataFrom:
    - extract:
        key: /run/secrets/myapp   # directory; every file becomes one key
```

---

## `vault` store - HashiCorp Vault KV

Vault configuration moves into a `SecretStore` document in Git. The `vault:` section that used to live in `agent.yaml` is gone; Vault credentials stay in `agent.env` on the host.

```yaml
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: vault
spec:
  provider:
    vault:
      server: https://vault.example.com
      kvVersion: 2          # 1 or 2; default 2
      # namespace: team-a  # Vault Enterprise only
      # caCert: /etc/pki/vault-ca.pem
      auth:
        appRole:
          roleId: env:VAULT_ROLE_ID        # resolved from agent.env
          secretRef:
            name: env:VAULT_SECRET_ID      # resolved from agent.env
        # tokenSecretRef:
        #   name: env:VAULT_TOKEN          # alternative to AppRole
```

Add the credentials to `agent.env` (0600, never in Git):

```bash
sudo -u podcd bash -lc 'umask 077 && cat >> ~/.config/podcd/agent.env' <<'EOF'
VAULT_ROLE_ID=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
VAULT_SECRET_ID=yyyyyyyy-yyyy-yyyy-yyyy-yyyyyyyyyyyy
EOF
```

Fetch individual keys - `remoteRef.key` is the full KV path including mount, `property` selects the field within that secret:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: db-creds
spec:
  secretStoreRef:
    name: vault
  target:
    name: db-creds
  data:
    - secretKey: PASSWORD
      remoteRef:
        key: secret/prod/db     # mount/path
        property: password      # field within the KV secret
```

`dataFrom` extracts every field from one KV path:

```yaml
  dataFrom:
    - extract:
        key: secret/prod/db     # all fields become Secret keys
```

The agent logs in when first needed, re-logs in on token expiry or rejection, and caches reads so a reconcile with many keys from one path costs one round trip.

---

## Mounting secrets as files

Because all workloads use the kube manifest path, you can mount a secret as a file using standard Kubernetes volume syntax:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: api
spec:
  volumes:
    - name: db-secret-vol
      secret:
        secretName: db-creds
  containers:
    - name: api
      image: example.com/api@sha256:...
      volumeMounts:
        - name: db-secret-vol
          mountPath: /run/secrets/db
          readOnly: true
```

---

## Git-defined Secrets

A plain `v1/Secret` document can live in Git, but its `stringData` values are **not** resolved at reconcile time - they go into the manifest as literal strings. These documents exist for non-sensitive data that happens to be typed as a Kubernetes Secret (for example, a public TLS certificate chain). They are not a mechanism for injecting env-var or file-backed secrets into containers. Use `ExternalSecret` for that.

The `data:` field (base64-encoded values) is rejected at load time because base64 is encoding, not encryption.

---

## Repository credentials

A private repository's read credential is a `env:` or `file:` reference in `agent.yaml`, resolved on every fetch:

```yaml
repositories:
  - name: gitops
    url: https://github.com/you/gitops.git
    revision: main
    auth:
      token: env:GITOPS_TOKEN   # in agent.env, not a literal
```

See [installation](../installation.md#private-repositories).
