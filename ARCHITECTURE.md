# Architecture — `ai-platform-o11y-integrator` (`aip-oi`)

**Status:** design (v1 in planning). **Audience:** maintainers and contributors.
**Scope of this document:** the durable design — what the tool is, the components and the
interfaces between them, how data flows, how it stays available and correct, how it emits, and
how it is configured. The **detailed, build-facing design spec** (concrete contracts, schemas,
failure handling F1–F47, the test plan, and the Opus + Codex review-disposition matrices) is the
tracked **`docs/DESIGN.md`**; only the step-by-step **implementation plans** live in the gitignored
`docs/superpowers/` scratch area.

---

## 1. Purpose & scope

`aip-oi` is a **generic, vendor-neutral integrator** that pulls **operational telemetry** from
AI-platform APIs and emits it to Grafana Cloud (or any OTLP endpoint) as **metrics and logs**.

It exists because the most operationally important signals for an LLM application platform —
gateway cost/tokens/latency/errors/cache, and evaluation scores — are produced *inside*
managed products (LLM gateways, eval platforms) and are reachable **only by their APIs**, not
by scraping or OTLP push. Those APIs are pull-only, rate-limited, paginated, and return
content you must be careful not to ingest. `aip-oi` is the component that does that pulling
well, once, and turns it into clean Grafana-native telemetry.

**In scope**
- Two source *categories*: **LLM gateways** (first vendor: Portkey) and **LLM eval/observability
  platforms** (first vendor: LangSmith).
