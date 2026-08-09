---
title: FAQ
description: Frequently asked questions about what genai-otel-bridge collects, costs, and does under restarts, failover, and misconfiguration.
---

# Frequently Asked Questions

Short answers to common questions. Each answer links to the authoritative page for the full
detail — treat those linked pages as the source of truth.

## What it collects

### Does it ever see prompt or completion text?

No. `genai-otel-bridge` never requests prompt text, completion text, message bodies, or any
inference content from the platform APIs it polls — it collects request counts, latencies,
token counts, cost, error rates, and status codes only. This is enforced by a three-layer
model (per-source strip, a shared `source.Guard` denylist, and release-gate conformance
tests), not by convention. See [Content Governance](./governance.md) and
[Security](./security.md).

### The `prompt` label on Portkey metrics sounds like it could contain prompt text — does it?

No. It is a saved-prompt **ID** (an opaque slug like `pp-my-prompt-abc123`), not the prompt
text itself. See [Portkey](./portkey.md#groups-loop).

### Can I opt in fields that aren't emitted by default?

Some safe operational fields can be added via `settings.extra_record_fields` on the relevant
loop (for example Portkey's `request_id`, `trace_id`, `response_latency`, or LangSmith's
`app_path`, `tags`). Hard-denied content fields — `inputs`, `outputs`, `messages`, `metadata`,
`portkeyHeaders`, `gen_ai.*` — cannot be opted in; the config validator rejects them at load
time. See [Content Governance](./governance.md#opt-in-fields).

## Cost and API load

### How much API traffic does this add against Portkey or LangSmith?

One request per loop per `cadence` tick (default `60s`), plus jittered retries. Each source
config also carries its own `rate_limit` (rps/burst) so the bridge won't exceed what you've
told it the upstream API can take. The `logs_export` and `runs` loops additionally page
through export/query results within their configured `window`, bounded by
`max_pages_per_window`. See [Configuration](./configuration.md#per-loop-knobs).

### Does a horizontally scaled deployment multiply that API load?

No. Only the elected leader replica runs the scheduler and polls upstream APIs; standby
replicas idle hot and do not poll. See [High Availability](./high-availability.md).

## Replicas and failover

### What happens if I run more than one replica?

Only one replica — the leader — collects and emits at a time, using a Kubernetes Lease
(`coordination.k8s.io/leases`). Standby replicas wait and take over within one lease duration
if the leader fails. The default Helm chart deploys two replicas (one active, one standby).
See [High Availability](./high-availability.md).

### Will a failover double-count metrics or duplicate log lines?

No, by design. A monotonic, lease-epoch-fenced checkpoint write means a demoted or overlapping
leader cannot move the watermark backward, and the OTLP encoder is deterministic — a re-emit
after failover produces the same `(series, timestamp, value)` tuple, which Mimir treats as a
no-op and Loki deduplicates as a byte-identical line. Log delivery is technically
at-least-once (an in-flight page emitted-but-not-checkpointed may repeat); metrics are
gap-free within the source retention and Mimir's out-of-order accept window. See
[High Availability](./high-availability.md#failover-behaviour).

### What happens on a plain restart or pod reschedule?

The new (or restarted) leader loads the last saved watermark for each loop from the
checkpoint store and resumes from there — the source API is the replayable buffer, so there's
no WAL to recover. On SIGTERM the leader lets its Lease expire rather than releasing it early,
and its context is cancelled immediately so nothing new is written after the signal. See
[High Availability](./high-availability.md#sigterm-behaviour).

## Backends and deployment targets

### Which checkpoint/coordination backends does it support?

Three: a Kubernetes Lease + ConfigMap (the production default), a local file (dev/single-
replica only — rejected in combination with `coordinator: lease`), and DynamoDB (for the AWS
ECS deployment target, sharing one table for both the lock and the checkpoint). See
[Configuration](./configuration.md#ha) and [High Availability](./high-availability.md#backends).

### Does it work outside Kubernetes?

Yes — the DynamoDB coordinator/checkpoint pair targets ECS deployments, and `coordinator: none`
with `checkpoint: file` supports a fully local, single-replica run for dev. See
[High Availability](./high-availability.md#noop-mode-single-replica-dev).

## Metrics that seem to be missing

### My gauge shows nothing when I use `rate()` — why?

Portkey analytics and groups metrics, and LangSmith session/usage metrics, are emitted as
OTLP gauges, not counters. Use `sum_over_time(...)` to aggregate over a window; `rate()` or
`increase()` on a gauge produces meaningless or negative values. See
[Portkey](./portkey.md#analytics-loop).

### A `quantile` or `token_type` label isn't showing up on a series — why?

Metric labels are default-deny: an empty `governance.allow_label_keys` list doesn't mean "no
extra restriction," it's enforced alongside a per-series label allow-list, and only labels the
composition root has allow-listed (or that you've added under
`governance.allow_label_keys`) can appear at all. See
[Content Governance](./governance.md#layer-2-sourceguard-shared-default-deny).

### A metric I expected for a whole polling window is just absent, not zero — why?

Because a polling or emit gap is treated as an alertable, counted signal rather than a silent
zero. Check `genai_otel_bridge_window_lag_seconds` and `genai_otel_bridge_samples_skipped_total`
by `reason` first. See [Troubleshooting](./troubleshooting.md#stale-watermark-and-window-lag).

### Why is a whole Portkey analytics bucket missing right after it should have appeared?

Buckets are only emitted once they've "settled" (`bucket_end ≤ now − bucket_settle`, default
10 minutes) to avoid emitting a value that later changes — Mimir can't overwrite an
already-emitted `(series, timestamp, value)`. If your workspace's late-arrival lag exceeds the
default, raise `bucket_settle`; the bridge counts
`genai_otel_bridge_bucket_revised_after_settle_total` so you can tune it from the p95 observed
age instead of guessing. See [Portkey](./portkey.md#bucket-settle-and-watermark).

## See also

- [Configuration](./configuration.md) — full config walk-through
- [Telemetry reference](./telemetry.md) — every metric and log the bridge can emit
- [Troubleshooting](./troubleshooting.md) — diagnosing common failure modes
- [Why This Bridge](./comparison.md) — how it compares to a vendor console or in-app tracing
