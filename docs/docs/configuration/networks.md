---
id: networks
title: Networks
---

## Pod Networks

Pods reach each other by name over a shared podman network. A `Network` document declares a podman network.
`podcd` creates it on hosts that has applications requiring it, and recreates it when there are changes. Cleanup occurs when nothing on the host uses it.


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

Every field is optional; `spec: {}` is a bridge network with podman's defaults.

```bash
podcd create network backend >> networks.yaml
podcd create network backend --subnet 10.90.0.0/24 --gateway 10.90.0.1 --internal
```

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: api
  annotations:
    io.podcd.networks: "backend"
```

A `Network` nobody joins will not be created. When the last pod on a host leaves it, it is removed in the same reconcile.

A definition with no `Network` document is taken as a reference to a network already on the host (e.g. podman's `podman`, or one made by hand), which podcd joins and otherwise leaves alone.

## What podcd writes

A Quadlet `.network` unit next to the pods' `.kube` units:

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

Quadlet turns it into `podcd-backend-network.service`, a oneshot running `podman network create --ignore ... backend`. The pod's unit refers to the network by unit file:

```ini
[Kube]
Yaml=/home/podcd/.local/state/podcd/kube/api.yaml
Network=podcd-backend.network
```

which Quadlet resolves to `--network=backend` plus `Requires=`/`After=` on the network's service.
`podcd status` to show:
```text
networks
  NETWORK  UNIT    PODMAN
  backend  active  exists
```

## Lifecycle

The rendered `.network` unit is compared with the one on disk, as for pods:

- no unit for it -> **create**: write the unit, start its service
- the unit differs from what Git renders -> **update**: the network is recreated
- the unit is right but podman has no such network, or the service is not active -> **restart**: run the service again
- a managed network no pod on this host names any more -> **delete**: stop the service, remove the unit, `podman network rm`

Order within a reconcile: application deletes, network deletes, network creates and updates, then application updates, creates and restarts.

### Changing a network

A `Network` change will cause a brief outage for everything on it.

Podman cannot change a network in place, and `podcd` has to stop the network's service (stopping every pod on it).
It runs `podman network rm`, starts the service to create it fresh, and restarts every pod on it in the same reconcile:

```text
  ~ update network backend - configuration in Git changed; the network is recreated and every application on it restarted
      - Subnet=10.90.0.0/24
      + Subnet=10.91.0.0/24
  > restart api - network backend is recreated
  > restart worker - network backend is recreated
```

### Adoption

When a `Network` document first appears for a name the host already has, podcd writes the unit and `--ignore` keeps the existing network, i.e. a stray Network will then be adopted. The next change to the document recreates it with the declared settings. Until then, removing the document removes only the unit: podcd deletes only networks carrying its `io.podcd.network` label.

### Removal

Removal comes after every pod on it is gone: if an unmanaged container is still attached, podman's `network is being used` fails the reconcile until you detach it. `podcd remove --all`, `prune` and `teardown` remove managed networks after the applications; `podcd remove NAME` removes applications only.

## Overrides and templates

`networkOverrides` on an `Environment`, `Group` or `Host` merges into the spec, with the same precedence as application overrides (a separate key, so a network and an application may share a name):

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

A `Network` may also be a `*.tpl` [template](values.md):

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

A `podcd-*.network` file without podcd's header is will not be managed, a reconcile will refuse with `unit ... exists but is not managed by podcd`.
A podman network with neither a podcd unit nor podcd's label is never removed or recreated.
A podcd network whose unit was deleted by hand is still recognised by its `io.podcd.network` label; the next reconcile rewrites the unit, or removes the network if Git no longer defines it.