- Producing **metrics** (derived gauges/counters) and **logs** (structured records). **Not traces** —
  the gateway hop is a black box to these APIs, and run data is emitted as a content-free log
  index, not synthesised spans. Distributed tracing of the *application* is a separate concern
  (the app's own OTLP), out of scope here.
- Emitting via **OTLP/HTTP**, direct to Grafana Cloud's OTLP gateway or to a local collector.
- Running as **highly-available, self-observing** infrastructure on the production critical path.

**Out of scope** (see [`followup.md`](./followup.md))
- Regulated-content / compliance governance beyond simple data-minimisation.
- Application tracing, RUM, infrastructure metrics, or anything the *application* emits itself.
- A dynamic/third-party plugin runtime (the tool is plugin-*ready*, not a plugin host).

---

## 2. Design principles

1. **Decoupled (hard rule).** No customer, vendor-deployment, or domain knowledge lives in core
   code or default config. Metric names, label keys, endpoints, cadences, windows, environment
   identifiers — all configuration. The only vendor-specific code is inside a source package,
   behind a common interface.
2. **OTLP-native.** One transport for both metrics and logs. The tool is "an OTLP source"; where
   that OTLP goes (Grafana Cloud gateway, or a local Alloy/OTel Collector) is configuration.
3. **Modular, compile-time sources.** Each source is a self-contained Go package implementing a
   small interface. Adding a source is a code contribution + rebuild — not a runtime plugin.
4. **Pull-by-window, forward-only.** Every source loop pulls a bounded time window forward from a
   durable watermark. The source API is the replayable buffer, so restarts/failovers recover **without
   a WAL — gap-free *within* source retention and the sink's accept window**. Outside those bounds
   (very long outage, a too-old/rejected sample) recovery is **loud, counted loss**, not silent and not
   unconditionally gap-free (see §9). "Gap-free" is a *conditional, engineered* property, never absolute.
5. **Operationally honest.** A polling/emission gap or a skipped (rejected/abandoned) sample is always
   an alertable, counted signal (`window_lag`, `samples_skipped_total`, `backfill_unstorable_total`),
   never silent. The tool observes itself as a first-class concern.
6. **Data minimisation as the *first* control (not the whole boundary).** The tool never *requests*
   message bodies / prompts / completions — so they can't leak via that path. But minimisation alone is
   not the content boundary: an **outbound field allow/deny-list** governs *every* emitted field (labels,
   log body, structured metadata), and a **content-leak conformance gate** on the outbound payload is a
   release gate the moment any logs/run-index feature ships (§10/§11; Cdx-C7/H7).

---

## 3. Architecture at a glance

```
                          ┌──────────────────────── aip-oi (Deployment, N replicas) ────────────────────────┐
                          │                                                                                  │
                          │   Coordinator (k8s Lease)  ──elected──►  only the leader runs the Scheduler      │
                          │                                                                                  │
   AI-platform APIs       │   ┌── Scheduler (leader only) ─────────────────────────────────────────────┐    │
  ┌────────────────┐      │   │                                                                         │    │
  │ LLM gateway    │◄─────┼───┤  per Loop:  tick ─► rate-limit ─► Source.Collect(ctx, watermark)        │    │
  │ (Portkey)      │ HTTP │   │                         (GET, forward window)   │ derive                │    │
  ├────────────────┤  GET │   │                                                 ▼                       │    │
  │ eval platform  │◄─────┼───┤                                          [bounded queue]                │    │
  │ (LangSmith)    │      │   │                                                 │  block on full        │    │
  └────────────────┘      │   │                                                 ▼  (backpressure)       │    │
                          │   │                          emit-worker pool ─► Emitter.Emit(Batch)        │    │
                          │   │                                                 │  OTLP/HTTP (retry)    │    │
                          │   │                                  on success ─►  ├─► advance watermark   │    │
                          │   │                                                 │   (Checkpointer)      │    │
                          │   └─────────────────────────────────────────────────┼───────────────────-─┘    │
                          │   Checkpointer (k8s ConfigMap)  ◄────────────────────┘                          │
                          │   Self-observability (OTel-Go SDK) ─────────────────────────────────────────────┼──► (self/meta OTLP)
                          └──────────────────────────────────────────────────────┼───────────────────────-─┘
                                                                                   ▼  OTLP/HTTP
                                                          Grafana Cloud OTLP gateway  (or local collector)
                                                                       │ metrics → Mimir   │ logs → Loki
```

Three things this picture encodes: **one replica emits at a time** (leader election);
**collection is decoupled from emission** by a bounded queue (which absorbs *transient* sink slowness;
under *sustained* slowness it blocks on full and intentionally backpressures collection — visible as
rising `window_lag`, never silent loss); and **the watermark only advances on successful emit** (the
basis of the *conditional* gap-free / no-duplicate behaviour — engineered, see §9, not assumed).

---

## 4. The vendor-neutral model (the central seam)

Everything vendor-specific is converted, inside a source package, into this small set of types.
Sources produce them; the emitter consumes them. Nothing else crosses this boundary.

```go
// MetricKind selects the OTLP metric representation. v1 emits Gauge ONLY; Sum carries the
// temporality+monotonicity it needs in the seam now (Cdx-M2) so adding a Sum later is non-breaking.
// NOTE: the Grafana Cloud OTLP gateway (→ Mimir) ingests CUMULATIVE temporality only — DELTA sums
// are silently dropped on the Prometheus path. So a Sum to the GC OTLP emitter MUST be Cumulative;
// the emitter rejects Temporality==Delta for that target (§10). This is a reason we model the
// per-bucket Portkey counts as Gauges, not Sums (avoids needing durable cumulative accumulator state).
type MetricKind int
const ( Gauge MetricKind = iota; Sum )
type Temporality int
const ( TempUnset Temporality = iota; Delta; Cumulative )

// Sample is one derived metric data point.
type Sample struct {
    Name        string          // final series name, config-derived (e.g. "portkey_api_requests")
    Kind        MetricKind      // v1: Gauge only
    Temporality Temporality     // Sum only (Delta|Cumulative); ignored for Gauge
    Monotonic   bool            // Sum only; ignored for Gauge
    Unit        string          // optional; the OTLP gateway appends a suffix (e.g. "_seconds")
    Labels      map[string]string // bounded per-series dimensions  → OTLP metric data-point attributes
    Value       float64
    Timestamp   time.Time       // the bucket/observation time; emission is forward-only
}

// LogRecord is one structured log line. The two attribute maps are SEMANTIC INTENT fields — named
// for what they are FOR, not for one backend's storage model (Loki stream labels / structured
// metadata are mapping OUTCOMES, not model primitives).
type LogRecord struct {
    Timestamp         time.Time
    Body              string            // structured (JSON) or message text — never message bodies/content
    Severity          string
    IndexedAttributes map[string]string // LOW-card routing/query identity → OTLP resource attrs → Loki stream labels *if promoted* (§10). GUARD-POLICED tier; no high-card ids here.
    RecordAttributes  map[string]string // non-indexed per-record context (ids/correlation) → OTLP log-record attrs → Loki structured metadata
    TraceID           []byte            // 16 bytes, empty = unset → OTLP log trace_id. Source-provided correlation id passed through for logs↔traces linking (ledger #15); NOT span synthesis (#4 holds).
}

// CheckpointKey namespaces a watermark so it survives ordinary config evolution (Cdx-C4):
// a new source instance / workspace / region, a changed metric_prefix or label set, or a new
// graph added to a loop, each get their own key — so a new series bootstraps its own history
// instead of being skipped by an already-current loop watermark.
type CheckpointKey struct {
    SourceInstance    string // e.g. "portkey-prod-eu" (not just the source TYPE)
    Loop              string // e.g. "analytics"
    OutputFingerprint string // hash of the emitted series set + naming config
}
func (k CheckpointKey) String() string // stable, used as the checkpoint store key

// Watermark is a loop's forward-only position. Opaque to the core; interpreted by the source.
type Watermark struct {
    Time   time.Time // last fully-emitted (or skipped-with-gap) observation time — monotonic
    Cursor string    // optional source-specific resume token (e.g. an export-job id)
    Epoch  int64     // leader lease epoch that wrote it — for write fencing (Cdx-C14)
}

// Batch is the unit produced by one Collect and consumed by one Emit.
type Batch struct {
    Key       CheckpointKey
    Samples   []Sample
    Logs      []LogRecord
    Watermark Watermark // the position this batch advances the key to, *iff* it emits (or is skipped-with-gap)
}
```

**Why the field *is* the policy.** A source expresses the destination class of every value by
*which field it places it in*: `Sample.Labels` → series labels; `LogRecord.IndexedAttributes` →
low-cardinality query/routing identity; `LogRecord.RecordAttributes` → non-indexed per-record
context. The source owns the *semantic* mapping (it alone knows "this is `use_case`", "this id is
`correlation_id`"); the emitter is purely mechanical. Neither knows about the other's domain.

These are **semantic intent fields, not a backend's vocabulary.** OTLP has resource attributes and
log-record attributes; Loki stream labels / structured metadata (and Prometheus labels) are backend
**mapping outcomes** (§10), not model primitives. `Sample.Labels` is the metric equivalent of the
indexed/identity tier — it keeps its idiomatic name, but the same cardinality discipline applies.
`IndexedAttributes` is the cardinality-**dangerous** tier and is **governed by the guard** (deny-by-
default allow-list + per-series budget, §5); high-cardinality per-record ids (`correlation_id`,
`run_id`, request ids) go in `RecordAttributes`. Global producer identity (`service.namespace`,
environment, source instance) is set once on the emitter as resource attributes — not per record.

---

## 5. Component interfaces (the frozen seams)

```go
// A Source is one vendor integration. It exposes one or more independent Loops.
type Source interface {
    ID() string        // stable, e.g. "portkey", "langsmith"
    Loops() []Loop
}

// A Loop is one independent pull→derive cycle within a source.
type Loop interface {
    Key() CheckpointKey        // namespaces this loop's watermark (Cdx-C4)
    Cadence() time.Duration    // how often to run
    // Collect pulls forward from `since`, derives a vendor-neutral Batch, and returns the new
    // watermark inside it. For simple GET loops it is side-effect-free; a stateful loop (e.g. the
    // logs-export lifecycle — create/start/poll/download/resume) carries job state in
    // Watermark.Cursor and must be idempotent-resumable (Cdx-C6, §4 logs loop). The runtime
    // advances the watermark only after a successful Emit (or an explicit skip-with-gap).
    Collect(ctx context.Context, since Watermark) (Batch, error)
}

// Emitter ships a Batch to a backend. v1: hand-encoded OTLP/HTTP. Pluggable (see followup.md).
// On a non-retryable reject it returns a typed error classifying the cause (duplicate-timestamp /
// too-old / 413 / bad-encoding) so the runtime can advance-past-with-gap vs skip+alert (§4.5, Cdx-C2).
type Emitter interface {
    Emit(ctx context.Context, b Batch) error
}

// Checkpointer durably stores watermarks keyed by CheckpointKey, shared across replicas for failover.
// Save is MONOTONIC and EPOCH-FENCED (Cdx-C14): it rejects a watermark whose Time ≤ the stored Time,
// or whose Epoch is older than the stored Epoch — so a stale/demoted leader cannot move the frontier
// backward. A Kubernetes Lease reduces overlap; it is NOT itself a write fence — this is.
type Checkpointer interface {
    Load(ctx context.Context, key CheckpointKey) (Watermark, error) // zero Watermark if absent;
                                                                    // error if present-but-unreadable (refuse start)
    Save(ctx context.Context, key CheckpointKey, w Watermark) error // monotonic + epoch-fenced; err on stale write
}

// Coordinator provides single-active-replica semantics. onElected runs the Scheduler; its
// ctx is cancelled on leadership loss. A no-op impl (always leader) covers single-replica/dev.
type Coordinator interface {
    Run(ctx context.Context, onElected func(leaderCtx context.Context)) error
}
```

The **registry** maps a config `type` string to a `Source` constructor. The **composition root**
(`cmd/aip-oi/main.go`) wires config → registry → enabled Sources → Scheduler → Emitter +
Checkpointer + Coordinator. Wiring only; no logic.

A thin, generic **governance guard** sits between derive and emit (analogous to an Alloy relabel
stage — the one place policy is enforced centrally, so a source bug cannot leak). It enforces
(Cdx-H6/H7): a **config allowlist of label keys** + a **per-series-name cardinality budget** (with a
`new_label_values{series}` self-metric as an early-warning before a series explosion); and an
**outbound field allow/deny-list applied to *every* emitted field** — `Sample.Labels`,
`LogRecord.IndexedAttributes`, **and** `LogRecord.Body`/`RecordAttributes` — so content or sensitive
identifiers (emails, study/document ids, slugs, error strings) cannot leave via a non-label field.
UUID-shape detection is one heuristic *within* this, not the whole guard.

---

## 6. The loop lifecycle

For each enabled `Loop`, while this replica is leader:

1. **Tick** on `Cadence()` (with jitter).
2. **Acquire** the per-source rate-limit token (token bucket, shared across that source's loops).
3. **Load** the watermark: `Checkpointer.Load(loop.Key())` (keyed by `CheckpointKey`, §4/§5).
4. **Collect**: `Loop.Collect(ctx, watermark)` — performs the HTTP GET(s) for the forward window
   and derives the `Batch`. Single-flight per loop: a loop never overlaps itself, and backfill
   never overlaps live polling.
5. **Enqueue** the `Batch` onto the bounded queue. If the queue is full, **block** (backpressure)
   — the loop slows, `window_lag` grows, the staleness alert fires. Never drop.
6. An **emit worker** dequeues, calls `Emitter.Emit(batch)`, with retry/backoff.
7. **On success, or on a non-retryable reject (advance-past-with-gap, §4.5/§9):**
   `Checkpointer.Save(batch.Key, batch.Watermark)` (monotonic + epoch-fenced) and record self-metrics.
   On a *retryable* failure after the retry budget: the watermark is *not* advanced, so the next tick
   re-pulls the same window (recovers **within** source retention + the sink accept window; older →
   loud counted loss, §9). A non-retryable reject advances past with `samples_skipped_total` so the
   loop never poison-pills.

Per-loop ordering is preserved (a loop's batches emit in order), so a saved watermark always
reflects a contiguous prefix of emitted data.

---

## 7. Scheduling, rate-limiting & backpressure

- **Per-source rate limiter** (token bucket; `rps`/`burst` from config), shared across that
  source's loops. Sources expose no rate-limit headers in practice (see §15), so the tool
  **self-throttles** rather than reacting to headers, and honours any source "quota exceeded"
  flag by backing off.
- **Jitter** on every tick to avoid thundering-herd alignment across loops/replicas.
- **Bounded queue, block-on-full.** This deliberately differs from the OTel exporter default
  (drop-on-full): we choose backpressure so there is never silent loss. Sizing is config.
- **Emit retry**: exponential backoff with jitter (initial 5s, ×1.5, cap 30s, ~5-min elapsed
  budget), retrying transport errors + 429 + 502/503/504, not 500. Modelled on the policy Alloy
  uses against the same gateway.

---

## 8. High availability — leader election & single-emit

- `aip-oi` runs as a **long-running Deployment with N replicas** (3 = active/passive/passive is a
  sensible default; HA is opt-in via config, not mandatory).
- The `Coordinator` is implemented with a **Kubernetes Lease** (`coordination.k8s.io/v1`, via
  client-go `leaderelection`). Exactly one replica holds the lease and runs the Scheduler;
  the others idle hot and take over within the lease duration (~15s) on leader loss.
- A **no-op Coordinator** (always leader) is used for single-replica and local/dev runs. A
  non-Kubernetes lock could be added later behind the same interface.
- **Single-emit** is the *common-case* consequence (only the leader's Scheduler runs) — but the Lease
  **reduces overlap, it is not a write fence**: a renewal race / partition can briefly produce two
  leaders. The actual correctness guarantee against double-emit/backwards-frontier is the **monotonic +
  lease-epoch-fenced checkpoint write** (§9, Cdx-C14), not the Lease alone.

RBAC required by the k8s impls: `coordination.k8s.io/leases` (get/create/update) and, for the
default checkpointer, `configmaps` (get/create/update) in the tool's own namespace.

---

## 9. Checkpointing & (conditional) gap-free failover

- **Watermark** = forward-only, **monotonic** position (`{Time, Cursor, Epoch}`), persisted by the
  `Checkpointer` under a `CheckpointKey` (`{source_instance, loop, output-fingerprint}` — §4/§5,
  Cdx-C4). Default impl: a **Kubernetes ConfigMap** (one key per `CheckpointKey`), via a single writer
  goroutine, throttled, resource-version optimistic concurrency. **`Save` is monotonic + lease-epoch
  fenced** (rejects ≤-stored Time or stale Epoch — Cdx-C14), so a Lease (which only reduces overlap)
  does not have to be a write fence. A **file** impl (atomic temp-then-rename, periodic + on-shutdown
  sync) covers dev — **discouraged for HA/critical prod** (corrupt-file re-bootstrap risks dup-timestamp
  storms; Cdx-M5).
- **Failover recovers gap-free *within bounds*, without a WAL — not unconditionally.** A new leader
  loads the watermark and resumes; the ~15s of missed ticks is re-pulled because the **source API is
  the replayable buffer**. This holds **within source retention and the sink's accept window**; beyond
  them (a long outage, a too-old sample) recovery is **loud, counted loss** (`backfill_unstorable_total`),
  and a non-retryable reject advances-past with a counted gap (§4.5). (This is the key difference from
  Alloy's
  `remote_write` WAL, which exists because *scraped* samples are not replayable; ours are.)
- **No duplicates — but the sinks are only *conditionally* idempotent, so we engineer the
  condition.** Mimir does **not** overwrite on `(series, timestamp)`: a same-`(series, ts)` sample
  with a *different value* is **rejected** (`err-mimir-sample-duplicate-timestamp`); only a
  *value-identical* re-send is a no-op. Loki keeps a line only if it is *byte-identical* at the
  same `(stream, ts)`. So at-least-once is safe **iff re-emission is value/byte-identical**, which
  we guarantee three ways: **(1) emit each aggregate bucket exactly once, only after it has settled**
  (`bucket_settle` ≥ the *measured* maximum source late-arrival lag) so its value is final — a
  crash-before-save re-emit then carries the same value; **(2) deterministic encoding** — attribute
  KV lists in sorted-key order, any structured log body as canonical JSON — so re-emitted bytes are
  identical; **(3) never re-emit an already-emitted (≤ watermark) bucket** — if a later poll shows a
  settled bucket changed (late arrival beyond settle), we do *not* re-emit (you cannot correct a
  same-`ts` value in Mimir), we increment `bucket_revised_after_settle_total` so the drift is
  observable and `bucket_settle` can be tuned (detection is **active**: `bucket_revised_after_settle_total`
  fires on every post-settle change; alert on `rate(...) > 0` to signal that `bucket_settle` needs
  bumping). Result: no duplicates, no 400 stalls, bounded+visible inaccuracy — without a distributed transaction.
- **Emit-time 400s — advance-past + counted gap (Cdx-C2), never "don't advance".** A non-retryable
  reject (`duplicate-timestamp`/`too-old`/`413`-on-min-chunk) records `samples_skipped_total{reason}`
  + a gap log and **advances the monotonic watermark past the offending bucket** so the loop always
  progresses. (An earlier "drop but don't advance" rule was a poison pill — it re-pulled and
  re-rejected forever, fatal for `too-old`, which only ages further.) A `bad-encoding` 400 → skip +
  alert (a real bug; must not silently advance). Emit is **per-bucket granularity** so a partial accept
  leaves accepted samples durable and only the rejected bucket is skipped-with-gap. Conditions (1)–(3)
  keep these off the steady-state path; the classification is defence-in-depth.
- **Checkpoint writes are monotonic + lease-epoch-fenced (Cdx-C14).** A Kubernetes Lease reduces
  overlap but is not a write fence: `Save` rejects a watermark ≤ stored or from a stale epoch, so a
  demoted/over-lapping leader cannot move the frontier backward or double-advance. The checkpoint key
  is `{source_instance}/{loop}/{output-schema-fingerprint}` (Cdx-C4), so adding a new series bootstraps
  its own history rather than being skipped by an already-current loop watermark.

**Durability under a downstream (Grafana Cloud) outage — three tiers.** (1) *Default:* the in-memory
queue + replay-from-watermark already covers crashes, failover, and transient outages (the source is
the replayable buffer). (2) *Recommended for production:* front `aip-oi` with a **persistent-queue
Alloy** (`otelcol.storage.file` on a PVC; `block_on_overflow=true`; `num_consumers=1` for metrics) —
no `aip-oi` code, and it owns buffering/retry to Grafana Cloud; the OTLP exporter's durability is an
opt-in persistent sending queue, distinct from the always-on `remote_write` WAL.
(3) *The real lever for long outages:* **for metrics, no local buffer beats the backend's
out-of-order / too-old accept window** — surviving a multi-hour outage means widening that window
(Grafana Support), not buffering. For logs, a buffer also avoids an expensive re-fetch.

**Memory is modelled and bounded to the container limit** (never OOM-killed): the dominant consumers
are the bounded per-loop queues and the largest in-flight payload (a Portkey logs-export can be 50k
records — **streamed/chunked, never buffered whole**); `queue.*` sizes + `GOMEMLIMIT` are set against
the k8s memory limit, and Go memstats are self-observed. Detail in `docs/DESIGN.md` §5.

---

## 10. Emit — OTLP mapping & cardinality governance

**Transport.** OTLP/HTTP to the configured endpoint (Grafana Cloud's OTLP gateway, or a local
collector). Auth is HTTP Basic `base64(instance-id:token)` for Grafana Cloud; gzip-compressed;
nanosecond timestamps; multiple resources per export. The OTLP payloads are **hand-encoded
protobuf** (`go.opentelemetry.io/proto/otlp` + `google.golang.org/protobuf`), *not* produced via
the OTel metrics SDK — because the SDK's instrument API is built for instrumenting live code, not
for republishing externally-derived series at explicit historical timestamps. The OTel-Go SDK is
used for **self-observability** (§11), which is exactly its sweet spot.

**Attribute placement** (the cardinality rule, mechanical in the emitter):

| Model field | OTLP placement | Becomes (via the gateway) |
|---|---|---|
| global `identity` (config) | resource attributes | promoted labels + `target_info` (`service_name`, `service_namespace`, `deployment_environment`, …) |
| `Sample.Labels` | metric data-point attributes | that series' own labels |
| `LogRecord.IndexedAttributes` | log **resource** attributes | Loki stream labels *only if the target stack's OTLP-log config promotes them* — see caveat below |
| `LogRecord.RecordAttributes` | log **record** attributes | Loki **structured metadata** (`trace_id`, `correlation_id`, …) |

> **⚠ OTLP→Loki label promotion is NOT automatic (Cdx-C8/A6).** A resource attribute becomes a Loki
> *stream label* only if the stack's OTLP-log mapping promotes it (there's a default promoted set + a
> label cap); otherwise it lands as structured metadata, so a query like `{workspace="x"}` silently
> matches nothing or scans broadly. So for **logs**, "field placement is policy" is necessary but not
> sufficient: a deployment must **validate the target stack's OTLP-log mapping** (or route logs through
> a collector that sets the intended label hints — the regulated-mode Alloy path). A PoC item
> (`docs/DESIGN.md` OP5g); this caveat does not affect the v1 metrics-only slice. **Where a label
> requirement is known upfront, it is recorded as a stack-setup action so the promotion is
> configured before deployment.**

**Hard governance rules (Cdx-H6/H7):** the guard enforces a **config label-key allowlist + per-series
cardinality budget** (UUID/per-request/per-run ids are never labels — one heuristic within that), and
an **outbound field allow/deny-list over *all* emitted fields** (labels, log body, structured
metadata) so content/identifiers can't leave via a non-label field. Series dimensions are a small,
bounded, config-declared set.
The governance guard (§5) enforces this independently of any source.

**Naming.** The gateway transforms names deterministically (dots→underscores, unit suffixes,
non-dimensional units dropped, `job` synthesised from `service.namespace`/`service.name`). The
tool emits clean metric names + units; the post-gateway series names are therefore predictable and
documented per source. Note: the gateway does **not** emit `otel_scope_*` labels — do not rely on
them for querying.

**Forward-only emission** (§6/§9) keeps every sample at or ahead of the series' latest timestamp,
avoiding Mimir out-of-order / too-old rejections.

**Temporality: cumulative only.** The Grafana Cloud OTLP gateway (→ Mimir) ingests **cumulative**
temporality; **delta sums/histograms are silently dropped** on the Prometheus path (Alloy's mitigation
is `otelcol.processor.deltatocumulative`). The emitter therefore **rejects `Temporality==Delta`** for
the GC OTLP target. v1 emits **Gauges only**, so this is moot for the slice — and it is a reason we
model Portkey's per-bucket counts as **gauges, not cumulative Sums**: a cumulative counter from a
per-interval API would need a durable accumulator surviving restarts, which fights the stateless
republish + forward-only model. (A non-GC OTLP target that accepts delta could be supported behind the
`Emitter` interface later.)

**Deterministic encoding & bucket finality.** Attribute KV lists are encoded in **sorted-key
order** and any structured log body is **canonical JSON** (sorted keys, fixed float formatting), so
re-emitting the same logical batch yields byte-identical output — the precondition for the
conditional sink-idempotency in §9. Aggregate buckets are emitted **once, after settling**, and
never re-emitted once changed (§9). **Backfill is window-bounded twice over:** it must stay inside
the source's fine-granularity regime (so a wide re-pull doesn't flip 1-min buckets to hourly — walk
multiple short sub-windows instead) **and** inside Mimir's accept window (samples older than the
stack's out-of-order / too-old bound are unstorable and are abandoned with a loud counter, never
retried into a 400 loop). The emitter also **guards on bucket spacing** — if a response's bucket
step ≠ the expected granularity, it rejects and alerts rather than emitting mis-aligned series.

---

## 11. Self-observability

The tool is on the production critical path, so it observes itself as a first-class concern,
**OTLP-natively** (no Prometheus `/metrics` scrape endpoint).

- **Self-metrics** (via OTel-Go SDK) include, per source/loop: `last_success_timestamp_seconds`,
  `window_lag_seconds`, `api_errors_total`, `emitted_total`, `rate_limited_total`,
  `queue_depth`, `emit_latency_seconds`, and a `leader` gauge. These are marked to **survive any
  Adaptive-Metrics aggregation** — they are the staleness signal and must not be silently dropped.
- **Self-logs**: structured (logfmt) to **stdout**, scraped by the k8s-monitoring collector → Loki —
  NOT pushed via OTLP (a deliberate divergence from OTLP-everywhere, for logs only; self-metrics stay
  OTLP-push). Format is config-keyed (`log.format`, default `logfmt`) for cheap Loki parsing; built in
  `internal/logging`, set as the slog default in `cmd/aip-oi`. (For an HTTP poller the logs carry most
  of the causal story; **opt-in, default-off self-APM tracing** adds per-tick spans when enabled —
  decision #14, `selfobs.tracing.enabled`, same self OTLP path into Tempo.)
- **Separate destination**: self-telemetry can be pushed to a **different OTLP endpoint** than the
  product telemetry (for customers with a meta-monitoring setup); it defaults to the product
  endpoint, tagged so it is distinguishable.
- **Health**: lightweight `/healthz` and `/readyz` HTTP endpoints for Kubernetes liveness/readiness
  probes only (not telemetry).

A polling gap therefore surfaces as a `window_lag`/staleness alert in the customer's own Grafana —
the tool failing is itself observable.

---

## 12. Configuration model

A single declarative config file (YAML), with secrets referenced via `${ENV}` / file references,
never inline. Illustrative shape (the values shown are *examples*, not built-in defaults):

```yaml
emit:
  telemetry:                                   # product signals
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
  self:                                        # optional meta-monitoring; falls back to telemetry
    otlp:
      endpoint: ${META_OTLP_ENDPOINT}

identity:                                      # → OTLP resource attributes
  service_namespace: aip-oi
  deployment_environment: ${ENV}

ha:
  coordinator: lease                           # lease | none
  checkpoint: configmap                        # configmap | file

queue:
  max_batches: 256                             # bounded; block on full
  emit_workers: 4

sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    rate_limit: { rps: 1, burst: 3 }
    source_instance: portkey-${ENV}            # part of the CheckpointKey (Cdx-C4); not just the type
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m                            # ≤55m: stays in the 1-min-bucket regime, never flips to hourly (Cdx-H5)
        bucket_settle: 10m                     # measured ~185s late-arrival lag (2026-06-19); headroom; emit-once-after-settle
        metric_prefix: portkey_api
        graphs: [requests, cost, tokens, latency, errors]   # workspace-aggregate; the v1 slice
        # groups_metadata: [use_case]          # OUT of v1 — enable only after the groups PoC (OP5b/RP1, Cdx-C5)
      logs_export: { enabled: false, cadence: 15m }     # later phase (stateful lifecycle, Cdx-C6)

  - type: langsmith
    enabled: false                             # phase 3
    base_url: ${LANGSMITH_BASE_URL}
    auth: { header: x-api-key, value: ${LANGSMITH_API_KEY} }
    http: { user_agent: "aip-oi/0.1" }         # required: default UA may be WAF-blocked (see §15)
    rate_limit: { rps: 1, burst: 2 }
    loops:
      sessions: { enabled: true, cadence: 5m, metric_prefix: langsmith_project }
      runs:     { enabled: false, cadence: 5m }          # run-index + correlation, later phase
```

Config is loaded once, validated up front (fail fast on missing required fields / bad cadences /
unknown source types), and is otherwise immutable for the process lifetime.

---

## 13. Repository layout

```
ai-platform-o11y-integrator/
├── cmd/aip-oi/main.go            # composition root — wiring only
├── internal/
│   ├── config/                   # schema, load, validate, env-ref resolution
│   ├── model/                    # Sample, LogRecord, Batch, Watermark  ← the central seam
│   ├── source/                   # Source/Loop interfaces + registry + governance guard
│   │   ├── portkey/              #   analytics + logs_export loops; thin HTTP client
│   │   └── langsmith/            #   sessions + runs loops; thin HTTP client
│   ├── emit/                     # Emitter interface
│   │   └── otlp/                 #   hand-encoded OTLP/HTTP emitter
│   ├── checkpoint/               # Checkpointer interface
│   │   ├── configmap/            #   k8s ConfigMap impl (default)
│   │   └── file/                 #   atomic-rename file impl (dev / non-k8s)
│   ├── coordinate/               # Coordinator interface
│   │   ├── lease/                #   k8s Lease leader election
│   │   └── noop/                 #   always-leader (single replica / dev)
│   ├── schedule/                 # scheduler, per-source rate limiting, the bounded queue + workers
│   ├── selfobs/                  # OTel-SDK self-metrics/logs, health endpoints
│   └── httpx/                    # shared HTTP client: User-Agent, TLS, timeouts, retry
├── deploy/helm/                  # chart: Deployment, RBAC (Lease + ConfigMap), config, secrets
├── docs/DESIGN.md                # tracked: detailed design spec + review dispositions
│   └── superpowers/              # gitignored scratch: implementation plans only
├── ARCHITECTURE.md  README.md  followup.md
```

---

## 14. Source modules (first two)

Each source converts its API into the §4 model and exposes one or more loops.

### Portkey (LLM gateway)
- **`analytics` loop** — polls the Analytics *graphs* endpoints (`requests`, `cost`, `tokens`,
  `latency`, `errors`, `users`) and *groups* endpoints (e.g. `groups/metadata/{key}`,
  `groups/ai-models`) over a short forward window, deriving gauges under a configurable prefix
  (`portkey_api_*` by default). Label provenance matters: the key is workspace-scoped, so
  `workspace` is **constant** (identity/resource attribute, not a per-series label); the plain
  *graphs* yield workspace-aggregate series, while per-series dimensions (`ai_model`,
  `metadata_<key>` such as `metadata_use_case`) come from the *groups* endpoints. `status_class` is
  **not** available from the Analytics API (§15) — it is a logs-derived label produced by the
  `logs_export` loop.
- **`logs_export` loop** (phase 2) — runs the export lifecycle (create → start → poll → download via
  signed URL), **never selecting `request`/`response`** (content excluded at source), transforms the
  JSONL into `LogRecord`s, and ships them as OTLP logs (→ Loki). Cache/retry/fallback signals that
  are *not* in the Analytics API live here (see §15) and are best derived downstream with Loki
  recording rules.

### LangSmith (eval / observability platform)
- **`sessions` loop** — polls per-project session stats (`?include_stats=true`): run count, latency
  p50/p99, first-token, tokens, cost, error rate, and **`feedback_stats` eval-score facets** →
  gauges (`langsmith_project_*`), labelled by `project`/`session` and `feedback_key` (the evaluator
  name) — never by run id.
- **`runs` loop** (later phase) — `POST /runs/query` with a `select` list that **excludes**
  `inputs`/`outputs`/`events`, deriving a content-free run-index log stream (run/trace/correlation
  ids as structured metadata) for cross-signal correlation.

---

## 15. Validated source-API behaviour (live, 2026-06-18)

Findings from exercising the real dev APIs (GET-only). These are vendor-API facts the source
modules must encode, and they **correct several assumptions** in the originating analysis.

**Portkey Analytics** (`https://api.portkey.ai/v1`, header `x-portkey-api-key`):
- Time params `time_of_generation_min`/`_max` are **ISO-8601** (epoch is rejected).
- **Bucket granularity is window-adaptive, not parameterised**: ~30-day window → daily buckets;
  ≤24h → hourly; **≤1h → 1-minute**. There is no `granularity` parameter — you choose resolution by
  choosing the window. ⇒ poll a **short forward window** for fine resolution; emit only newly-closed
  buckets (forward-only).
- Response shape: `{ summary:{total}, data_points:[{timestamp,total}], is_quota_exceeded, object }`.
  Watch `is_quota_exceeded`.
- Graph endpoints that **exist**: `requests`, `cost`, `tokens`, `latency`, `errors`, `users`.
  Graph endpoints that **404** (under every naming variant tried): `cache-hit-rate`,
  `rescued-requests`, `status-codes`, `feedback`. ⇒ **cache / retry / fallback / status-class are
  not available from the Analytics API** on this version; they must come from the logs-export path
  (or are not API-accessible). The `errors` graph carries only a total, no status breakdown.
- `groups/metadata/{key}`, `groups/ai-models`, `groups/users` exist and bind to the key's workspace.
- **No rate-limit headers** are returned (only a request id) ⇒ the tool must self-throttle.
- `GET /v1/logs/exports` (list/latest export jobs) returns 200 on the public control plane with a
  workspace key — useful for resume.

**LangSmith** (self-hosted, header `x-api-key`):
- On the **dev** instance a workspace-bound key needed no `X-Tenant-Id`. ⚠ This is a *dev* result, not
  a production guarantee (Cdx-H3): the validated-platform version and tenant/key semantics remain
  **PoC/deployment gates** — do not assume prod matches dev.
- The dev instance reports version **0.13.5**, and `GET /api/v1/sessions/{id}?include_stats=true` there
  returns **fully-populated `feedback_stats` eval facets** (`{evaluator: {n, avg, stdev, …}}`) — strong
  evidence the eval-score-gauge feature works at this version, but the **production** version/feature
  gate stays open until confirmed on the validated platform (Cdx-H3).
- `POST /api/v1/runs/query`: the field is `session` (not `session_id`); the `select` enum is fixed
  (`id, name, run_type, start_time, end_time, status, error, extra, events, inputs, outputs`).
  **With no `select`, full `inputs`/`outputs` content is returned** — so a content-free index
  **requires** an explicit `select` that omits `inputs`/`outputs`/`events`. (`latency`/`tokens`/
  `feedback_stats` are not run-`select` fields; they come from the sessions endpoint.)
- The instance sits behind an **edge/WAF that blocks default client User-Agents** (a default
  `Python-urllib` UA got 403; a normal UA got 200). ⇒ the HTTP client **must set a sane
  `User-Agent`**, and the edge may need the tool's identity allow-listed.

> These are the live behaviours v1 is built and tested against (the `httptest` fakes mirror these
> exact shapes). Where they diverge from the originating analysis, this document is the
> authority for the tool; the analysis doc may be reconciled separately.

---

## 16. Decision ledger

| # | Decision | Rationale | Alternatives rejected |
|---|---|---|---|
| 1 | Generic, decoupled, modular (compile-time sources) | Bounded source universe (gateways + eval platforms), all maintained in-tree; clean interfaces give 90% of "product" feel without a plugin runtime's contract-freeze risk | Plugin SPI/runtime (premature for in-tree sources); single-tenant/customer-specific tool (fails the reuse goal) |
| 2 | Go | Grafana-ecosystem-native, single static binary for customers to own, goroutine concurrency for many rate-limited loops, first-class OTLP/AWS libs | Python (messy single-artifact distribution, weaker emission libs, SDK coupling); Rust (not idiomatic for this codebase, overkill for I/O-bound work) |
| 3 | OTLP/HTTP emit, hand-encoded protobuf — **the committed, sole destination wire format** | One transport for metrics+logs; native to Grafana Cloud or a collector; hand-encoding fits republishing external series at explicit timestamps (SDK does not). The in-process `model` stays a neutral *domain* type (not an OTLP wire type) for source-author ergonomics, the guard/data-min chokepoint, deterministic encoding, proto-version decoupling, and because it carries pipeline concepts OTLP lacks (Batch/Watermark/CheckpointKey) | OTel SDK for emit (wrong fit for external series); remote_write+Loki as alternative protocols (**not planned** — OTLP is committed; the `Emitter` interface is kept only as a test/queue seam, see followup §2) |
| 4 | Metrics + logs, not traces | The APIs yield derived metrics and content-free log indexes; the gateway hop is a black box; run data is a log index by design | Synthesising spans from runs (explicitly not recommended) |
| 5 | Pull-by-window, forward-only, watermark-gated; **emit-once-after-settle + deterministic encoding** | Source API = durable buffer; restart/failover gap-free **within source retention + the sink accept window** (older/rejected = loud counted loss), no WAL. Sink idempotency is *conditional* (Mimir rejects value-changed `(series,ts)`; Loki dedup is byte-exact), so we *engineer* the condition — settle buckets to finality, encode deterministically, never re-emit a changed bucket (§9) | Re-emitting whole windows (OOO rejects/dupes); assuming overwrite-on-`(series,ts)` (false); unconditional "gap-free"; cumulative counters from delta APIs (wrong) |
| 6 | HA via k8s Lease leader election, single-emit | Standard, dependency-free in-cluster pattern; single active emitter prevents duplicates | Sharding/active-active (needless complexity); external lock (extra dependency — addable later) |
| 7 | Checkpoint = k8s ConfigMap (default), file impl for dev | Shared/durable across replicas for failover, zero external deps; file impl mirrors Alloy positions | PVC/local-only (doesn't survive failover to another node); external store (extra dep — addable) |
| 8 | Bounded queue, **block-on-full**, in-memory + replay | Decouples collection from emission (absorbs *transient* sink slowness; *sustained* slowness intentionally backpressures collection → visible `window_lag`, never silent loss); replay covers crashes, so no WAL needed | Drop-on-full (silent loss); on-disk WAL (only needed if source retention < downtime — untrue here) |
| 9 | OTLP-native self-o11y, no `/metrics` (self-**metrics** OTLP-push; self-**logs** → stdout, collector-scraped) | Consistent OTLP-everywhere for metrics; staleness alerting still works (pushed not scraped); optional separate meta-monitoring endpoint. Self-logs ride the existing k8s-monitoring log pipeline (logfmt, cheap Loki parse) instead of standing up a second OTLP-logs path | Prometheus `/metrics` scrape (against the OTLP-native posture); a second OTLP-logs pipeline for self-logs (redundant when the collector already scrapes stdout) |
| 10 | Data minimisation by construction | Never request bodies/content; regulated-content framing parked | Fetch-then-strip (leak surface; parked to followup.md) |
| 11 | Log model fields named by **intent** (`IndexedAttributes`/`RecordAttributes`), not a backend's vocabulary | The model is an internal *domain* type, not a wire type. Intent names carry the cardinality-discipline signal (`IndexedAttributes` = the guard-policed, low-cardinality tier) and stay accurate even under OTLP-only: a per-record indexed attr becomes an OTLP *resource* attr only via emitter grouping (so Loki can promote it) — it is not a "resource attribute" in the producer sense. The guard/data-min layer keeps operating on plain maps. | Loki vocabulary (`StreamLabels`/`StructuredMetadata`) in the neutral seam (backend leak, even though OTLP is the only backend); OTLP proto types in the model (awful source-author API, spreads proto-version churn through the core, loses the determinism chokepoint, and OTLP has no Batch/Watermark/CheckpointKey) |
| 12 | **Opt-in, default-off self-profiling** (continuous profiling of the integrator's OWN runtime). Two config-selectable modes: **pull** (expose `net/http/pprof` on a dedicated listener for an Alloy/k8s-monitoring scrape — zero new egress/dependency surface) and **push** (the `github.com/grafana/pyroscope-go` agent → Grafana Cloud Profiles). Runs on **leader and standby** (process-level, not emit-gated); start failure is **fatal** (operationally honest — an operator who enabled it must not run silently un-profiled). Amends **#9** (self-o11y signal set: now metrics + self-logs + optional profiles); orthogonal to **#4** (which governs *republished product* data, still metrics+logs-not-traces). **Content discipline holds by construction**: profiles are stack frames of our own binary — they never touch the data plane, so no denylist change. **H4 identity preserved**: push tags carry the `-meta` namespace + `deployment.environment` + per-replica `service_instance_id`. `pyroscope-go` is the first non-OTel push client — infra, not vendor/domain knowledge, so it does **not** violate the decoupling rule. | A second OTLP path for profiles (OTLP profiling signal still immature); always-on profiling (cost + against default-off posture); in-process push as the only mode (forces the dependency + egress on every deploy — pull avoids both) |
| 13 | **Hard 1DPM cap, one knob `governance.max_dpm` (default 1), two implementations** | "1DPM emerges" was a footgun (sub-minute/grouped sources, future Sum, semantics change). Product: stateless per-(series,minute) LWW coalesce before splitByBucket (counted via `aip_oi_samples_capped_total`). Self: clamp the PeriodicReader interval to 60s/max_dpm (SDK is structurally 1-point-per-interval). | A wall-clock token bucket (drops legit backfill); a cross-batch seen-set (mutable state on the gap-free path — unnecessary: settle+watermark + ValidateOwnership + Mimir dup-reject already cover cross-batch/cross-loop) |
| 15 | **`LogRecord.TraceID` — correlation-id passthrough to OTLP `trace_id`** (opt-in via Portkey `logs_export` `settings.metadata_record_fields` + `metadata_trace_id_field`). A source may lift a content-free correlation id out of its payload (e.g. the Portkey request-metadata `correlation_id`, a UUID the backend apps stamp) into the OTLP log `trace_id`, so a backend (Grafana) can link these operational logs to the originating application's traces. **Does NOT violate #4** (metrics+logs-not-traces): we synthesise no spans and invent no trace from the gateway hop — we pass through an id the *application* already generated. Content discipline holds: the only metadata sub-keys that ever egress are operator-named, content-free, and validated against the hard-deny floor; the rest of `metadata` (PII) stays dropped. The value is also emitted as a content-free `RecordAttribute` (always queryable). FROZEN-seam change to `model.LogRecord` (additive `TraceID []byte`; LogRecord is transient in `Batch`, never persisted, so additive is safe). | A Loki derived-field linking on the `correlation_id` structured-metadata (works without the seam change but no native OTLP `trace_id`, and depends on per-datasource config); mapping it to an indexed/stream label (high-cardinality UUID — forbidden); synthesising spans from runs/the gateway hop (rejected by #4) |
| 14 | **Opt-in, default-off self-APM tracing** (spans over the integrator's OWN tick→collect→enqueue pipeline, `selfobs.tracing.enabled`). Exported over OTLP to the SAME self endpoint/auth/`-meta` identity as self-metrics (`emit.self`, falling back to `emit.telemetry`) — traces ride the same Grafana Cloud gateway into Tempo, **no separate egress channel** (decision-ledger contrast with #12, which needed pyroscope's own push path). Emitted via the OTel **global** tracer: disabled ⇒ the global stays the no-op tracer and a tick allocates only a no-op span (negligible at cadence ≥ 10s); enabled ⇒ `main` installs an SDK `TracerProvider` (AlwaysSample — our pipeline is low-volume). Start failure is **fatal** (operationally honest — enabled-but-can't-start must not run silently un-traced). Amends **#9** (self-o11y signal set: now metrics + self-logs + optional profiles + optional self-traces); orthogonal to **#4** (republished *product* data stays metrics+logs-not-traces — we never synthesise spans from the gateway hop or run data). **Content discipline holds by construction**: spans describe our own poll/emit timing + counts, never data-plane payload. The async emit (runner worker, decoupled via the queue) is intentionally NOT in the tick span — covered by `emit_errors_total` + the upstream histogram; cross-queue span propagation is a documented future path (followup §4). | Always-on tracing (cost + against default-off posture); a bespoke Tempo channel (the self OTLP path already reaches Tempo); threading a tracer through every seam (the global tracer + one chokepoint span gives the value without the spread) |
| 16 | **Portkey `api_key_use_cases`: per-api-key use-case label (metrics + logs).** Operators map Portkey api-key UUIDs to human use-case names; the integrator slugifies the name and stamps it as an `api_key_use_case` label (metrics) / record attribute (logs). **Metrics/logs architecture split driven by M7 (`ValidateOwnership`):** analytics and groups are `SeriesDeclarer`s — two distinct-keyed loop instances for the same metric name would fail `ValidateOwnership` (same series, different keys → duplicate-timestamp storm in Mimir). Therefore metrics use **one loop instance with N internal filtered passes** (one Key, one watermark, one ownership entry; settle-exceedance `revisionHistory` per slug). Logs are NOT a `SeriesDeclarer`, so fan-out to N `logsExportLoop` instances is ownership-safe and fits the per-job cursor model. `api_key_use_case` is a metrics data-point label (Prometheus-style; allow-listed in `AllowedLabelKeys()`) and a logs record attribute (record-tier; queryable as `| api_key_use_case="…"`, no GS1 dependency). Empty `api_key_use_cases` ⇒ byte-identical to pre-feature (metrics `Key()` unchanged ⇒ no watermark reset). Unlisted keys are intentionally out of scope. Detailed in `docs/DESIGN.md` §7 RP3. | Metrics fan-out (N analytics/groups instances) — rejects at startup via `ValidateOwnership`; pulling key names from Portkey admin API — would require org-admin scope (least-privilege violated); an "other" aggregate bucket — no exclude filter available, would double-count |

---

## 17. Out of scope / parked

See [`followup.md`](./followup.md): data-sensitivity/compliance governance; additional emit backends
(remote_write+Loki, on-disk WAL); vendor Go SDKs; LangSmith bulk-export; further source vendors;
deeper self-APM trace coverage (the opt-in per-tick span shipped in #14; cross-queue emit-span
propagation + more spans are followup §4); a plugin runtime.
