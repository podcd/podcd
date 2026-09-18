---
id: model
title: The configuration model
---

podcd reads documents from a Git repository. There is no prescribed directory structure; every `.yaml`/`.yml` file in the tree is read, and every document in them is decoded strictly against its real type - a misspelled field is an error, not something silently ignored. podcd's own kinds share one API version:

```yaml
apiVersion: gitops.podcd.io/v1
```

The core document types are:

- `Pod`: a workload definition, referenced by name
- `Environment`: what applies broadly to a whole environment
- `Group`: a role or machine-purpose definition
- `Host`: a specific VM, including its environment, groups, and overrides

`Pod` defines a workload. `Environment`, `Group` and `Host` decide **which workloads run on a host** and may override their configuration.

## Pod

A workload is a plain Kubernetes `Pod`, played by podman through a Quadlet
`.kube` unit. It says what a workload *is*, not where it runs:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: local
spec:
  restartPolicy: Always
  terminationGracePeriodSeconds: 30
  containers:
    - name: nginx
      image: docker.io/library/nginx@sha256:72ba65eb42c10344912a84ff42408db7d34f2feb642204570ab8fc5ffd29f1d3
      ports:
        - containerPort: 80
          hostPort: 8080
          hostIP: 127.0.0.1
      resources:
        limits:
          memory: 256Mi
      livenessProbe:
        httpGet: { path: /, port: 80 }
```

podcd considers a workload healthy when its regular containers are running; it does not interpret container healthchecks. `readinessProbe` and `startupProbe` are ignored by podman, so a check written as either does nothing.

`initContainers` are supported as well. Podman runs them once, in declaration order, before regular containers. A completed init container with exit code zero is expected to be exited; podcd waits while an init container is still running and treats a non-zero exit as an unhealthy workload.

Podman has no field for networks, so podcd takes them as an annotation and
turns them into the unit's `Network=` lines:

```yaml
metadata:
  annotations:
    io.podcd.networks: "edge,monitoring"
```

Pods are validated before anything is written:

- referenced ConfigMaps and Secrets must exist, or be marked optional
- host port conflicts across the workloads one host runs

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
      spec:
        containers:
          - name: nginx
            ports:
              - containerPort: 80
                hostPort: 9090
```

An `Environment` has exactly the same shape. Both may also name [values files](values.md).

## Overrides, inheritance and merge rules

Precedence runs from lowest to highest:

```text
Pod -> Environment override -> Group overrides -> Host override
```

Groups are applied in the order listed by the host, so later entries win:

```yaml
groups:
  - base
  - web
```

`web` overrides `base`, and the host overrides both.

An override is a **strategic merge patch** against the Pod, so it follows Kubernetes' own merge rules: containers merge by `name`, ports by `containerPort`, environment variables by `name`, and a plain list is replaced wholesale. Changing one variable leaves the others alone:

```yaml
# pod
containers:
  - name: api
    env:
      - {name: LOG_LEVEL, value: info}
      - {name: PORT, value: "8080"}
```

```yaml
# host override
spec:
  containers:
    - name: api
      env:
        - {name: LOG_LEVEL, value: debug}
```

```yaml
# result
containers:
  - name: api
    env:
      - {name: LOG_LEVEL, value: debug}
      - {name: PORT, value: "8080"}
```

An override may only name an application that every host in that layer actually runs; an environment-level override for an application only some of its hosts have is an error for the hosts that don't. For parametrizing *within* a shared definition - an image tag that differs between dev and prod, say - see [values templating](values.md).
