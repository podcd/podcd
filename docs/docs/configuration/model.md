---
id: model
title: The configuration model
---

podcd reads documents from one or more Git repositories. There is no prescribed directory structure; every `.yaml`/`.yml` file in the tree is read, and every document in them is decoded strictly against its real type - a misspelled field is an error, not something silently ignored. podcd's own kinds share one API version:

```yaml
apiVersion: gitops.podcd.io/v1
```

The core document types are:

- `Application`: a reusable application definition, referenced by name
- `Environment`: what applies broadly to a whole environment
- `Group`: a role or machine-purpose definition
- `Host`: a specific VM, including its environment, groups, and overrides

`Application` and `Pod` define workloads. `Environment`, `Group` and `Host` decide **which workloads run on a host** and may override their configuration.

## Application

An `Application` says what a workload *is*, not where it runs:

```yaml
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: local
spec:
  image: docker.io/library/nginx@sha256:72ba65eb42c10344912a84ff42408db7d34f2feb642204570ab8fc5ffd29f1d3
  ports:
    - host: 8080
      container: 80
      hostIP: 127.0.0.1
  restartPolicy: always
  healthcheck:
    http:
      port: 8080
      path: /
```

Ports and volumes take either the object form above or the shorthand podman uses: `"127.0.0.1:8080:80"`, `"/srv/data:/data:Z"`.

## Host

A `Host` selects what should run on one machine, directly and/or through an environment and groups:

```yaml
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: local
spec:
  environment: local
  groups:
    - local
  applications:
    - my-api
```

The final set is the union of everything selected by the environment, the groups and the host itself. A host can remove something a layer above selected:

```yaml
spec:
  groups:
    - web
  excludeApplications:
    - node-exporter
```

If the application was not selected in the first place, podcd reports an error rather than silently ignoring the entry.

## Groups and environments

For larger repositories, applications are selected indirectly. A group names what its members run, and may override them:

```yaml
apiVersion: gitops.podcd.io/v1
kind: Group
metadata:
  name: web
spec:
  applications:
    - nginx
    - node-exporter
  overrides:
    nginx:
      ports:
        - host: 9090
          container: 80
```

An `Environment` has exactly the same shape. Both may also name [values files](values.md).

## Pod manifests

A host's application list can also name a plain Kubernetes `Pod`, played by podman through a Quadlet `.kube` unit. Pods are validated by the same rules as applications:

- referenced ConfigMaps and Secrets must exist, or be marked optional
- host port conflicts
- the first readiness or liveness probe on a published port becomes the health check

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: metrics
spec:
  containers:
    - name: collector
      image: docker.io/library/nginx@sha256:72ba65eb42c10344912a84ff42408db7d34f2feb642204570ab8fc5ffd29f1d3
      envFrom:
        - configMapRef: { name: metrics-config }
      ports:
        - containerPort: 9200
          hostPort: 9200
      readinessProbe:
        httpGet: { path: /ready, port: 9200 }
```

`Secret`s in Git hold references, not plaintext values - see [Secrets](secrets.md). Values are resolved on the host, written to a 0600 file outside the unit directory, and displayed as a hash in `podcd plan`.

Overrides for a Pod use strategic merge semantics. Containers merge by name and ports merge by `containerPort`, which matches the expectation of a Pod author:

```yaml
overrides:
  metrics:
    spec:
      containers:
        - name: shipper
          args: ["--target", "http://127.0.0.1:9200", "--verbose"]
```

## Overrides, inheritance and merge rules

Precedence runs from lowest to highest:

```text
Application -> Environment override -> Group overrides -> Host override
```

Groups are applied in the order listed by the host, so later entries win:

```yaml
groups:
  - base
  - web
```

`web` overrides `base`, and the host overrides both. An override merges fields rather than replacing the application:

- scalars: a non-empty value in the higher layer wins
- maps (`env`, `secretEnv`, `labels`): merged key by key, higher layer wins per key
- lists (`ports`, `volumes`, `networks`, `command`, `entrypoint`): replaced wholesale, never appended - appending to a port list has no sane meaning and hides what is actually running

```yaml
# application
env:
  LOG_LEVEL: info
  PORT: "8080"
```

```yaml
# host override
env:
  LOG_LEVEL: debug
```

```yaml
# result
env:
  LOG_LEVEL: debug
  PORT: "8080"
```

An override may only name an application that every host in that layer actually runs; an environment-level override for an application only some of its hosts have is an error for the hosts that don't. For parametrizing *within* a shared definition - an image tag that differs between dev and prod, say - see [values templating](values.md).
