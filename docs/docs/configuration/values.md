---
id: values
title: Values templating
---

You can utilize `Hosts`/`Groups`/`Environments` to do overrides, an alternative way to do overrides is by using templating and values.
Each `Hosts`/`Groups`/`Environments` can specify values files it should include.

`Host`, `Group` and `Environment` cannot be templated (they decide the values), nor can `SecretStore` be templated.

## Templates

A `*.tpl` file (e.g. `edge-api.yaml.tpl`) is rendered for each host against that host's values.

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
      env:
        - name: LOG_LEVEL
          value: '{{ default "info" .Values.logLevel }}'
{{- if .Values.publicHostname }}
        - name: PUBLIC_HOSTNAME
          value: '{{ .Values.publicHostname }}'
{{- end }}
      resources:
        limits:
          memory: '{{ default "256Mi" .Values.resources.memory }}'
```

A template may render `Pod`, `ConfigMap`, `Secret`, `ExternalSecret` or `Network`.

A YAML `#` comment in a `.tpl` is still template text, use `{{/* ... */}}` to prevent rendering.

## Where values come from

`Host`, `Group` and `Environment` documents list values files. Precedence follows [overrides](model.md#overrides-inheritance-and-merge-rules): environment, then groups, then host.

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

A values file is plain YAML with no `apiVersion`/`kind`, so the loader does not treat it as a document:

```yaml
# values/prod.yaml
image:
  repository: registry.example.com/edge-api
  tag: "1.4.0"
resources:
  memory: 512M
```

`agent.yaml` can also list values files as the lowest-precedence layer. This is not recommended, as this hides configurations from IaC/Git.

```yaml
repository:
  name: gitops
  url: https://github.com/your-user/podcd-gitops.git
  revision: main
  values:
    - values/common.yaml
```

## Functions

Go's own `text/template`, plus a small helper set:

| Function | Use |
|---|---|
| `default DEF VAL` | `VAL` if set, else `DEF`. For anything genuinely optional. |
| `required MSG VAL` | `VAL` if set, else fails the render with `MSG`. Per host: a host that never selects the application never renders its template, so only hosts that actually need the value have to supply it. |
| `upper`, `lower`, `trim` | String transforms. |
| `trimPrefix P S`, `trimSuffix SUF S` | Strip a fixed prefix/suffix from `S`. |
| `replace OLD NEW S` | `strings.ReplaceAll`. |
| `quote V` | Go-quote a value, for embedding it as a JSON/YAML string literal. |

## Secrets and values

Values may carry secret *names*: a template can emit an `ExternalSecret` or reference one by name:

```yaml
# apps/api.yaml.tpl
apiVersion: v1
kind: Pod
metadata:
  name: api
spec:
  containers:
    - name: api
      image: "{{ .Values.image }}"
      envFrom:
        - secretRef:
            name: "{{ .Values.secretName }}"   # values: secretName: db-creds
```

See [Secrets](secrets.md).
