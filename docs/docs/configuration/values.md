---
id: values
title: Values templating
---

Overrides parametrize one application at a time, by name, and only work when every host in the layer runs that application. Values templating parametrizes the documents themselves — several hosts can share one `Application`/`Pod` and each fill in the parts that differ: an image tag, a resource limit, a domain.

## Templates

A template is a file named `*.tpl` (`edge-api.yaml.tpl`, say). A template is rendered for each host against that host's values, and only then read as a document.

```yaml
# apps/edge-api.yaml.tpl
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: edge-api
spec:
  image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
  env:
    LOG_LEVEL: '{{ default "info" .Values.logLevel }}'
{{- if .Values.publicHostname }}
    PUBLIC_HOSTNAME: '{{ .Values.publicHostname }}'
{{- end }}
  resources:
    memory: '{{ default "256M" .Values.resources.memory }}'
```

Because rendering happens before anything reads the result, a template gets the whole of Go's [`text/template`](https://pkg.go.dev/text/template): conditionals around entire keys or containers, `range`, even a `metadata.name` computed from a value.

A template may render any deployable kind - `Application`, `Pod`, `ConfigMap`, `Secret` - but not a `Host`, `Group` or `Environment`, since those are what decide a host's values in the first place.

One consequence of the whole file being a template: inside a `.tpl`, a YAML `#` comment is still template text, so a comment that quotes template syntax literally (a bare `{{ if }}`) fails to parse. Write it as a Go template comment, `{{/* like this */}}`, which renders to nothing.

## Where values come from

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

A values file is plain YAML with no `apiVersion`/`kind`, relative to its own document's repository, so the loader ignores it as a document - it exists only to be read as `.Values`:

```yaml
# values/prod.yaml
image:
  repository: registry.example.com/edge-api
  tag: "1.4.0"
resources:
  memory: 512M
```

You can also pass value paths from the hosts `agent.yaml` is meant to stay a fire-and-forget pointer (which host, which repositories).

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

A missing value that is only printed - `{{ .Values.nope }}` - renders as the literal text `<no value>`, which is valid YAML and will not fail by itself. Use `required` for anything the resource cannot do without, and `default` for anything optional.

## Secrets and values

For secrets, you can utilize *references* instead, and consume it through `secretEnv:` - which resolves `env:`/`file:`/`vault:` references on the host.
You can also set values to refer to *references* in Vault.

```yaml
spec:
  secretEnv:
    DB_PASSWORD: '{{ .Values.dbPasswordRef }}'   # values: dbPasswordRef: vault:prod/api/DB_PASSWORD@secret
```

See [Secrets](secrets.md).
