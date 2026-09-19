---
id: overview
title: Overview
---



![podcd-diagram](../static/img/overview.svg)


## Overview

#### Fetch

- Every repository in `agent.yaml` is fetched at its `revision` (a branch, a tag or a commit). If a remote is unreachable and a checkout already exists on disk, the agent reconciles from the commit it has and reports the repository as offline.

- The checkout is read into one index. A name defined twice will be treated as an error. The index is then compiled for *this* host - the `Host` document matching `agent.yaml`'s `host` (default: the machine's hostname). See the [configuration model](configuration/model.md) and [values templating](configuration/values.md).

- The runtime (podman, or docker) is inspected: which units podcd wrote, their content hashes, whether systemd reports them active, which container image is actually running.

#### Plan & Reconcile

Desired and actual are compared per application:

- not present on the host -> **create**
- the rendered unit, or the manifest it plays, differs from what is on disk -> **update**
- present and unchanged, but the unit is not active -> **restart**
- present in the runtime but no longer declared in Git -> **delete** (only when `prune` is on, the default; otherwise reported as a no-op)

Networks a `Network` document declares go through the same comparison, as `.network` units. A changed network is recreated and the applications on it restarted; one no pod on the host names any more is deleted.

Deletes are ordered before creates, so a renamed application frees its host port before its successor binds it; networks sit in between, after the applications that leave them and before the ones that join them.

Reconciles run one at a time under a file lock, so a human running `podcd reconcile` and the agent's own loop cannot fight over the same unit files. A destructive action is logged before it happens. The first failure stops the run and is recorded.

A failed reconcile is retried after `retryInterval`, doubling on each further failure up to `maxRetryInterval`, then the regular `interval` resumes once a reconcile succeeds. `jitter` adds a random delay on top of `interval` so a fleet does not hit Git in lockstep.

#### State and history

The agent stores local state in `~/.local/state/podcd/state.json`. It records the agent identity, last revisions, last successful and failed reconciliation, and per-application history including the previous deployment. `podcd status` reads it, so status works offline.

That state is metadata only and is safe to delete. The agent can reconstruct its state from Git and the host.

Everything else the agent owns is under its running user's home:

```text
~/.config/podcd/agent.yaml          agent config
~/.config/podcd/agent.env           secrets
~/.config/containers/systemd/       the Quadlet units podcd wrote
~/.local/state/podcd/               checkouts, played manifests, state.json
```
