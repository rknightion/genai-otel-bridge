---
title: High Availability
description: Leader election, single-emit safety, checkpoint fencing, and failover behaviour in genai-otel-bridge.
---

# High Availability

genai-otel-bridge is designed to run as a **highly available, leader-elected** service. Multiple
replicas can be deployed, but only the elected leader runs the collection and emission scheduler
at any time. Standby replicas wait and are ready to take over within one lease duration.

The default Helm chart deploys two replicas (one active, one standby). Raise to three for a
second standby — a `PodDisruptionBudget` serialises node drains at three replicas.

---

## Leader election

Leader election uses a **Kubernetes Lease** (`coordination.k8s.io/leases`). The leader holds
the Lease and renews it on a configurable interval; standby replicas watch and attempt to
acquire it when it expires.

The Lease **reduces overlap but is not the write fence** on its own. Single-emit safety comes
from three cooperating mechanisms:

1. **`leaderCtx` cancellation** — when a replica loses the Lease, its leader context is
   cancelled, aborting in-flight collect and emit work.
2. **Monotonic checkpoint fence** — the `CheckMonotonic` function rejects any watermark write
   where `incoming.Time ≤ stored.Time` or `incoming.Epoch < stored.Epoch`. A demoted leader
   cannot move the frontier backward or double-advance it.
3. **Lease epoch** — the current `LeaseTransitions` count is threaded into the leader context
   as an epoch integer. Every checkpoint write carries the epoch; a stale-epoch write is
   rejected by `CheckMonotonic`.

```text
new leader elected (epoch N+1)
    │
    ▼
leaderCtx carries epoch N+1
    │
    ▼
Collect → Emit → CheckMonotonic (epoch N+1 > stored epoch N → accepted)
                                (epoch N+1 == stored epoch N+1, time advances → accepted)
                                (old leader writes epoch N → rejected)
```

### Async-elected barrier

client-go runs `OnStartedLeading` in a goroutine that `Run()` does not join. To prevent a
re-election race, `elected.Store(true)` is set inside the callback and `leadDone` is closed
when the callback exits. After `Run()` returns, the coordinator waits on `leadDone` before
allowing re-election — ensuring the old leader's drain completes before a new leader starts.

### SIGTERM behaviour

On SIGTERM, the leader **does not release the Lease** — it lets the Lease expire. This ensures
the leader can finish persisting watermarks before the standby takes over. The standby must
wait a full `LeaseDuration` before acquiring the Lease, which gives the draining leader time
to write its final checkpoints. The Helm chart sets `terminationGracePeriod: 300s` to cover
the emit-retry budget.

---

## Checkpointing

The checkpointer stores each loop's watermark durably so the leader (or a new leader after
failover) knows where to resume collection.

### Backends

**ConfigMap backend (default, Kubernetes)** — a single Kubernetes ConfigMap holds all
watermarks as JSON values, one key per loop. Writes use optimistic concurrency
(resource-version checks); on a conflict (409) the backend re-reads and retries up to five
times. If a concurrent writer's newer watermark trips `CheckMonotonic`, the write is
rejected (never overwritten with a stale value). Data keys are sanitized and hash-suffixed
for stability.

```yaml
ha:
  coordinator: lease
  checkpoint: configmap
```

**File backend (dev/test only)** — a local YAML file. Uses atomic temp-then-rename writes.
**Not suitable for production HA** — it is per-pod and not shared across replicas. Config
validation rejects `file` + `coordinator: lease` in combination.

```yaml
ha:
  coordinator: none
  checkpoint: file
```

### Checkpoint key

Each loop has a unique `CheckpointKey` that includes:

- `SourceInstance` — the `id:` from the source config (e.g. `portkey-prod-eu`)
- `Loop` — the loop name (e.g. `analytics`, `runs`)
- `OutputFingerprint` — a hash of the emitted series set and naming config

The fingerprint means that adding a new graph or changing the `metric_prefix` creates a new
checkpoint key, so the new series bootstraps its own history instead of inheriting the
existing loop's already-current watermark.

---

## Failover behaviour

When the leader fails or loses the Lease:

1. `leaderCtx` is cancelled, aborting in-flight collect and emit.
2. The watermark is **not advanced** for any work that was not checkpointed.
3. A standby acquires the Lease (within one `LeaseDuration`).
4. The new leader loads the last saved watermark for each loop and resumes collection from
   that point.

**For metrics (analytics/groups):** collection is gap-free within the source retention and
the Mimir accept window. The new leader re-derives the same settled buckets from the source
API, producing byte-identical OTLP output. Mimir accepts re-emission of the same
`(series, timestamp, value)` tuple as a no-op.

**For logs (logs_export, runs):** delivery is **at-least-once**. An in-flight page that was
emitted but not checkpointed may be re-emitted by the new leader. Loki deduplicates
byte-identical log records. A mid-window leader change restarts the window from the last
checkpointed page.

---

## RBAC

The Helm chart creates a **namespace-scoped Role** (not ClusterRole) with:

- `coordination.k8s.io/leases`: `get`, `create`, `update` on `genai-otel-bridge-leader`
- `configmaps`: `get`, `create`, `update` on `genai-otel-bridge-checkpoints`

The pod cannot read or modify its own `genai-otel-bridge-config` ConfigMap. `delete` is not
granted to the running pod; it is granted only to the post-delete cleanup hook's ephemeral
ServiceAccount.

---

## Noop mode (single replica / dev)

Set `coordinator: none` for a single-replica or dev deployment. The no-op coordinator always
leads with epoch 1 and never contacts the Kubernetes API. Pair with `checkpoint: file`
for fully local operation.

---

## Monitoring HA health

The self-obs dashboard and the `GenaiOtelBridgeNoStandby` and `GenaiOtelBridgeLeaderAbsent`
alerts provide HA visibility:

- `genai_otel_bridge_last_success_timestamp_seconds` — absent for 15m → `GenaiOtelBridgeLeaderAbsent` fires
- Replica count below 2 → `GenaiOtelBridgeNoStandby` fires (warning)

See [Alerts & Runbooks](./alerts.md) for runbooks and [Dashboards](./dashboards.md) for the
self-obs dashboard.

---

## See also

- [Installation](./installation.md) — Helm chart HA options
- [Configuration](./configuration.md) — `ha:` config block
- [Alerts & Runbooks](./alerts.md) — leader-absent and no-standby runbooks
- [Troubleshooting](./troubleshooting.md) — common HA failure modes
