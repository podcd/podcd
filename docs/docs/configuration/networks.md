---
id: networks
title: Networks
---

Pods reach each other by name over a shared podman network. A `Network` document declares one; podcd creates it on hosts that need it, recreates it when the document changes, and removes it when nothing on the host uses it.

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

Every field is optional; `spec: {}` is a bridge network with podman's defaults.

```bash
podcd create network backend >> networks.yaml
podcd create network backend --subnet 10.90.0.0/24 --gateway 10.90.0.1 --internal
```

Names are lowercase letters, digits and dashes (it names both the podman network and the unit file).

## Which hosts get it

Not `Host`, `Group` or `Environment`: a host needs the network exactly when a `Pod` it runs names it:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: api
  annotations:
    io.podcd.networks: "backend"
```

A `Network` nobody joins is not created (and not an error). When the last pod on a host leaves it, it is removed in the same reconcile.

A name with no `Network` document refers to a network already on the host (podman's `podman`, or one made by hand), which podcd joins and otherwise leaves alone.

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

which Quadlet resolves to `--network=backend` plus `Requires=`/`After=` on the network's service: the network exists before the pod starts, and stopping it stops every pod on it. `podcd status` shows them:

```text
networks
  NETWORK  UNIT    PODMAN
  backend  active  exists
```

## Lifecycle

The rendered `.network` unit is compared with the one on disk, as for pods:

- no unit for it -> **create**: write the unit, start its service
- the unit differs from what Git renders -> **update**: the network is recreated, see below
- the unit is right but podman has no such network, or the service is not active -> **restart**: run the service again
- a managed network no pod on this host names any more -> **delete**: stop the service, remove the unit, `podman network rm`

Order within a reconcile: application deletes, network deletes, network creates and updates, then application updates, creates and restarts.

### Changing a network

Podman cannot change a network in place. podcd stops the network's service (stopping every pod on it), runs `podman network rm`, starts the service to create it fresh, and restarts every pod on it in the same reconcile:

```text
  ~ update network backend - configuration in Git changed; the network is recreated and every application on it restarted
      - Subnet=10.90.0.0/24
      + Subnet=10.91.0.0/24
  > restart api - network backend is recreated
  > restart worker - network backend is recreated
```

So a `Network` change is a brief outage for everything on it, on every host that has it.

### Adoption

When a `Network` document first appears for a name the host already has, podcd writes the unit and `--ignore` keeps the existing network: adopted, not replaced, so containers podcd did not start keep their network. The next change to the document recreates it with the declared settings.

### Removal

Removal comes after every pod on it is gone and is never forced: if an unmanaged container is still attached, podman's `network is being used` fails the reconcile until you detach it. `podcd remove --all`, `prune` and `teardown` remove managed networks after the applications; `podcd remove NAME` removes applications only.

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

Named fields are set, others kept; `options` merges per key; lists such as `dns` are replaced whole. Stale-entry rules are the same as for applications.

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

A `podcd-*.network` file without podcd's header is somebody else's; if Git declares that name, the reconcile refuses with `unit ... exists but is not managed by podcd`. A podman network with neither a podcd unit nor podcd's label is never removed or recreated, only joined.

A podcd network whose unit was deleted by hand is still recognised by its `io.podcd.network` label; the next reconcile rewrites the unit, or removes the network if Git no longer needs it.
