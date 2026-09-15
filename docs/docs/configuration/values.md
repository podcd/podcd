---
id: values
title: Values templating
---

[Overrides](model.md#overrides-inheritance-and-merge-rules) parametrize one application at a time, by name, and only work for structural differences: which apps run, which port, which interface - an override can only name an application every host in that layer actually runs.

Values templating parametrizes the documents themselves, so several hosts can share one `Application`/`Pod` definition and each fill in the parts that differ - an image tag, a resource limit, a domain - including for an application that only exists on some of those hosts, which an override cannot express.

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

A template may render any deployable kind - `Application`, `Pod`, `ConfigMap`, `Secret` - but not a `Host`, `Group` or `Environment`, since those are what decide a host's values in the first place. A rendered document is validated exactly like a written one: unknown fields, a plaintext `Secret`, a name already taken by a plain file, all fail the same way.

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

Merging is the same rule overrides use: maps merge key by key, recursively; anything else - a string, a number, a list - is replaced wholesale by the later file.

Beneath all of that sits one more, lowest-precedence layer: `agent.yaml`'s own `repositories[].values`, resolved on the host rather than declared in Git. 

Keep it for values that are genuinely host-local (or as a stopgap); prefer a `Host`/`Group`/`Environment`'s own `values:` for anything that is part of the fleet's declared intent - `agent.yaml` is meant to stay a fire-and-forget pointer (which host, which repositories).

```yaml
repositories:
  - name: gitops
    url: https://github.com/your-user/podcd-gitops.git
    revision: main
    values:
      - values/common.yaml
```

## Functions

Templates get Go's own `text/template`, plus this small helper set - deliberately not sprig, so podcd stays a dependency-light single binary and a missing value is meant to be visible, not smoothed over:

| Function | Use |
|---|---|
| `default DEF VAL` | `VAL` if set, else `DEF`. For anything genuinely optional. |
| `required MSG VAL` | `VAL` if set, else fails the render with `MSG`. Per host: a host that never selects the application never renders its template, so only hosts that actually need the value have to supply it. |
| `upper`, `lower`, `trim` | The obvious string transforms. |
| `trimPrefix P S`, `trimSuffix SUF S` | Strip a fixed prefix/suffix from `S`. |
| `replace OLD NEW S` | `strings.ReplaceAll`. |
| `quote V` | Go-quote a value, for embedding it as a JSON/YAML string literal. |

A missing value that is only printed - `{{ .Values.nope }}` - renders as the literal text `<no value>`, which is valid YAML and will not fail by itself. Use `required` for anything the document cannot do without, and `default` for anything optional.

## Secrets and values

A value is not a secret. A values file is an ordinary file in Git, so a literal password in one is a password in Git. Put a *reference* there instead, and consume it through `secretEnv:` - which resolves `env:`/`file:`/`vault:` references on the host - rather than `env:`, which copies text verbatim:

```yaml
spec:
  secretEnv:
    DB_PASSWORD: '{{ .Values.dbPasswordRef }}'   # values: dbPasswordRef: vault:prod/api/DB_PASSWORD@secret
```

See [Secrets](secrets.md).

## Trying it out

`podcd lint --values values/common.yaml path/to/repo` renders and compiles every host a repository defines, without touching one. `--values` stands in for `agent.yaml`'s fallback layer; a `Host`/`Group`/`Environment`'s own `values:` apply exactly as they would for a real agent.

See [`podcd-gitops/multi-env`](https://github.com/podcd/podcd-gitops/tree/main/multi-env) for a complete example: four hosts across two zones and two environments, using overrides for the structural zone differences and Environment/Group `values:` for the per-environment image tag and sizing that overrides can't reach.
