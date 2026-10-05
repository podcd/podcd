---
id: model
title: The configuration model
---

Every `.yaml`/`.yml`/`.tpl` file in the repository is read; layout is free. podcd's own kinds use `apiVersion: gitops.podcd.io/v1`:

- `Pod`: a workload definition, referenced by name
- `Network`: how a podman network the pods join should be created
- `Environment`: what applies broadly to a whole environment
- `Group`: a role or machine-purpose definition
- `Host`: a specific VM, including its environment, groups, and overrides

`Environment`, `Group` and `Host` decide **which workloads run on a host** and may override them. A `Network` follows the pods that name it.

## Pod

A workload is a plain Kubernetes `Pod`, played by podman through a Quadlet `.kube` unit. It says what runs, not where:

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

A workload is healthy when its regular containers are running; probes are passed to podman but do not decide podcd's verdict.

`initContainers` run once, in order, before the regular containers. podcd waits while one runs; a non-zero exit makes the workload unhealthy.

Networks are an annotation, written into the unit's `Network=` lines:

```yaml
metadata:
  annotations:
    io.podcd.networks: "edge,monitoring"
```

A name is either a [`Network` document](#network), which podcd creates and removes, or a network already on the host (podman's own `podman`, or one made by hand), which podcd leaves alone.

The user namespace is an annotation too, written into the unit's `UserNS=` line (`podman kube play --userns`):

```yaml
metadata:
  annotations:
    io.podcd.userns: "keep-id"
```

Any `--userns` value; it applies to every container in the pod.

Before anything is written, referenced ConfigMaps and Secrets must exist (or be optional), and host ports must not conflict across the host's workloads.

## Network

A `Network` says how podman creates a network of that name; every field is optional. It exists on a host while a pod there names it in `io.podcd.networks`, and is not selected by `Host`, `Group` or `Environment`. Changing one recreates it and restarts the pods on it. Fields, lifecycle, `networkOverrides` and templating: [Networks](networks.md).

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

Excluding an application that was never selected is an error.

## Groups and environments

A group names what its members run, and may override them:

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

An override is a Kubernetes **strategic merge patch** against the Pod:

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

An environment or group override may name any application the repository defines; it is idle on members that do not run it. An override for an application no Pod defines is an error, and a Host override for an application that host does not run is refused as stale. `networkOverrides` follow the same rules.

To parametrize *within* a shared definition (an image tag per environment, say), use [values templating](values.md).
