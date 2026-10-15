---
id: overview
title: Overview
---

![podcd-diagram](../static/img/overview.svg)

## Fetch

- The repository in `agent.yaml` is fetched at its defined `revision` (branch, tag or commit). 
  - If the remote is unreachable but a checkout exists, the agent reconciles from that commit and reports the repository as offline.
- Every manifest is read into one index;
  - You cannot name define a name twice, this is considered an error. The index is compiled for the host the agent is configured for  (default: the hostname). See the [configuration model](configuration/model.md) and [values templating](configuration/values.md).

## Plan & reconcile

Desired and actual are compared per application declared for a `Host`:

- if not on the host -> **create**
- if rendered/played unit differs from disk -> **update**
- if unchanged, but the unit is not active -> **restart**
- if on host but no longer in Git -> **delete** (when `prune` is on, the default; otherwise a no-op)

[Networks](configuration/networks.md) go through the same comparison. Deletes run before creates, so a renamed application frees its host port before its successor binds it.

Reconciles run one at a time under a file lock, so `podcd reconcile` and the agent's loop never fight.
Destructive actions are logged before they happen and the first failure stops the run and recorded.

A failed reconcile is retried after `retryInterval`, doubling up to `maxRetryInterval`; after a success the regular `interval` resumes. `jitter` adds a random delay to `interval` so a fleet does not hit Git in lockstep.

Images no container uses are removed (internal with `podman image prune --all`) every `imagePrune` (off by default; e.g. `24h`) after a successful reconcile.

## State

`~/.local/state/podcd/state.json` records the host identity, last revisions, last success and failure, and per-application history.
`podcd status` reads this state so it works offline, this is metadata only and is safe to delete.

Everything the agent owns is under its user's home:

```text
~/.config/podcd/agent.yaml          agent config
~/.config/podcd/agent.env           secrets
~/.config/containers/systemd/       Quadlet units podcd wrote
~/.local/state/podcd/               checkouts, played manifests, state.json
```

## Metrics

With `metricsEnabled: true`, the agent serves `GET /metrics` on `metricsAddress` (default `127.0.0.1:9464`) in the Prometheus text format, rendered from `state.json` on every scrape:

| Metric | Meaning |
|---|---|
| `podcd_info{version,host}` | Always 1; the agent's version and host. |
| `podcd_last_attempt_timestamp_seconds` | When the last reconcile started, successful or not; 0 if never. |
| `podcd_last_success_timestamp_seconds` | When the last successful reconcile started; 0 if never, so the age alert below fires on a host that never succeeded. |
| `podcd_consecutive_failures` | Reconciles failed since the last success. |
| `podcd_repository_info{repository,target_revision,last_attempt_revision,last_success_revision}` | Always 1; see [Revisions](#revisions). |
| `podcd_repository_behind{repository}` | 1 if the host does not run the commit the last reconcile tried to deploy. |
| `podcd_repository_offline{repository}` | 1 if the last reconcile used the commit on disk because the remote was unreachable. |
| `podcd_application_healthy{application}` | 1 if the application's last health check passed. |

A port that cannot be bound is logged and the agent keeps reconciling without metrics; it binds once, at start, so restart the agent after freeing the port.

The endpoint has no authentication and names the host, the version and every application. Keep it on loopback; bind another address only on a network you trust.

A containerized agent (`examples/quadlet`) has its own loopback: set `metricsAddress: 0.0.0.0:9464` and publish it on the host's with `PublishPort=127.0.0.1:9464:9464`.

To scrape the loopback address from a container, e.g. Grafana Alloy deployed by podcd, give its pod `hostNetwork: true`. That puts the pod in the host's network namespace: it reaches every port on the host and its own ports are opened on the host, so keep it to a scraper you trust.

```alloy
prometheus.scrape "podcd" {
  targets    = [{"__address__" = "127.0.0.1:9464"}]
  forward_to = [prometheus.remote_write.default.receiver] // or otelcol.receiver.prometheus for OTLP
}
```

Useful alerts: `time() - podcd_last_success_timestamp_seconds > 900` (the agent is stuck, failing or gone; keep it above `interval`), `podcd_consecutive_failures > 3`, `podcd_repository_behind == 1` for 30m (a commit that does not land), `podcd_application_healthy == 0`, and `podcd_repository_offline == 1` for longer than an outage you tolerate.

## Revisions

podcd records per repository in `state.json`, and exports with `podcd_repository_info`:

| Term | Meaning |
|---|---|
| `target_revision` | The branch, tag or commit `repository.revision` names in agent.yaml. |
| `last_attempt_revision` | The commit the target revision resolved to on the last reconcile: what podcd tried to deploy (`lastAttempt.revisions`). |
| `last_success_revision` | The commit of the last successful reconcile, applied and healthy: what the host runs (`lastSuccess.revisions`). |

`podcd_repository_behind` is 1 while the two commits differ: a newer commit has not reconciled successfully yet. While the remote is unreachable podcd compares against the commit on disk, so check `podcd_repository_offline` too. It reads 0 when the last reconcile failed before the commit was known, e.g. on an authentication error.
