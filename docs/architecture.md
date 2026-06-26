---
title: Architecture
description: Condensed user-facing architecture overview of genai-otel-bridge — data flow, seams, and key design decisions.
---

# Architecture

This page is a condensed user-facing overview. For the full design — component contracts,
the failure-handling taxonomy (F1–F47), the adversarial review dispositions, and the
decision ledger — see the repo docs:

- [`ARCHITECTURE.md`](https://github.com/rknightion/genai-otel-bridge/blob/main/ARCHITECTURE.md) —
  durable seams, data model, component interfaces, decision ledger (§16)
- [`docs/DESIGN.md`](https://github.com/rknightion/genai-otel-bridge/blob/main/docs/DESIGN.md) —
  build spec, F1–F47 failure handling, review dispositions

---

## What it is

genai-otel-bridge is a **generic, vendor-neutral** service that polls AI-platform APIs
(LLM gateways, eval platforms) and emits **operational telemetry** — metrics and logs — to
any OTLP endpoint. It is content-free by design: it never requests or emits prompt/response
bodies.

It exists because operationally important signals for LLM applications — gateway cost,
tokens, latency, errors, and eval scores — live inside managed products (Portkey, LangSmith)
and are reachable only through their APIs. genai-otel-bridge does that pulling well, once, and
turns it into clean Grafana-native telemetry.

---

## Data flow

```
AI-platform APIs          genai-otel-bridge (N replicas, one leader active)
┌───────────────┐         ┌──────────────────────────────────────────────┐
│ LLM gateway   │◄─GET────┤ Source.Loop.Collect(watermark)               │
│ (Portkey)     │         │   → model.Batch (Samples + LogRecords)       │
├───────────────┤         │   → source.Guard.Sanitize (label allow-list, │
│ Eval platform │◄─GET────┤     content denylist)                        │
│ (LangSmith)   │         │   → schedule.LoopRunner (bounded queue)      │
└───────────────┘         │   → emit.Emitter (OTLP/HTTP, retry)          │
                          │   → on success: advance Watermark            │
                          │     (Checkpointer — ConfigMap or file)       │
                          └──────────────────┬───────────────────────────┘
                                             │ OTLP/HTTP
                                             ▼
                                   Grafana Cloud OTLP gateway
                                   (metrics → Mimir, logs → Loki)
```

Three properties encoded in this picture:

1. **One replica emits at a time.** The Coordinator (Kubernetes Lease) elects a single leader;
   only the leader runs the Scheduler. Standbys wait and take over on leader failure.
2. **Collection is decoupled from emission** by a bounded per-loop queue. The queue absorbs
   transient sink slowness; under sustained slowness it backpressures collection — visible as
   rising `window_lag`, never silent loss.
3. **The watermark only advances on successful emit.** This is the basis of conditional
   gap-free / no-duplicate behaviour. It is engineered (emit-once-after-settle +
   deterministic byte-identical encoding + monotonic epoch-fenced checkpoint writes), not assumed.

---

## The vendor-neutral model

All vendor-specific code lives inside a source package (`internal/source/<vendor>/`) behind
the common `source.Loop` interface. The interface crosses a single boundary:

```
Source.Loop.Collect(ctx, watermark) → model.Batch
```

`model.Batch` contains `[]model.Sample` (for metrics) and `[]model.LogRecord` (for logs).
These types are **FROZEN** — adding or renaming fields is a design change requiring an
`ARCHITECTURE.md` update. No vendor or domain knowledge crosses into the core scheduler,
emitter, or checkpointer.

---

## Internal packages

| Package | Role |
|---------|------|
| `model/` | FROZEN vendor-neutral types: `Sample`, `LogRecord`, `Batch`, `Watermark`, `CheckpointKey` |
| `source/` | `Source`/`Loop` interface + registry + cardinality `Guard` |
| `source/portkey/` | Portkey analytics, groups, and logs_export loops |
| `source/langsmith/` | LangSmith sessions and runs loops |
| `emit/otlp/` | Hand-rolled deterministic OTLP protobuf encoder + retry |
| `schedule/` | Per-loop tick → collect → enqueue → emit driver; watermark state machine |
| `checkpoint/` | Durable watermark store + monotonic/epoch write fence |
| `coordinate/` | Leader election; single-active-replica |
| `httpx/` | Hardened outbound client (SSRF egress guard, cross-host redirect block) |
| `config/` | YAML config model, secret substitution, validation |
| `selfobs/` | The bridge's own metrics + health endpoints |
| `app/` | Composition root (wiring only) |

---

## Key design decisions

**Pull-by-window, forward-only.** Each loop pulls a bounded time window forward from a
durable watermark. The source API is the replayable buffer — restarts and failovers recover
without a WAL, within the source's retention period and the OTLP gateway's accept window.
Outside those bounds, recovery is loud and counted, never silent.

**Operationally honest.** Every polling/emit gap or skipped sample is alertable
(`window_lag`, `samples_skipped_total`, `auth_errors_total`, etc.), never silent. The tool
observes itself as a first-class concern.

**Per-bucket gauges for analytics.** Portkey analytics metrics are emitted as OTLP Gauge,
not Sum/Counter. Each data point is the total for one 1-minute bucket. This avoids needing
a durable cumulative accumulator (which would complicate failover) and matches Grafana Cloud
Mimir's ingestion path. Use `sum_over_time` to aggregate over a window, not `rate()`.

**Emission is exactly-once (conditional).** Settled buckets are emitted once. The
deterministic encoder produces byte-identical OTLP for the same input — so a re-emit after
failover is a Mimir no-op (same `(series, timestamp, value)`) and a Loki dedup (byte-exact
log line). This is engineered: it depends on settle margins, deterministic encoding, and
monotonic checkpointing working together.

**Data minimisation is the first control, not the whole boundary.** The tool never requests
content, so content cannot leak via the request path. But the outbound field allow/deny-list
and the conformance gate tests are the actual content boundary — see
[Content Governance](./governance.md).

---

## See also

- [`ARCHITECTURE.md`](https://github.com/rknightion/genai-otel-bridge/blob/main/ARCHITECTURE.md) — full component specs and decision ledger
- [`docs/DESIGN.md`](https://github.com/rknightion/genai-otel-bridge/blob/main/docs/DESIGN.md) — failure handling, review dispositions, test plan
- [Content Governance](./governance.md)
- [High Availability](./high-availability.md)
- [Telemetry reference](./telemetry.md)
