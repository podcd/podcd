---
id: networks
title: Networks
---

Pods that should reach each other by name need a podman network to share. A `Network` document declares one, and podcd creates it on every host that needs it, changes it when the document changes, and removes it when nothing on the host uses it any more. Nothing is created by hand, and nothing is left behind.

## Declaring one

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
  internal: false         # true: no route out of the host
  ipv6: false
  disableDNS: false
  dns: [10.90.0.53]
  options: {mtu: "1400"}  # driver options, --opt key=value
```

Every field is optional. `spec: {}` is a plain bridge network with podman's defaults, which is what most repositories want:

```bash
podcd create network backend >> networks.yaml
podcd create network backend --subnet 10.90.0.0/24 --gateway 10.90.0.1 --internal
```

The name must be lowercase letters, digits and dashes: it becomes both the podman network's name and a unit file's.

## Which hosts get it

A `Network` is not selected by a `Host`, `Group` or `Environment`, and has no `applications:` list of its own. A host needs the network exactly when a `Pod` that host runs names it:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: api
  annotations:
    io.podcd.networks: "backend"
```

Two consequences follow. A `Network` that no pod on a host names is not created there, and declaring one nobody joins is not an error. And when the last pod on a host that named it leaves - removed from Git, or moved off the network - the network is removed from that host in the same reconcile.

A name in `io.podcd.networks` that no `Network` document defines keeps its old meaning: a network that already exists on the host, podman's own `podman` or one made by hand, which podcd joins and otherwise leaves alone.

## What podcd writes

For a network the host needs, podcd writes a Quadlet `.network` unit next to the pods' `.kube` units:

```text
~/.config/containers/systemd/podcd-backend.network
```

```ini
# Managed by podcd - do not edit. Change Git instead.
# podcd-network: backend
# podcd-spec-hash: 3f2a...
# podcd-renderer: 1

[Unit]
Description=podcd network backend

[Network]
NetworkName=backend
Label=io.podcd.managed=true
Label=io.podcd.network=backend
Subnet=10.90.0.0/24
Gateway=10.90.0.1

[Install]
WantedBy=default.target
```

Quadlet turns that into `podcd-backend-network.service`, a oneshot that runs `podman network create --ignore ... backend` and stays active. The pod's own unit refers to the network through its unit file rather than by name:

```ini
[Kube]
Yaml=/home/podcd/.local/state/podcd/kube/api.yaml
Network=podcd-backend.network
```

which Quadlet resolves to `--network=backend` on the `kube play` command and, more importantly, to `Requires=` and `After=` on the network's service, systemd creates the network before it starts the pod, and stopping the network's service stops every pod on it.

`podcd status` lists what it found:

```text
networks
  NETWORK  UNIT    PODMAN
  backend  active  exists
```

## Lifecycle

podcd compares the rendered `.network` unit with the one on disk, the same way it compares pods, and plans one of:

- no unit for it -> **create**: write the unit, start its service
- the unit differs from what Git renders -> **update**: the network is recreated, see below
- the unit is right but podman has no such network, or the service is not active -> **restart**: run the service again
- a managed network no pod on this host names any more -> **delete**: stop the service, remove the unit, `podman network rm`

Within one reconcile the order is: application deletes, network deletes, network creates and updates, application updates, creates and restarts. A pod always leaves a network before the network is removed, and a network always exists before a pod joins it.

### Changing a network

Podman cannot change an existing network, and `podman network create --ignore` keeps whatever is there. So podcd does what the Quadlet documentation asks a human to do: stop the network's service, which stops every pod that requires it, `podman network rm` it, and start the service again so it is created fresh. Every pod on this host that joins the network is then restarted, in the same reconcile, even though its own unit did not change. The plan says so:

```text
  ~ update network backend - configuration in Git changed; the network is recreated and every application on it restarted
      - Subnet=10.90.0.0/24
      + Subnet=10.91.0.0/24
  > restart api - network backend is recreated
  > restart worker - network backend is recreated
```

A change to a `Network` document is therefore a brief outage for everything on it on every host that has it. Plan for that the way you would for a pod's image change.

### Adoption

When a `Network` document first appears for a name the host already has - typically a network created by hand before podcd managed networks, or by an earlier `bootstrap` script - podcd writes the unit and starts the service, and `--ignore` leaves the existing network as it is. It is adopted, not replaced: podcd will not tear a network out from under containers it did not start. From then on it is podcd's, and the next change to the document recreates it with the declared settings. Adoption is the only time the network on the host may differ from the document.

### Removal

A network is removed only after every pod on it is gone, and `podman network rm` is never forced. If something podcd does not manage is still attached, the removal fails with podman's own error (`network is being used`) and the reconcile is retried; detach or remove that container by hand. `podcd remove --all`, `podcd prune` and `podcd teardown` remove managed networks after the applications, and say so before doing it. `podcd remove NAME` removes applications only.

## Overrides and templates

Like a pod, a network can differ per environment, group or host. `networkOverrides` on an `Environment`, `Group` or `Host` merges into the spec, in the same precedence as application overrides and under its own key so a network and an application may share a name:

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

The merge is plain: a field the override names is set, one it does not name is kept, `options` gains and replaces keys, and a list such as `dns` is replaced whole. An override for a network no document defines is an error; a host's override for a network none of its pods join is a stale entry and is refused, exactly as for applications.

A `Network` may also be a `*.tpl` template rendered with each host's [values](values.md), for the case where the whole spec is per host:

```yaml
# networks/backend.yaml.tpl
apiVersion: gitops.podcd.io/v1
kind: Network
metadata:
  name: backend
spec:
  subnet: '{{ required "values: backendSubnet" .Values.backendSubnet }}'
```

## What podcd will not touch

A `podcd-*.network` file in the unit directory without podcd's header is somebody else's. If Git declares a `Network` of that name, the reconcile refuses with `unit ... exists but is not managed by podcd` rather than guess; remove the file or restore the header. A podman network with neither a podcd unit nor podcd's label is never listed, removed or recreated, whatever Git says - it can only be joined.

A network podcd created whose unit file was deleted by hand is still recognised by its `io.podcd.network` label, reported as missing its unit, and gets the unit written again on the next reconcile if Git still needs it, or removed if not.
