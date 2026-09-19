---
id: model
title: The configuration model
---

podcd fetches documents from a Git repository.

There is no prescribed directory structure; every `.yaml`/`.yml`/`.tpl` file in the tree is read and rendered.

```yaml
apiVersion: gitops.podcd.io/v1
```

The core document types are:

- `Pod`: a workload definition, referenced by name
- `Network`: how a podman network the pods join should be created
- `Environment`: what applies broadly to a whole environment
- `Group`: a role or machine-purpose definition
- `Host`: a specific VM, including its environment, groups, and overrides

`Pod` defines a workload. `Environment`, `Group` and `Host` decide **which workloads run on a host** and may override their configuration. A `Network` is neither: it follows the pods that name it.

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

`podcd` considers a workload healthy when its regular containers are running; it does not actively interpret container healthchecks. You can still define probes that `podman` supports, `podcd` will still consider running pods healthy.

`initContainers` are supported as well. the underlying `podman` runs them once in declaration order before regular containers. A completed init container with exit code zero is expected to be exited; `podcd` waits while an init container is still running and treats a non-zero exit as an unhealthy workload.

Podman has no field for networks, podcd instead takes them as an annotation and writes them into the unit's `Network=` lines:

```yaml
metadata:
  annotations:
    io.podcd.networks: "edge,monitoring"
```

A name here is either a network a [`Network` document](#network) declares, which podcd then creates and removes, or one that already exists on the host - podman's own `podman`, or one made by hand - which podcd leaves alone.

Pods are validated before anything is written:

- referenced ConfigMaps and Secrets must exist, or be marked optional
- host port conflicts across the workloads one host runs

## Network

A `Network` says how podman should create a network of that name. Every field is optional; an empty spec is a plain bridge network with podman's defaults. The fields are the ones `podman network create` takes:

```yaml
apiVersion: gitops.podcd.io/v1
kind: Network
metadata:
  name: backend
spec:
  driver: bridge          # bridge (default), macvlan, ipvlan
  subnet: 10.90.0.0/24
  gateway: 10.90.0.1
  ipRange: 10.90.0.128/25
  internal: false         # no route out of the host
  ipv6: false
  disableDNS: false
  dns: [10.90.0.53]
  options: {mtu: "1400"}  # driver options, --opt key=value
```

A `Network` is not selected by a `Host`, `Group` or `Environment`. It exists on a host when a pod that host runs names it in `io.podcd.networks`, and is removed once no pod there does. 

Pods on it resolve each other by pod name. Declaring one that nothing joins will simply creates nothing.

`podcd` writes a Quadlet `.network` unit for it, and a pod's own unit refers to the network through that unit, so systemd creates the network before the pod and stops the pod when the network is stopped.

Podman cannot change an existing network in place, so **changing a `Network` recreates it, and every pod on this host that joins it is restarted** in the same reconcile.

A network of that name that already exists on the host when the document first appears is adopted as it is and will not be replaced: `podcd` will not tear a network out from under containers it does not manage. From then on it is podcd's, and a later change to the document recreates it. The full lifecycle - adoption, recreation, removal, ordering - is on the [Networks](networks.md) page. 

A `Network` resource may be a template, so a subnet that differs per host is one document rendered with each host's values. And like a `Pod`, it can be overridden per layer, under `networkOverrides` rather than `overrides` so a network and an application may share a name:

```yaml
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: vm-1
spec:
  groups: [web]
  networkOverrides:
    backend:
      subnet: 10.91.0.0/24
      options: {mtu: "1500"}
```

A network override is a plain merge into the spec: a field it names is set, one it does not name is kept, `options` gains and replaces keys, and a list such as `dns` is replaced whole. Precedence and the rules about stale entries are the ones below.

`podcd create network NAME [--subnet CIDR] [--gateway IP] [--internal] [--dns IP]... [--opt KEY=VALUE]...` prints one, validated.

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

An override is a **strategic merge patch** against the Pod, it follows Kubernetes' own merge rules:

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

An environment or group override may name any application the repository defines, whether or not every member runs it: the override applies wherever the application does and is simply idle elsewhere.

An override that no Pod defines is treated as an error. A Host's own overrides are held to a stricter rule, an override for one it does not run is a stale entry and is refused. `networkOverrides` follow the same two rules, against the Network documents and the networks the host's applications join.

For parametrizing *within* a shared definition - an image tag that differs between dev and prod, say - see [values templating](values.md).
