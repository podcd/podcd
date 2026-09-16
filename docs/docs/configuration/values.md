---
id: values
title: Values templating
---

Overrides parametrize one application at a time, by name, and only work when every host in the layer runs that application. Values templating parametrizes the documents themselves - several hosts can share one `Pod` and each fill in the parts that differ.

## Templates

A template is a file named `*.tpl` (e.g. `edge-api.yaml.tpl`). A template is rendered for each host against that host's values.

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

A template may render any deployable kind - `Pod`, `ConfigMap`, `Secret` - but not a `Host`, `Group` or `Environment`, since those are what decide a host's values in the first place.

One consequence of the whole file being a template: inside a `.tpl`, a YAML `#` comment is still template text, so a comment that quotes template syntax literally (a bare `{{ if }}`) fails to parse. Write it as a Go template comment, `{{/* like this */}}`, which renders to nothing.

## Where values come from

A `Host`, `Group` or `Environment` document can provide a list of values files. The precedence is in bottom to top order.

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

A values file is plain YAML with no `apiVersion`/`kind`, relative to its own document's repository, so the loader ignores it as a document - it exists only to be read as `.Values`:

```yaml
# values/prod.yaml
image:
  repository: registry.example.com/edge-api
  tag: "1.4.0"
resources:
  memory: 512M
```

You can also pass value paths from the hosts `agent.yaml`.
The agent config however is meant to stay a fire-and-forget pointer, values directly on the agent is not a recommended pattern.

```yaml
repositories:
  - name: gitops
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
| `upper`, `lower`, `trim` | The obvious string transforms. |
| `trimPrefix P S`, `trimSuffix SUF S` | Strip a fixed prefix/suffix from `S`. |
| `replace OLD NEW S` | `strings.ReplaceAll`. |
| `quote V` | Go-quote a value, for embedding it as a JSON/YAML string literal. |

Use `required` for anything the resource cannot do without, and `default` for anything optional.

## Secrets and values

Secret names are safe to use in values - a template can emit an `ExternalSecret` or reference one by name in a Pod spec:

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

The actual secret values never enter the template - only the name does. See [Secrets](secrets.md).
