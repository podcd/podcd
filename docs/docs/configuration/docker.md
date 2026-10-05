---
id: docker
title: Docker runtime
---

A host without podman can run the same repository on Docker. Nothing in Git changes; set it per host in `agent.yaml`:

```yaml
runtime: docker
```

## What the host needs

- Docker Engine, reachable by the agent's user through the `docker` CLI: add it to the `docker` group (`sudo usermod -aG docker $USER`, then log in again), or set `DOCKER_HOST` in `agent.env` for a rootless or remote daemon.
- The Compose plugin, v2 or later (`docker compose version`). Compose v1, the python `docker-compose`, cannot wait for an init container to finish and is refused.
- Access to `registry.k8s.io/pause:3.10` (directly or via a registry mirror) for the infra container.

## How a Pod runs

Each `Pod` becomes a Compose project `podcd-<app>` shaped like a podman pod:

- An **infra** container (`<app>-infra`, the pause image) owns the network namespace, the published `hostPort`s and the pod's networks. It carries the pod's name as a network alias, so other pods reach it by name over a [managed network](networks.md) exactly as on podman.
- Every container joins it (`network_mode: service:infra`): containers of one pod talk over `localhost`, and the pod has one address per network.
- Init containers run first, in order, as services the workload `depends_on` having completed successfully. A failing init container fails the apply, with its output in the error.
- The pod's `restartPolicy` becomes Compose's `restart:`; Docker brings the containers back after a daemon restart or a reboot on its own.
- `hostNetwork: true` puts the infra container, and so the pod, on the host's network; ports are then not published.
- `livenessProbe` becomes the container's healthcheck (exec as is; httpGet needs `curl` in the image, tcpSocket needs `nc`). `resources.limits` become memory and CPU limits, `securityContext` the user, capabilities and read-only root, `terminationGracePeriodSeconds` the stop timeout.

Containers are named `<app>-<container>`; `docker logs`, `docker compose -p podcd-<app>` and `podcd logs <app>` all work.

## ConfigMaps and Secrets

podcd resolves them itself, after [ExternalSecrets](secrets.md) are fetched:

- Read as environment (`env`, `envFrom`): written into the project's `compose.yaml`, readable by the agent's user only.
- Mounted as volumes: written as files under `<stateDir>/docker/<app>/{configmaps,secrets}/`, one file per key (`items`, `subPath` and `defaultMode` honoured), laid out on every apply.

Differences from podman:

- **File permissions.** Docker runs containers as real host uids, so a mounted file must be readable by the container's own user. The default mode is `0644`, and `<stateDir>` and every directory above it must be traversable by that user: a home directory with `0750` permissions blocks it, `~/.local/state/podcd` usually does not. To keep a secret to its container, set `defaultMode: 0400` on the volume and a matching `runAsUser` on the container.
- **Secrets on disk.** A Secret mounted as a file lives on the host's disk, under the agent user's state directory, for as long as the pod is applied. podman keeps it in a tmpfs.

## Volumes

`hostPath` binds the path. `emptyDir` and `persistentVolumeClaim` become named volumes (`podcd-<app>_<volume>`, or the claim's own name), which removing the application never deletes. `configMap` and `secret` are files, as above. Other volume types are refused.

## What is not supported

These fail the apply with a message naming the field:

- volume types other than `hostPath`, `emptyDir`, `persistentVolumeClaim`, `configMap` and `secret`
- `env[].valueFrom.fieldRef` and `resourceFieldRef`
- `subPath` on an `emptyDir` or `persistentVolumeClaim` mount
- `Network` documents with `disableDNS` or `dns`, which are podman options

These are accepted and ignored:

| Field | Notes |
|---|---|
| `startupProbe`, `readinessProbe` | Docker has one healthcheck per container over the `livenessProbe`. |
| `imagePullSecrets` | Registry credentials come from the daemon's own login (`docker login`). |
| `hostAliases`, `dnsConfig` | No per-container `/etc/hosts` or resolver settings are written. |
| `hostPID`, `hostIPC` | Host namespaces are not shared. |
| `lifecycle` | No postStart or preStop hooks. |
| `resources.requests` | Only limits map to Docker; requests are a scheduler's concern. |
| `securityContext.fsGroup` (pod level) | Mounted files keep the agent user's ownership. |

## What podcd keeps on disk

podcd records what it applied in the same unit format the podman runtime writes, under `<stateDir>/docker/`, so `podcd plan` reads the same on both runtimes. Do not edit these files.

```text
<stateDir>/
  docker/
    podcd-<app>.kube          record of what was applied
    podcd-<network>.network   same, for a Network
    <app>/
      compose.yaml            the project, 0600
      configmaps/<volume>/    mounted ConfigMap keys
      secrets/<volume>/       mounted Secret keys
  kube/<app>.yaml             the played manifest, 0600
```

An application whose containers are gone but whose record remains (`docker compose down` by hand) is restarted from the manifest on the next reconcile; containers left without a record are still podcd's, and pruned when Git no longer declares them.
