---
id: model
title: The configuration model
---

Every `.yaml`/`.yml`/`.tpl` file in the repository is read; there is no prescribed directory structure. 

`podcd`'s Kinds use `apiVersion: gitops.podcd.io/v1`:

- `Pod`: a workload definition, referenced by name
- `Network`: a podman network
- `Environment`: what applies broadly to a whole environment
- `Group`: a role or machine-purpose definition
- `Host`: a specific VM, including its environment, groups, and overrides

`Environment`, `Group` and `Host` decide **which workloads run on a host** and may override them. Pods maybe assigned to a `Network`.

## Pod

Pods in `podcd` is a plain Kubernetes `Pod`, played by podman through a Quadlet `.kube` unit:

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

A workload is healthy when its containers are running.
`initContainers` run before the containers.

Networks are an annotation, written into the unit's `Network=` lines:

```yaml
metadata:
  annotations:
    io.podcd.networks: "edge,monitoring"
```

You can assign workloads to a [`Network`](#network), which podcd will create and/or remove.
You can assign workloads to a network already on the host (podman's own `podman`, or one made by hand).

The user namespace can also be passed via annotations, internally written into the unit's `UserNS=` line (`podman kube play --userns`):

```yaml
metadata:
  annotations:
    io.podcd.userns: "keep-id"
```

A `--userns` value applies to every container in the pod.

Referenced ConfigMaps and Secrets must exist (or marked optional), and host ports must not conflict across the host's workloads.

## Network

A `Network` defines how podman creates a network of that name. It will be kept on a host for as long as a pod names it via `io.podcd.networks`. It is not selected by `Host`, `Group` or `Environment`.

Changing the network recreates it and will restart the pods on the network.
Fields, lifecycle, `networkOverrides` and templating: [Networks](networks.md).

## Host

A `Host` an agent identifies as determines what should run on a machine:

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

The final set is the union of everything selected by the environment, the groups and the host itself. A host can remove something a layer above prescribed:

```yaml
spec:
  groups:
    - web
  excludeApplications:
    - node-exporter
```

Excluding an application that was never selected will produce an error.

## Groups and environments

A group can specify what Hosts under it run, and may override them as well:

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

An `Environment` works similarly. 

Both `Group` and `Environment` can list [values files](values.md).

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

An override is a **strategic merge patch** against the Pod:

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

To parametrize *within* a shared definition (an image tag per environment, say), use [values templating](values.md).
