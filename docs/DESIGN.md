# Design spec — `genai-otel-bridge` v1 (tracked design-of-record)

**Tracked** (promoted from gitignored scratch on 2026-06-18 so the load-bearing invariants and the
§12 review-disposition matrix are durable and reviewable from a clone). `ARCHITECTURE.md` is the
higher-level durable design; **implementation plans** remain gitignored scratch in `docs/superpowers/`.
This is the detailed, build-facing spec — v1 scope, concrete contracts, schemas, failure handling,
the test plan, and the Opus + Codex review dispositions (§11/§12). Date: 2026-06-18.

---

## 1. v1 scope — the vertical slice

Prove the **entire spine** on one source loop, end-to-end, against real data, then add breadth
additively (each later loop is a new package behind the existing interfaces).

**v1 delivers:** the Portkey **`analytics`** loop → derived metrics → hand-encoded OTLP/HTTP →
Grafana Cloud OTLP gateway → Mimir, running on the full runtime: config load+validate; scheduler
with per-source rate-limit + jitter; bounded queue with block-on-full + emit-worker pool with
retry; leader election (k8s Lease) **and** the no-op coordinator; checkpoint (k8s ConfigMap)
**and** the file impl; OTLP-native self-observability; forward-only watermark.

**Explicitly deferred (additive, post-slice):** Portkey `logs_export` loop (→ OTLP logs → Loki);
LangSmith `sessions` loop (eval facets) and `runs` loop (run-index/correlation); remote_write+Loki
emitter; on-disk durable queue; data-sensitivity governance beyond data-minimisation.

---

## 2. Functional requirements

- **FR1** Load a YAML config; resolve `${ENV}`/file secret refs; validate on startup (fail fast).
- **FR2** Construct enabled sources via a registry keyed by `type`.
- **FR3** For each enabled loop, on its cadence (jittered), while leader: load watermark → Collect
  forward window → enqueue → emit → on success advance watermark.
- **FR4** Portkey `analytics`: poll the validated **workspace-aggregate graph** endpoints over a short
  forward window (≤55m); derive gauges; emit only newly-closed, settled buckets (forward-only). **The
  `groups` endpoints are OUT of the v1 slice** until their shape/pagination is proven (Cdx-C5/OP5b/RP1)
  — no per-`ai_model`/`use_case` labelled series until then.
- **FR5** Emit metrics as hand-encoded OTLP/HTTP with the §10 attribute placement; identity as
  resource attrs, series dims as data-point attrs.
- **FR6** Persist watermarks durably (keyed by `CheckpointKey`) and shared across replicas; resume
  **gap-free *within* source retention + the sink accept window** (older/rejected = loud counted loss, §6).
- **FR7** Exactly one replica emits at a time (leader election); no-op coordinator for single replica.
- **FR8** Bounded queue; block (backpressure) on full; never drop.
- **FR9** Emit self-metrics + self-logs via OTLP (optionally to a separate endpoint); expose
  `/healthz`+`/readyz`.
- **FR10** Never request content fields from any source API.

---

## 3. The vertical slice in detail — Portkey `analytics`

### 3.1 API contract (validated 2026-06-18)
- Base `https://api.portkey.ai/v1` (configurable); auth header `x-portkey-api-key`.
- `GET /analytics/graphs/{requests|cost|tokens|latency|errors|users}?time_of_generation_min=<ISO8601>&time_of_generation_max=<ISO8601>`.
- `GET /analytics/groups/metadata/{key}?…` for per-metadata grouping (e.g. `use_case`) — **documented
  here but OUT of the v1 slice** (gated on the groups-shape PoC, §1/§7 RP1); v1 uses graphs only.
- Response: `{ summary:{total}, data_points:[{timestamp,total}], is_quota_exceeded, object }`.
- **Bucket granularity is window-adaptive** (no granularity param). **OP5c measured (2026-06-19, §15):**
  ≤59m → **1-minute**; ~61m → **10-minute**; 24h → **hourly**. The flip out of 1-min is between 59m and
  61m. **Decision:** poll a trailing window **strictly inside the 1-min regime (≤55m, config)** — never
  near 1h, so jitter/skew can't flip the server's bucketing (H5). **Backfill walks multiple ≤55m
  sub-windows**, never one wide window. The source **guards on bucket spacing**: if a response's bucket
  step is not a positive multiple of the expected granularity, reject + alert (`granularity_unexpected`)
  rather than emit mis-aligned series.
- **Timestamps are bucket-START** (OP5e measured, §15): the data-point `timestamp` is the interval
  start and the **current in-progress bucket is included**. The source sets `startSemantics=true` and
  derives `bucket_end = timestamp + granularity`; the settle gate then keeps the in-progress (unsettled)
  bucket out.
- **Empty buckets are returned explicitly as `total: 0`, never omitted** (OP5f measured, §15) — so "zero
  traffic" is distinguishable from "poller down" directly from the data, and the source emits a 0-valued
  gauge per quiet bucket. (The granularity guard's k×gran omitted-bucket tolerance is therefore
  defensive only for Portkey.)
- **Buckets are eventually-consistent**: a recent bucket's value can change after first observation
  (late/async-logged requests); late-arrival semantics are **undocumented**. **OP5d (§15) measured
  (2026-06-19):** max post-close late-arrival lag ~185s on a live traffic-bearing workspace → default
  `bucket_settle = 10m` (headroom); settle-exceedance now actively detected via
  `genai_otel_bridge_bucket_revised_after_settle_total` (followup.md §8).
- No rate-limit headers; `is_quota_exceeded` boolean present. **On `is_quota_exceeded:true`, discard
  the whole batch** (derive nothing, advance nothing) + back off — never emit a quota-truncated body.
- Client timeout 10s (per the originating analysis); configurable.

### 3.2 Derivation → metric schema (defaults; all config-overridable)
Prefix `portkey_api` (config). **Label provenance** (validated): the key is workspace-scoped, so
`workspace` is **constant** → it is identity (a resource attribute / config-set value), not a
per-series label. The plain *graphs* return **workspace-aggregate** time series (no per-series
dimension); per-series dimensions come from the *groups* endpoints (`ai_model` from
`groups/ai-models`; `metadata_<key>` such as `metadata_use_case` from `groups/metadata/{key}`).
`status_class` is **not** available from the Analytics API (§15) — it is logs-derived (the
`logs_export` loop), so it is **out of the slice**.

**Slice sequencing within the loop:** v1 ships the workspace-aggregate graphs only (no per-series
dims). The groups endpoints (`ai_model` / `metadata_use_case` breakdowns) are a **post-v1 addition,
gated on the groups-shape PoC** (§7 OP5b/RP1) — not part of the v1 slice.

Slice metrics (Gauges; per-bucket values, not cumulative):
- `portkey_api_requests` — per-bucket request count.
- `portkey_api_tokens` — `{ai_model}` once groups are added.
- `portkey_api_cost_usd`.
- `portkey_api_latency_seconds{quantile}` — **OP5a measured (§15):** the `latency` graph is
  structurally different — summary `{avg,p50,p90,p99}`, data points `{timestamp,avg,p50,p90,p99}` (no
  `total`). Emit **one gauge per percentile** with a `{quantile="p50|p90|p99"}` label (a data-point
  attribute → gateway auto-label; no GS row). This needs a **dedicated derive path**; the generic
  `{timestamp,total}` path serves requests/cost/tokens/errors/users. **Knock-on (Task 15):** the
  governance guard defaults to deny-all-labels (CP-C6), so the composition root must set
  `AllowLabelKeys: ["quantile"]` or latency samples are dropped. **Unit (RESOLVED 2026-06-19):** the
  first live soak confirmed Portkey reports latency in **milliseconds** (e.g. p99≈155633), so `derive`
  divides by 1000 (`msPerSecond`) to honour the `_seconds` suffix (Prometheus/OTel convention). This
  closes the earlier s-vs-ms caveat (was followup.md §8).
- `portkey_api_errors` — per-bucket error count (error-rate = errors/requests, no status label).

Bounded label set only: `ai_model`, `metadata_<key>`. No per-request/trace/UUID labels, ever.

> Per-bucket counts are modelled as **gauges valued at the bucket** (the API returns per-interval
> values, not a running total). Dashboards use `sum_over_time`/aggregation; never `rate()` on these.
> This is also the right call given the **GC OTLP gateway ingests cumulative temporality only** —
> delta sums are silently dropped (Alloy converts via `deltatocumulative`); emitting per-interval
> values as a *cumulative Sum* would require a durable accumulator surviving restarts, which fights
> the stateless-republish + forward-only model. So: **gauges, not Sums.** Any future `Sum` to the GC
> OTLP emitter must be `Cumulative` (the emitter rejects `Delta`; §4.5).

### 3.3 Emission — the correctness model (revised after adversarial review)

**Why this is subtle:** the sinks are only *conditionally* idempotent. Mimir **rejects** a
same-`(series, ts)` sample whose *value differs* (`err-mimir-sample-duplicate-timestamp`); it
no-ops only on a value-identical resend. Loki keeps a line only if *byte-identical* at the same
`(stream, ts)`. Combined with eventually-consistent buckets, a naive "re-emit the window" or
"re-emit a corrected value" would either be **rejected (400 → poison-pill stall)** or **silently
wrong**. So at-least-once safety (the basis of gap-free failover, §4.3/§4.4) is *engineered*:

- **Emit each bucket exactly once, after it settles.** A bucket is eligible only once
  `bucket_end ≤ now − bucket_settle`, where `bucket_end = timestamp + granularity` (timestamps are
  bucket-START, OP5e/§15). `bucket_settle` ≥ the maximum source late-arrival lag — **OP5d (§15)
  measured ~185s max on a live workspace (2026-06-19); default `bucket_settle = 10m` for headroom**.
  One `Sample` per metric (per `{quantile}` for latency), `Timestamp = bucket_end`, Gauge.
- **Watermark = last emitted `bucket_end`** (a single hard frontier). Forward-only: only buckets with
  `bucket_end > watermark.Time` are ever emitted.
- **Never re-emit an already-emitted bucket.** If a later poll shows a settled (≤ watermark) bucket
  changed value, do **not** re-emit (you cannot correct a same-`ts` value in Mimir). Increment
  `bucket_revised_after_settle_total{loop}` so post-settle drift is **observable** (and `bucket_settle`
  tunable). This bounds inaccuracy and makes it visible — never a 400, never a stall.
- **Deterministic encoding** (emitter contract): attribute KVs sorted by key; any structured body is
  canonical JSON. So a crash-before-watermark-save re-emit (§4.4) is **byte/value-identical** → Mimir
  no-op / Loki dedup. This is what makes at-least-once safe.
- `Collect` is pure: it returns the batch + proposed watermark and mutates no state; the runtime
  advances the watermark only after a successful emit (§6 step 7).

*(Noted alternative for a future metric that genuinely needs late corrections captured: emit at
**poll-time** `ts` rather than bucket-time `ts` — corrections then land as new samples — at the cost
of bucket-time resolution. Not used in v1; documented in followup.md if needed.)*

---

## 4. Component specs

### 4.1 Config (`internal/config`)
- YAML → typed struct; unknown source `type` = fatal; missing required field = fatal; cadence < a
  floor (e.g. 10s) = fatal. **Window validation (fatal otherwise, M3):** `cadence + bucket_settle +
  jitter_margin ≤ window ≤ 55m`. The lower bound guarantees no uncovered time between polls *after*
  settling; the upper bound keeps every poll inside the 1-min-bucket regime (H5).
- **Series-name ownership (M7):** a given emitted series *name* must be owned by exactly one loop;
  startup fails on overlap, so two loops can never write the same `(series, ts)`.
- Secret resolution: `${ENV}` and `file:/path`. A referenced-but-unset secret = fatal at startup.
- Immutable after load. (Hot reload is out of scope for v1.)

### 4.2 Scheduler / rate-limit / queue (`internal/schedule`)
- One goroutine per loop; `time.Ticker(cadence)` + jitter (±10%).
- **Rate limiting is per *outbound request*, not per Collect (M6).** The token bucket
  (`golang.org/x/time/rate`, rps/burst from config, one per source, shared across its loops) is
  acquired inside the HTTP client (`httpx`) before *each* request — because one Collect fans out into
  many GETs (6 graphs + N groups pages). Without this the limiter doesn't actually bound the request
  rate and will trip a source's own limiter (e.g. LangSmith's ~10 req/10s shared budget). LangSmith
  additionally caps max-in-flight = 1.
- **Queue topology is frozen (C3 / resolves OP4): per-loop bounded queue + exactly one emit worker per
  loop** (one in-flight emit per loop). A shared pool "keyed by loop" is rejected — it preserves
  *dispatch* order but not *completion* order, and an out-of-order completion that advances the
  watermark past an un-emitted earlier batch is a silent gap. Per-loop single-flight emit makes
  "advance the watermark to the batch that just completed" trivially correct (it is always the
  contiguous successor). Cross-loop, fully concurrent — no head-of-line blocking between loops (F20).
- Bounded queue (`queue.max_batches` per loop); send **blocks** when full (backpressure, never drop).
- Single-flight per loop on **both** Collect and emit (a loop never overlaps itself).

### 4.3 Coordinator (`internal/coordinate`)
- `lease`: client-go `leaderelection` on a named Lease; leader runs the Scheduler under a ctx
  cancelled on leadership loss; on loss, in-flight Collect/emit are cancelled and the queue drained
  or discarded (watermark protects correctness either way).
- **Leadership reduces overlap; it is not a transaction (Cdx-C14/A2).** Single-emit is *not* assumed
  to fall out of the Lease alone. The leader epoch (lease acquisition identity) is threaded into every
  `Save` (§4.4) and checked immediately before `Emit`/`Save`, so a briefly-overlapping or demoted
  leader cannot move the frontier backward or double-advance.
- `noop`: invokes `onElected` immediately with a never-cancelled ctx (single-replica/dev).

### 4.4 Checkpointer (`internal/checkpoint`)
- **Checkpoint key = `{source_instance_id}/{loop}/{output_schema_fingerprint}` (Cdx-C4/A3), not bare
  loopID.** This survives ordinary production evolution: a second Portkey instance/workspace/region
  doesn't collide; changing `metric_prefix`/labels/resource identity re-namespaces; and **adding a new
  graph/metric to an existing loop creates a new key that bootstraps its own history** instead of being
  silently skipped because the loop watermark was already current (Cdx-A3). The fingerprint covers the
  output series set + naming config.
- **Writes are monotonic + epoch-fenced (Cdx-C14/A2 — Lease is not a write fence).** `Save` rejects a
  watermark ≤ the stored one (never moves the frontier backward) and carries the **leader lease
  epoch**; a stale demoted leader's write is rejected. The scheduler also checks lease ownership
  immediately before `Emit` and `Save` (§4.3). The single-threaded ConfigMap writer does *not* by
  itself prevent cross-replica stale writes — monotonicity + epoch do.
- `configmap`: one ConfigMap, one key per checkpoint key, value = serialized `{Time, Cursor, Epoch}`.
  **A single writer goroutine owns the ConfigMap** (M1) — loops hand it updates; it batches, writes
  with resource-version optimistic concurrency, **throttled** (≥ N s; re-pull is bounded + idempotent),
  size-guarded against the 1 MiB limit. At higher loop/instance counts an external store may become
  less optional (Cdx-H5 → followup.md).
- `file`: YAML map, atomic temp-then-rename, mutex snapshot-under-lock, periodic (≤10s) + on-shutdown
  flush. **Discouraged for HA/critical production** (Cdx-M5): a corrupt-file `ignore_invalid` fallback
  that re-bootstraps can cause duplicate-timestamp storms; prefer `configmap` (or external) in prod.
- **`Load` distinguishes absent from unreadable (OP3):** *confirmed-absent* (no key) → bootstrap from
  `now − bootstrap_lookback` (config; lookback ≤ the H3 backfill cap). *Exists-but-unreadable*
  (corrupt/permission/parse) → **refuse to start** (file impl may `ignore_invalid` with a loud
  warning, but k8s-unreadable is fatal) — bootstrapping over a real-but-unreadable watermark risks
  re-emitting already-stored buckets into duplicate-timestamp 400s (C1).

### 4.5 Emitter (`internal/emit/otlp`)
- Hand-encoded OTLP protobuf (metrics for the slice; logs later). Basic auth `instance-id:token`;
  gzip; nanosecond timestamps; one ResourceMetrics per identity.
- **Cumulative temporality only.** The GC OTLP gateway (→ Mimir) ingests **cumulative**; **delta sums
  are silently dropped** (Alloy converts via `deltatocumulative`). v1 is Gauges (moot); the emitter
  **rejects `Temporality==Delta`** for the GC target as a programming/config error. (Why we use
  per-bucket gauges, not cumulative Sums — see §3.2.)
- **Deterministic encoding (C2):** attribute KV lists emitted in **sorted-key order**; any structured
  `LogRecord.Body` is **canonical JSON** (sorted keys, fixed float formatting). This makes
  re-emission byte-identical — the precondition for §3.3/§4.4 idempotency. A unit test shuffles map
  order and asserts byte-equality.
- Retry: initial 5s, ×1.5, cap 30s, ~5-min elapsed budget, ±50% jitter; retry transport+429+502/503/504.
  **500 is retried at the next cadence, not inline** — a conscious latency/pressure tradeoff (Cdx-H12);
  it gets its own counter, never advances the watermark, never drops.
- **Rejection model — advance-past + counted gap (Cdx-C2/C3/A1, resolves the poison-pill).** Parse the
  gateway error. A **non-retryable** reject (`duplicate-timestamp`, `too-old`, `413` after splitting)
  **does not block the frontier**: record `samples_skipped_total{reason}` + a structured gap log, and
  **advance the watermark past the offending bucket** so the loop always makes progress. (The earlier
  "drop but do *not* advance" rule was a poison pill — it re-pulled and re-rejected forever, fatal for
  `too-old` which only ages further; Cdx-C2.) A `bad-encoding`/malformed 400 → skip + alert (a real
  bug, must not silently advance). The advance is still **monotonic** (§4.4).
- **Partial accept (Cdx-C3/A1).** Do not assume all-or-nothing OTLP accept (Mimir can ingest some
  samples and reject one). Emit at **per-bucket granularity** (one bucket's samples per export unit)
  so the advance/skip decision is per-bucket; on a partial reject, the accepted samples are durable
  and the rejected bucket is skipped-with-gap, never re-pulled into a loop.
- **Payload bounds & chunking (Cdx-C12/A8).** Bound each OTLP export by **max bytes**
  (`queue.max_batch_bytes` → `otlp.Config.MaxBytes`, proactive-split-then-reactive-split-on-413),
  independent of `queue.max_batches`. The implemented bound is bytes-only — there is no general
  samples/log-records **count** bound on the emit-export path itself (a source package may impose its
  own memory-chunking count knob, e.g. Portkey logs_export's `chunk_max_records`, but that governs
  chunking within a source's Collect, not this emit-export bound). **Chunk** large derivations (esp. a
  50k-record logs export) into multiple deterministically-ordered exports, streamed (§5). A `413` on a
  minimal chunk is treated as a non-retryable reject (skip+gap), never an infinite retry.
- `Emit` is synchronous from a worker's view; returns error after the retry budget is exhausted.

### 4.6 Self-observability (`internal/selfobs`)
- OTel-Go SDK meter+logger → OTLP (telemetry endpoint, or `emit.self` if set).
- **Distinct resource identity (H4):** self-telemetry uses its own `service.namespace`
  (e.g. `genai-otel-bridge-meta`), never the product identity — otherwise the gateway synthesises the same `job`
  and a single shared `target_info` series, and the two producers collide (duplicate-timestamp) or
  flap. Self and product must never share a resource identity on one Mimir tenant.
- **Self-metrics carry a per-replica `instance` (M4).** Staleness alerts are written as
  `time() − max by (loop) (last_success_timestamp)` / `max by (loop)(window_lag)` so they follow the
  leader correctly and survive a leader vanishing (use `absent`/`*_over_time` for total-failure).
- `/readyz` (200 once config loaded + coordinator started). **`/healthz` reflects a heartbeat (L1)** —
  scheduler-loop progress within K×cadence — so a wedged goroutine is restartable; it is *not* a
  constant 200 (a leader correctly blocked on backpressure must stay alive, so the heartbeat tracks
  *loop progress attempts*, not successful emits).

### 4.7 HTTP client (`internal/httpx`)
- Shared client with a **configurable, non-default User-Agent** (default-UA WAF-block is real, §15);
  per-source timeouts; TLS 1.2+; bounded connection pool; context-aware; acquires the per-source
  rate-limit token per request (M6).
- **Egress guard (H6):** optional config allow-list of permitted hosts/domains; **default-deny
  link-local / cloud-metadata (169.254.0.0/16) and, unless explicitly permitted, RFC-1918** — because
  `base_url` (and the future logs-export `signed_url`) are config/server-controlled inputs (SSRF
  surface). The future logs-export loop must additionally validate the `signed_url` host against an
  allow-list and assert the *downloaded* payload is content-free (defence beyond the field selector).

---

## 5. Non-functional requirements
- **HA:** survive single-replica loss with **no data gap *within* source retention + the sink accept
  window** (the source is replayable; older/rejected = loud counted loss, §6 — not unconditionally
  gap-free). The *recovery latency* is not "lease duration" (H1) — it is `LeaseDuration + RetryPeriod
  + one-collect-cycle` (acquisition + the new leader's first successful pull/emit), potentially
  minutes. Staleness-alert thresholds are
  set off *that* bound + margin, not off the lease duration.
- **Resource / memory (modelled & bounded to the container limit).** It is fine for the app to use
  several GB if needed, **provided memory is modelled and bounded under the k8s container limit** —
  never OOM-killed. The model:
  - **Dominant consumers:** (a) the per-loop bounded queues — `Σ queue.max_batches × max_batch_bytes`;
    (b) the **largest single in-flight payload** — for Portkey `logs_export` a job can be up to
    **50k records**, which if buffered whole is the real spike; (c) OTLP encode buffers; (d) Go
    runtime overhead.
  - **Bounding levers:** set `queue.max_batches` (per loop) and `max_batch_bytes` so worst-case
    `Σ + max_in_flight_download_chunk + overhead ≤ container limit × safety_factor`. **Stream/chunk
    large downloads** (decode the logs-export JSONL incrementally and enqueue in chunks — never hold
    the whole 50k-record file in memory), so per-loop memory is bounded by chunk size, not export
    size. Cap simultaneous in-flight batches (per-loop single-flight already does this for emit).
  - **Set `GOMEMLIMIT` to (a fraction of) the container memory limit** so Go's GC targets it and
    applies backpressure/GC before the cgroup OOM-kills — graceful degradation, not a hard kill.
  - **Self-observe** Go memstats (heap in-use, GC) via the OTel SDK and alert approaching the limit.
  - k8s `requests`/`limits` set from the model; document the formula so the limit and `queue.*`/
    `GOMEMLIMIT` are tuned together.
- **Cardinality:** every series' label set is bounded and config-declared; guard rejects violations.
- **Security:** secrets never logged, never in config-in-git, never in self-telemetry; least-privilege
  RBAC (own namespace: leases + configmaps).
- **Determinism:** identical input window ⇒ identical emitted series (idempotent).

---

## 6. Failure scenarios & handling

The set designed for, **after an independent adversarial review** (2026-06-18, Opus). The review's
findings are folded in here and across §3–§5/§7 (tagged C1–C3, H1–H6, M1–M7, F29–F36); see §11 for
the outcome summary. F1–F28 were the author's original set (several corrected by the review).

### Source-API failures
- **F1 Transient 5xx / timeout on Collect** → loop logs, increments `emit_errors_total{kind="collect"}`
  (the real per-loop counter — there is no standalone `api_errors_total`), does *not* advance the
  watermark; next tick re-pulls the same window. Repeated failure → `window_lag` grows → staleness alert.
- **F2 401/403 (bad/expired key, or WAF UA block)** → fatal-ish per source: mark source unhealthy,
  keep retrying with backoff, surface a distinct `auth_errors_total`; do not crash the whole process
  (other sources keep running).
- **F3 `is_quota_exceeded: true`** → back off that source, emit a `quota_exceeded` signal, retry later.
- **F4 Partial graph success** (e.g. `cost` ok, `latency` 500) → emit the graphs that succeeded for
  the window; do not advance the watermark past a bucket unless *all configured graphs* for it
  succeeded (else a later success would be an out-of-order write). **Open design point — see §7.**
- **F5 Schema drift + capability detection (Cdx-H4).** Don't hardcode a static capability set from one
  key/date (the live probe already found graph endpoints 404 that the source material expected). At
  startup (and on drift) the source does **capability detection** that distinguishes: endpoint absent ·
  plan/edition unsupported · permission denied · workspace-has-no-data · transient 404/route · schema
  changed. Derive what parses, emit a `source_capability{state}` self-metric + `schema_warning`, never
  panic on a missing field, and surface a removed-but-expected metric rather than silently dropping it.
- **F6 Open/incomplete & revised buckets (C1, the big one)** → a recent bucket may be incomplete (if
  emitted now → undercount) *and* may still change value *after* it settles (eventual consistency).
  **Mitigation:** emit a bucket only once `bucket_end ≤ now − bucket_settle`, where `bucket_settle` is
  the **measured** max late-arrival lag (PoC), not a guess. Emit once; **never re-emit a changed
  bucket** (you can't correct a same-`ts` value in Mimir) — count `bucket_revised_after_settle_total`
  so drift is visible and `bucket_settle` tunable (§3.3). Clock handling is UTC throughout (F26/F28).

### Emission failures
- **F7 Gateway 5xx/timeout** → retry per policy; on exhaustion the batch stays unemitted, watermark
  not advanced, batch re-derived next tick (queue slot freed). No silent drop.
- **F8 Gateway 429** → retry with backoff; backpressure propagates to Collect via the full queue.
- **F9 Gateway 400 — advance-past + counted gap, NOT "don't advance" (Cdx-C2/A1).** A non-retryable
  reject (`duplicate-timestamp`/`too-old`/`413`-on-min-chunk) records `samples_skipped_total{reason}` +
  a gap log and **advances the (monotonic) watermark past the offending bucket** — the loop always
  progresses. (The earlier "don't advance" was a permanent poison pill, fatal for `too-old`.) A
  `bad-encoding`/malformed 400 → skip + alert (a real bug; must not silently advance). §4.5.
- **F10 Partial OTLP accept (Cdx-C3/A1)** → per-bucket emit granularity (§4.5): accepted samples are
  durable, the rejected bucket is skipped-with-gap; never re-pull a partially-accepted bucket into a
  reject loop. Do not rely on all-or-nothing OTLP accept.

### HA / leader-election edge cases
- **F11 Split-brain / two leaders briefly** (lease renewal race, clock skew) → two replicas emit the
  same window. Safe **only because re-emission is value-identical** (§3.3 emit-once-after-settle +
  deterministic encoding): if the two leaders computed *different* values for an edge bucket (pod
  clock skew shifting which bucket is "settled"), Mimir would 400 (duplicate-timestamp). Mitigated by:
  (a) settle margin makes edge-bucket disagreement unlikely; (b) the F9 `duplicate-timestamp` handler
  drops rather than stalls; (c) **leader-epoch fencing** — check lease ownership immediately before
  `Save` (and ideally before `Emit`) so a demoted leader's in-flight work is dropped.
- **F12 Leader loses lease mid-Collect/mid-emit** → leaderCtx cancelled; in-flight work aborts;
  watermark not advanced; new leader re-pulls. Gap-free; no dupe (value-identical re-emit, §3.3).
- **F13 Leader crashes after emit, before watermark save** → new leader re-emits that window
  (at-least-once). Safe **because the re-emit is value/byte-identical** (settled bucket + deterministic
  encoding) → Mimir no-op / Loki dedup. (This is *conditional* idempotency — engineered in §3.3, not
  an inherent sink property: Mimir rejects value-*changed* `(series,ts)`; Loki dedup is byte-exact.)
- **F14 ConfigMap write conflict** (resource-version mismatch) → retry the read-modify-write; if a
  *non-leader* somehow writes, the lease should prevent it — assert leader-only writes.
- **F15 Lease API unavailable** (k8s API down) → cannot establish/renew leadership → no replica
  emits (fail-safe: better a visible staleness gap than split-brain). Surface loudly.

### Checkpoint failures
- **F16 No/unreadable checkpoint on startup (resolves OP3)** → distinguish: *confirmed-absent* →
  bootstrap from `now − bootstrap_lookback`; *exists-but-unreadable* → **refuse to start** (k8s) —
  bootstrapping over a real-but-unreadable watermark risks re-emitting stored buckets into
  duplicate-timestamp 400s (C1). (§4.4.)
- **F17 Watermark save fails repeatedly** → emission keeps succeeding but watermark stalls → on any
  restart, large re-pull. Surface `checkpoint_save_errors_total`; bound the re-pull by
  `max_backfill_window`.
- **F18 Corrupt watermark value** (file impl) → ignore-and-bootstrap with a loud warning (Alloy
  `ignore_invalid_yaml` pattern).

### Queue / backpressure
- **F19 Sustained sink outage** → queue fills → Collect blocks → `window_lag` climbs → staleness
  alert. On recovery, loops re-pull from the (un-advanced) watermark — bounded by `max_backfill_window`
  so we don't stampede the source API after a long outage.
- **F20 One slow loop starves others?** → no: **per-loop queue + per-loop emit worker** (§4.2, frozen
  from OP4). A loop blocking on its own full queue applies backpressure to *its own* Collect only;
  other loops are independent. No cross-loop head-of-line blocking.

### Config / lifecycle / security
- **F21 Missing/!set secret** → fatal at startup.
- **F22 Two loops emitting the same series (M7)** → a **series-name ownership registry** (one name →
  one loop) fails startup on overlap (§4.1), so two loops can never write the same `(series, ts)`
  (which would be a data-dependent duplicate-timestamp clash config-validation alone can't catch).
- **F23 Graceful shutdown (SIGTERM)** → stop accepting new ticks; `leaderCtx` is cancelled immediately
  (hard-cancel, not a drain-to-completion window) — in-flight collect/emit is aborted via ctx, queued
  batches are dropped, and the checkpoint commit path refuses any post-cancel `Save`. Nothing new
  persists after cancel; the grace window only bounds how long the process has before SIGKILL. Safe
  because re-emission after restart is deterministic and byte-identical (§3.3). **Lease-release
  ordering: see F35** (the lease is not released on cancel — it expires — so a standby can't acquire
  mid-shutdown; this is independent of whether anything actually finished emitting).
- **F24 Secret in logs** → forbidden; redact auth in all logging and self-telemetry; test for it.
- **F25 Backfill after long downtime (H3)** → `max_backfill_window` caps the re-pull, and is **≤ the
  Mimir accept window** (the stack's `out_of_order_time_window` — finite & per-tenant; e.g. 2h on
  Grafana Cloud, enforced on the distributor) — samples older than that are
  *unstorable* and are abandoned with `backfill_unstorable_total`, never retried into a 400 loop.
  Raising the Mimir OOO window (via Grafana Support) to cover the intended max downtime is a
  **documented deployment prerequisite**. Backfill walks **≤55m sub-windows** ascending (H5), each
  rate-limited (F19), so it neither flips bucket granularity nor stampedes the source.

### Time / correctness
- **F26 DST / timezone** → all time handling in UTC; ISO-8601 with `Z`.
- **F27 Bucket alignment + granularity flip (H5)** → pin windows to aligned boundaries (floor to the
  minute) so 1-min bucket timestamps are stable across polls; **and keep every window ≤55m** so the
  server never flips to hourly buckets (which would re-bucket an already-emitted minute into an hourly
  point at a different `(series, ts)` → contradictory/lost series). The emitter rejects + alerts on
  unexpected bucket spacing (`granularity_unexpected_total`).
- **F28 Monotonic clock vs wall clock** for cadence vs timestamps → use wall clock (UTC) for windows
  and bucket timestamps; ticker may use monotonic.
- **F29 Recomputed value re-emit → Mimir duplicate-timestamp 400 (C1)** → never re-emit a settled
  bucket; see F6/F9/§3.3. The headline correction: Mimir does **not** overwrite `(series,ts)` — a
  value-changed resend is rejected.
- **F30 Non-deterministic map encoding breaks Loki dedup / golden bytes (C2)** → sorted-key attribute
  encoding + canonical JSON bodies (§4.5); else re-emitted log lines differ byte-wise → duplicates.
- **F31 Out-of-order emit *completion* across workers advancing the watermark past an un-emitted batch
  (C3)** → per-loop single-flight emit + contiguous-successor `Save` (§4.2). No gap.
- **F32 `target_info` / job collision between self-obs and product telemetry (H4)** → distinct
  `service.namespace` for self-telemetry (§4.6).
- **F33 SSRF via `base_url` / signed-URL host; downloaded-payload content leak (H6)** → egress
  allow-list, default-deny metadata/RFC-1918; signed-URL host allow-list + downloaded-payload
  content assertion for the (deferred) logs loop (§4.7).
- **F34 `is_quota_exceeded:true` with a parseable truncated body (M2)** → discard the whole batch;
  never derive/emit from a quota-flagged response (§3.1).
- **F35 SIGTERM lease-release racing the standby mid-drain (M5)** → SIGTERM cancels the root ctx, which
  **hard-cancels** any in-flight collect/emit and the epoch-fenced commit (there is NO drain-to-completion
  and NO final watermark persist on the way out — the next leader re-pulls the partial window from the last
  committed watermark). We do **not** release the lease; it is left to expire so the standby waits the full
  LeaseDuration (avoids overlap). (Supersedes F23's "release promptly".)
- **F36 NTP step / large backward clock correction on the leader** → a backward wall-clock jump moves
  `now − settle` backward, making an already-emitted bucket "eligible" again → duplicate-timestamp.
  Mitigation: the watermark is monotonic (never emit `bucket_end ≤ watermark.Time`), so a backward
  clock jump cannot resurrect an emitted bucket; alert on detected backward jumps.

### Added from the Codex review (2026-06-18)
- **F37 New series added under an existing loop is silently skipped (Cdx-C4/A3)** → checkpoint key
  includes an output-schema fingerprint (§4.4), so enabling a new graph/metric creates a new key that
  bootstraps its own history instead of inheriting the loop's already-current watermark.
- **F38 Stale leader saves an older watermark → frontier moves backward (Cdx-C14/A2)** → `Save` is
  monotonic + lease-epoch-fenced (§4.3/§4.4); older/stale-epoch writes are rejected.
- **F39 Partial-accept poison pill (Cdx-C2/A1)** → resolved by advance-past + per-bucket granularity
  (F9/F10, §4.5). Never re-pull a partially-accepted bucket into a reject loop.
- **F40 Oversized export → 413 loop (Cdx-C12/A8)** → max bytes + max records per export, chunked
  deterministically (§4.5/§5); a 413 on a minimal chunk is a skip-with-gap, not an infinite retry.
- **F41 OTLP log labels don't map to Loki stream labels on the target stack (Cdx-C8/A6)** → resource
  attrs are not auto-promoted; the stack's OTLP-log config governs promotion (+ a label cap). Validate
  the target stack's mapping, or route logs via a collector that sets the label hints; do not assume
  `{workspace=…,env=…}` are stream labels. (Logs loop; OP5.)
- **F42 Post-gateway metric-name collision / double unit suffix (Cdx-C9/M4)** → the series-ownership
  registry validates the **normalized post-gateway** name (dots→`_`, unit suffixing, dropped
  non-dimensional units), not the raw configured name (§4.1).
- **F43 Missing zero buckets read as "poller down" (Cdx-H10)** → determine whether the API omits empty
  buckets; if so emit explicit zeros, or pair error-rate/no-traffic/SLO alerts with poller self-metrics
  so "no requests" ≠ "poller failed" (OP5).
- **F44 Catch-up starves live polling or never converges (Cdx-C13)** → explicit catch-up algorithm
  (§4.8): bounded sub-windows per tick, fairness vs live polling, rate-budget accounting, max
  catch-up duration, and an explicit abandon rule (ties to F25).
- **F45 Shared source rate budget exhausted by other clients/replicas (Cdx-A10)** → per-request
  self-throttle bounds *our* contribution only; the budget is shared (dashboards, Infinity, other jobs,
  a split-brain replica). Treat source 429 as a first-class backoff signal (F1/F8), size cadence
  conservatively, and document that we cannot unilaterally guarantee the source stays under quota.
- **F46 Self-telemetry fails with the sink it shares (Cdx-C10)** → product+self share an OTLP endpoint
  by default, so an endpoint/egress/token outage can blind the very lag metrics meant to detect it.
  Mitigation: support a separate self/meta endpoint (§4.6), recommend an independent path where
  feasible, and write staleness alerts as **absence-aware** (`absent_over_time`), documenting the
  residual risk when both share a sink.
- **F47 First-run + long-outage are explicit data-loss modes, not just backfill (Cdx-H8/A7)** → first
  install skips data older than `bootstrap_lookback`; an outage longer than the backend accept window
  loses the older portion (loud, counted). Surface these as a stated **retention/availability
  decision**, not an implied "always gap-free."

---

## 7. Open design points

**Resolved by the adversarial review (now frozen seams / decided):**
- **OP1 (F4) — RESOLVED:** all-or-nothing per window for *graphs*; *groups* get per-endpoint handling
  (see RP1). Do not advance the watermark past a bucket unless all configured graphs for it succeeded.
- **OP2 (F9) — RESOLVED (revised by Cdx-C2):** classify 400s (§4.5). `duplicate-timestamp`/`too-old`/
  `413` → **advance-past + counted gap** (`samples_skipped_total`), monotonically — *not* "don't
  advance" (that was a poison pill). `bad-encoding` → skip batch + alert. Loop always progresses.
- **OP3 (F16) — RESOLVED:** absent → bootstrap-lookback; unreadable → refuse-start (§4.4).
- **OP4 (F20/C3) — RESOLVED & frozen:** per-loop queue + one emit worker per loop (single-flight emit).
  The "shared pool keyed by loop" option is rejected (preserves dispatch, not completion, order).

**Remaining (resolve at PoC / planning) — expanded from the review:**
- **OP5** Confirm against the live API, before fixing schemas: (a) the `latency` graph shape
  (quantiles?); (b) **whether `groups` endpoints are time-bucketed or window-total, and their
  pagination/`page_size`** (H2 — the slice stays workspace-aggregate-graphs-only until proven);
  (c) **granularity-flip behaviour at the ~1h window boundary** (H5); (d) the **measured max
  late-arrival lag** that sets `bucket_settle` (C1) and the **Mimir accept window** on the target
  stack that caps `max_backfill_window` (H3); (e) **`timestamp` meaning** — bucket-start vs bucket-end
  vs sample-time, and timezone (Cdx-C11/A9): all the settle/emit logic assumes bucket-end; prove it and
  prefer server time; (f) **zero-bucket behaviour** — does the API omit empty buckets or return
  explicit zeros (Cdx-H10/F43); (g) **OTLP-log label promotion on the target stack** (Cdx-C8/A6 — for
  the logs loop). PoC list mirrors Codex's required tests (see §12).
- **RP1 (H2) — BUILT (2026-06-19; shape RESOLVED by the live groups PoC, then implemented as
  `internal/source/portkey/groups.go`).** Groups endpoints are **WINDOW-TOTAL, not
  time-bucketed** — each returns one aggregate row per dimension value over `[min,max]`; no `timestamp`,
  no `data_points`. Consequence: the entire graphs settle/watermark/bucket-revision machinery is
  **inapplicable**. Groups use a **stateless periodic-SNAPSHOT loop**: each poll derives a fresh
  Gauge per dimension value, timestamped at the window upper bound (a `settle`-offset trailing window,
  recomputed each tick); `Watermark.Time` is a monotonic heartbeat for staleness alerting only — no
  replay frontier, no Mimir duplicate-timestamp hazard. Per-endpoint independent: one endpoint failing
  does not block others. Within an endpoint: all-or-nothing across pages — on any page error discard
  partial set, retry whole window next poll. Pagination: `page_size` + `current_page` (0-indexed; `page`
  **ignored**; `total` drops to 0 past the end — stop when `data` is empty). Metric schema: distinct
  `_by_model` / `_by_metadata` Gauge names (e.g. `portkey_api_requests_by_model{ai_model}`) — **not**
  the unlabelled aggregate graph names (avoids labelled+unlabelled overload / M7 ValidateOwnership
  collision). `is_quota_exceeded` is optional in the response (absent on `ai-models`, present on
  `users`/`metadata/*` — treat absent as false). Two confirm-before-ship flags: **(i) cost data-field
  unit** (dollars vs cents — `cost=6619.19` for 3706 requests is ambiguous; resolve against the Portkey
  dashboard before naming the metric `_usd` or applying a divisor; ship `requests` first); **(ii)
  metadata-group row field name** (dev workspace has no tagged requests, so the exact JSON key for the
  dimension value in `metadata/{key}` rows is unobserved — confirm against a workspace that actually
  tags request metadata before freezing the derive path). Full spec: `docs/superpowers/specs/portkey-groups-poc.md`.
  **As built:** a separate `groupsLoop` (NOT a re-parameterised `analyticsLoop`) sharing the source's
  httpx client/limiter; `Window==0` so the scheduler treats it as a snapshot (no catch-up acceleration,
  no bucket-math, and — fix landed alongside — no spurious `backfill_unstorable` count, which also
  corrected the langsmith snapshot loop); vendor knobs (`window_span`/`settle`/`page_size`/
  `metadata_keys`/`emit_cost`/`max_groups`) ride the decoupled per-loop `settings` map (no
  `internal/config` change). Flag (i): cost is OFF by default and emitted unit-neutral (`_cost_by_*`).
  Flag (ii): tolerant lone-non-reserved-field decode tolerates the unconfirmed metadata field name.
- **RP2 (Cdx-C6) — BUILT (2026-06-20); lifecycle proven by the 2026-06-19 live PoC.** The `logs_export`
  loop ships in `internal/source/portkey` (logs_export.go state machine + logs_client.go lifecycle +
  logs_download.go streaming chunker + logs_strip.go field-allow-list + logs_settings.go decoupled
  knobs); guard policy wired in `internal/app` (allow-list `ai_org`/`ai_model`/`response_status_code`,
  content denylist backstop now includes `metadata`/`portkeyHeaders`); content-leak conformance gate
  Cdx-C7 lands as `TestLogsExportContentLeakConformanceGate` (drives Build→strip→guard→encode→/v1/logs
  and asserts injected PII/content never reaches the wire). **Delivery is at-least-once** for logs (not
  the metric plane's exactly-once gap-free): in-flight pages resume via the cursor, but a job failure /
  mid-window leader change restarts the window from page 0 and may re-emit a page (Loki tolerates dup
  operational records); a completed window is never re-pulled. **Still required before ship: GS1** — the
  stack-side Loki stream-label promotion of `ai_org`/`ai_model`/`response_status_code`;
  un-promoted, those land as structured metadata and `{label=…}`
  matches nothing. GS1 is a Grafana-staff action, not a code blocker.
  The export lifecycle is confirmed end-to-end: `create (draft)` → `start (queued)` → poll
  `in_progress` → `success` → `download` (signed AWS S3 URL, **6h expiry** `X-Amz-Expires=21600`) →
  resume/page. Key invariants proven: (a) **`requested_data` is not an egress allow-list** — `metadata`
  (PII: owner names, RITM #s, `data_classification:C3`) and `portkeyHeaders` are injected regardless;
  mandatory downstream field-allow-list strip required (Cdx-C7 release gate). (b) **50k-record split
  confirmed** (1,022,784-record match → exactly 50,000 lines delivered); cover larger windows with one
  export per `current_page`. (c) **No DELETE** — exports are permanent server-side records; "cleanup"
  = `cancel` on queued/in_progress only; terminal jobs persist. (d) **Signed URL re-call mints a fresh
  URL** — expiry recovery and idempotent resume are the same cheap operation. (e) **Control-plane vs
  in-VPC:** lifecycle API on `api.portkey.ai`; download on Portkey's signed-URL host (e.g.
  `signed-url-host.example.com`); a self-hosted Portkey changes both hosts —
  `signed_url_allow_hosts` allow-list required before fetch (DESIGN §4.7:271).
  The Loop/Collect variant encodes the job state machine in `Watermark.Cursor` (JSON: `phase`,
  `job_id`, `win_min/max`, `page`, `pages`, `total_records`) — one non-blocking step per tick, empty
  batch while mid-flight; `Watermark.Time` advances only when all pages of a window are emitted. No
  FROZEN seam change. Loki label-promotion list (feeds GS1): promote `ai_org`, `ai_model`,
  `response_status_code` as new stream labels; keep `trace_id` as structured metadata (high-card);
  never promote `id`, `created_at`, cost/token values; strip `metadata`/`portkeyHeaders`/content
  fields entirely. GS1 is a SHIP prerequisite (stream labels) for the logs loop, not a code-build
  blocker — the loop emits OTLP logs today; GS1 makes the indexed attrs queryable as Loki stream labels.
  Full spec: `docs/superpowers/specs/portkey-logs-export-poc.md`.

- **RP3 — Portkey `api_key_use_cases`: per-key use-case label (BUILT 2026-06-24).** Operators configure
  a source-level `api_key_use_cases` list mapping each use-case name to one or more Portkey api-key UUIDs.
  The integrator slugifies the label (lowercase, collapse each run of non-`[a-z0-9]` to a single `_`, trim; `"Data Gen"`→`data_gen`)
  and stamps it as an `api_key_use_case` metric label (analytics, groups) and log record attribute (logs).

  **Config shape** (`sources[].api_key_use_cases`, source-level, cross-loop):
  ```yaml
  api_key_use_cases:              # optional; empty ⇒ today's workspace-wide behaviour unchanged
    - label: "Data Gen"           # human label; slugified → data_gen (metric label / log record attr)
      api_key_ids: ["<uuid-a>"]   # Portkey api-key UUIDs (id field from GET /api-keys, NOT the secret)
    - label: "Content Gen"
      api_key_ids: ["<uuid-b>"]
  ```
  Fail-fast validations at construction: non-empty `api_key_ids` per entry; slug non-empty after
  normalisation; slugs unique across entries; UUIDs unique across entries (would double-count).
  **Mutual exclusion:** `api_key_use_cases` combined with an **enabled** loop's `settings.api_key_ids`
  is a config error (ambiguous double-filter). A disabled loop's `settings.api_key_ids` is ignored.

  **Architecture split — WHY metrics use one instance, logs fan out:**
  - `source.ValidateOwnership` (M7, `source.go`) forbids two `SeriesDeclarer` loops sharing a
    normalised series name under different checkpoint keys. A metrics fan-out (N analytics/groups
    instances, same metric names, distinct keys) would fail startup. Therefore **analytics and groups
    each use ONE loop instance issuing N internal filtered passes**: per pass, fetch with that
    use-case's `api_key_ids` filter, derive samples, stamp the slug; all passes accumulate into one
    `Batch` with one Key and one shared watermark. `Key()` is **unchanged** (`"prefix="+prefix`).
    Per-use-case `revisionHistory` instances (settle-exceedance detection) are keyed by slug so one
    key's late arrivals are not read as another key's bucket revision.
  - `logsExportLoop` is **not a `SeriesDeclarer`** (it emits OTLP logs, not metric series). `New`
    therefore builds **one `logsExportLoop` instance per use-case** (fan-out), each with its own
    `api_key_ids` filter in the `createExport` request body and its own cursor watermark. `Key()` folds
    the slug so each fan-out instance owns a distinct watermark. This is intentionally asymmetric with
    the metrics side.

  **Label tiers (the deliberate asymmetry):**
  - Metrics: `api_key_use_case` is a **Prometheus-style data-point label** on `Sample.Labels` (requires
    allow-listing in `portkey.AllowedLabelKeys()` and the Guard).
  - Logs: `api_key_use_case` is a **record-tier attribute** (`RecordAttributes`) — immediately queryable
    in Loki as `| api_key_use_case="data_gen"`, no GS1 stream-label promotion needed, and no Loki
    stream-label budget impact.

  **Unmapped-traffic scope (D5):** only the listed api-key UUIDs are collected. Traffic from keys NOT
  listed in `api_key_use_cases` is intentionally excluded — for both metrics (filtered passes) and logs
  (filtered export jobs). There is no "other" aggregate bucket. When `api_key_use_cases` is non-empty,
  the workspace-wide aggregate is replaced by the sum of the listed use-case slices (recoverable in
  PromQL via `sum by(...)` over the `api_key_use_case` label).

  **Migration note:** moving from `settings.api_key_ids` to `api_key_use_cases` — the analytics/groups
  metrics `Key()` is unchanged, so the checkpoint watermark is NOT reset and no data gap is introduced.
  However, the emitted series gain the `api_key_use_case` label: old unlabelled series stop and new
  labelled ones start — an expected, documented series-shape change (dashboards/recording rules must
  be updated to filter or aggregate by the new label).

### 7a. Cross-cutting design (Codex fold-in)
- **Catch-up algorithm (Cdx-C13/F44):** per tick a loop drains **up to `max_catchup_per_tick`** ≤55m
  sub-windows (bounded, not "all due" → won't starve live polling or burst the source), oldest-first,
  each rate-limited; live polling and backfill never overlap; windows past `max_backfill_window` are
  abandoned-loud. Steady state = one current window/tick.
- **Cardinality guard = allowlist + budget, not UUID-shape (Cdx-H6/H7):** the guard enforces a
  **config allowlist of label keys** + a **per-series-name cardinality budget**, emits a
  `new_label_values{series}` self-metric (early warning before a series explosion), and applies an
  **outbound field allow/denylist to *all* emitted fields** — labels (incl. `LogRecord.IndexedAttributes`,
  the indexed/cardinality-sensitive log tier) *and* `LogRecord.Body`/`RecordAttributes` — so
  content/identifiers (emails, study/doc ids, slugs, error strings) can't leave via a non-label field.
  There is no UUID-shape (or other value-pattern) detection anywhere in `internal/source` — an
  allow-listed key's UUID-valued labels are bounded only by the cardinality budget
  (`governance.per_metric_cardinality_budget`, default 10k distinct series admitted first per series
  name), not blocked outright.
- **Content-leak conformance is a RELEASE GATE for any logs/run-index feature (Cdx-C7/A5):** a test
  asserting forbidden fields are neither requested *nor present in the outbound OTLP/log payload* must
  gate release — not deferred — the moment a loop emits logs or run metadata. (Metrics-only v1 that
  never touches bodies is the only exemption.)
- **Health semantics (Cdx-H13):** `/readyz`/`/healthz` distinguish **process · leadership · scheduler
  liveness · per-source health · sink reachability** — a standby (no scheduler) is healthy; a leader
  blocked on intended backpressure is healthy; a single source's auth failure does not fail the pod
  (per-source alerting, Cdx-M6); a wedged scheduler loop is unhealthy (heartbeat).
- **Multi-source / one-process-per-scope (Cdx-H11):** the intended production pattern is **one process
  per environment/workspace**; where one process pulls multiple source instances, each gets its own
  resource identity, credentials, rate limiter, loop-id, checkpoint namespace, and series ownership
  (ties C4). State and validate this in config.
- **Enterprise networking (Cdx-H15):** `httpx` supports custom CA bundle, outbound proxy + no-proxy
  (exclude in-cluster collector), optional mTLS/extra headers, and PrivateLink endpoint overrides;
  NetworkPolicy/egress-allowlist are first-class deploy concerns.
- **Deploy hardening (Cdx-M10):** non-root, read-only rootfs, requests/limits, pod anti-affinity/
  topology-spread, NetworkPolicy, namespaced RBAC (leases+configmaps only), restricted secret mounts,
  image signing/SBOM/scan, termination grace ≥ emit-retry budget.
- **Self-metric retention mechanism (Cdx-M3):** "survive Adaptive Metrics" must be an actual provisioned
  + tested aggregation exemption on the target stack, not a naming convention — or it's not a guarantee.
- **Determinism is broader than sorted maps (Cdx-H14):** also normalise source response/pagination
  order, float formatting, timestamp precision, omitted-empty fields, and OTLP resource/scope grouping
  order; the determinism test runs at **full export-payload** level on realistic multi-resource batches.
- **`MetricKind` for Sum (Cdx-M2):** add temporality + monotonicity fields to the model before any Sum
  metric ships (v1 gauges don't need them, but the seam must be right).
- **Dashboard contracts (Cdx-H9):** ship recording rules / example queries that make the per-bucket
  gauge semantics hard to misuse (no naive `rate()`).

---

## 8. Test plan (strict TDD)
- **Unit / table-driven** per source: feed captured real response bodies (the `{summary,data_points,
  is_quota_exceeded}` shapes; window-adaptive bucket fixtures; a `feedback_stats` facet fixture for
  LangSmith later) → assert derived `Sample`/`LogRecord` sets.
- **`httptest` fakes** of each API: success, 401/403, 429, 5xx, timeout, `is_quota_exceeded`,
  schema-drift, empty window. Assert F1–F6 behaviours.
- **Watermark/forward-only & settle:** overlapping windows emit each bucket once; restart resumes from
  watermark; `bucket_settle` excludes open buckets; a **changed settled bucket is NOT re-emitted** and
  bumps `bucket_revised_after_settle_total` (C1/F6); a backward clock step never resurrects an emitted
  bucket (F36); window clamped ≤55m and a wrong bucket-step is rejected (H5/F27).
- **Emitter determinism (C2/Cdx-H14):** encode the same batch twice with shuffled map order →
  **byte-identical** output, asserted at **full export-payload level** (not just top-level maps —
  also pagination order, float formatting, omitted-empty fields, resource/scope grouping). This is a
  **comparative** determinism check (both shuffled encodes run in the same process/version), which
  proves within-version idempotency — sufficient for the Mimir `(series,ts,value)` contract — but does
  NOT pin byte stability *across* a version upgrade (no golden-file/checked-in-payload test exists); a
  re-emit spanning an encoder-changing release could produce a value-identical but byte-different
  payload undetected. **Rejection model (OP2/Cdx-C2):** `duplicate-timestamp`/`too-old`/`413` → **advance-past
  + `samples_skipped_total`** (monotonic, no retry loop, loop progresses — assert no poison-pill stall);
  `bad-encoding` → skip + alert; 500 retried next cadence not inline; retry on 429/502/503/504; auth redacted.
- **Codex-required tests (Cdx §12):** fake gateway that **accepts some + rejects one** sample, assert
  per-bucket advance-with-gap (Cdx-A1); **stale-leader** save after a new leader advanced → rejected by
  monotonic+epoch (Cdx-A2); **new series under an existing loop** → new checkpoint key bootstraps its
  history, not skipped (Cdx-A3); **source-instance / schema-fingerprint change** vs existing checkpoints;
  **oversized payload** split by bytes + record count, 413-on-min-chunk = skip-with-gap (Cdx-A8);
  **content-leak conformance on the outbound payload** (not just request selectors) for any logs/run
  feature (Cdx-C7); **cardinality guard with non-UUID** high-card values (Cdx-H6); **soak** with source
  429/quota + a Grafana outage longer than the accept window (Cdx-A7/A10).
- **Coordinator:** noop always-leader; lease failover (envtest or fake clientset) — single emitter,
  re-pull on takeover; **leader-epoch fence drops a demoted leader's in-flight Save/Emit (F11)**.
- **Checkpointer:** single-writer + throttle; configmap RMW + conflict retry; **absent → bootstrap vs
  unreadable → refuse-start (OP3/F16)**; file atomic-rename + corrupt-file fallback.
- **Queue/backpressure & ordering:** full queue blocks Collect (F19); one slow loop doesn't starve
  others (F20); **out-of-order emit completion never advances the watermark past an un-emitted batch
  (C3/F31)** — per-loop single-flight emit.
- **Backfill bounds (H3/F25):** re-pull capped to the Mimir accept window; walks ≤55m sub-windows;
  older-than-window abandoned with `backfill_unstorable_total`, never a 400 loop.
- **Self-obs identity (H4):** self-telemetry resource identity is distinct from product (no
  `target_info`/job collision).
- **Content-minimisation (G4):** no request ever carries `request`/`response`/`inputs`/`outputs`/
  `events` selectors; no content attribute ever emitted (F10/FR10).
- **Conditional idempotency (corrected):** re-emitting the *same settled window* yields value/byte-
  identical output (Mimir no-op, Loki dedup); a *value-changed* re-emit is the explicitly-handled F29
  path, **not** an assumed overwrite. (Replaces the old "Mimir overwrites" test premise.)

## 9. Acceptance criteria (v1)
- Portkey `analytics` runs against the live (or faithfully faked) API, emits `portkey_api_*` gauges to
  a Grafana Cloud stack via OTLP, with bounded labels and correct forward-only timestamps.
- Kill the leader → a standby resumes (recovery latency bounded by `LeaseDuration + RetryPeriod +
  one-collect-cycle`, H1) with **no data gap within source retention + the sink accept window** and no
  duplicate series (older/rejected data = loud counted loss, not silently gap-free).
- Sustained sink outage → `window_lag`/staleness visible; recovery backfills (bounded to the Mimir
  accept window), no loss within that window; older abandoned loudly.
- All §8 tests green; full build+vet+lint clean; no secret in any log/telemetry.

## 10. Risks
- Live-API drift vs captured shapes (mitigated by fakes + schema-tolerant derivation, F5).
- WAF UA-block / egress allow-listing in a customer env (config UA; document allow-list need).
- **`bucket_settle` is a measured value, not a guess (C1)** — measured ~185s on a live workspace
  (2026-06-19); default 10m for headroom; actively detected via `bucket_revised_after_settle_total`
  (alert on `rate(...) > 0` to bump if load grows).
- Mimir accept-window vs intended max downtime (H3) — may require raising the stack's OOO window.

## 11. Adversarial review outcome (2026-06-18, Opus, independent fresh context)

**Verdict:** skeleton sound; one false premise ("idempotent sinks ⇒ at-least-once is free") propagated
into the headline guarantees and is now corrected. **Must-fix seams (folded in):**
- **C1** — Mimir rejects value-changed `(series,ts)`; Loki dedup is byte-exact. ⇒ emit-once-after-(measured)-settle, never re-emit a changed bucket, observe drift (§3.3, F6/F29).
- **C2** — deterministic (sorted-key / canonical-JSON) encoding so re-emission is byte-identical (§4.5, F30).
- **C3 / OP4** — per-loop queue + one emit worker per loop; watermark = contiguous successor (§4.2, F31).
- **OP2** — classify emit 400s (`duplicate-timestamp`/`too-old` vs `bad-encoding`) (§4.5, F9).
- **H3** — `max_backfill_window` ≤ Mimir accept window; abandon older loudly; raising OOO window = deploy prereq (F25).
- **H5** — clamp windows ≤55m; backfill walks sub-windows; guard on bucket spacing (§3.1, F27).
- **H4** — distinct self-telemetry resource identity (§4.6, F32).

**Deferred (don't block the graphs-only v1 slice, scheduled):** H2/RP1 (groups shape/pagination —
slice is graphs-only until PoC), H6/F33 (SSRF + signed-URL — with the deferred logs loop), and the
M-items (M1 single-writer/throttle, M3 window validation, M4 self-metric ownership, M5/F35 shutdown
ordering, M6 per-request rate token, M7 series-name ownership) — all written into §3–§6 and to be
covered by tests during the slice build. L-items are implementation hygiene.

## 12. Codex review disposition (2026-06-18, independent review)

A second, independent adversarial review (Codex). It corroborated the Opus C1 finding and added the
poison-pill, checkpoint-identity, and fencing issues. **Every valid finding is resolved or explicitly
dispositioned below, tagged `Cdx-*` in the body so Codex can re-verify the fix.** Status legend:
✅ resolved · ✅(scoped) resolved within v1 scope · ↩ deferred-with-plan (logs/groups loop) ·
◻ accepted-by-design (rationale).

| ID | Finding (short) | Status | Resolution & location |
|---|---|---|---|
| Cdx-C1 | "gap-free/duplicate-free" overstated | ✅ | Conditional idempotency engineered; language tightened. §9, §3.3, ARCH §3, README |
| Cdx-C2 | 400 "don't advance" = poison pill | ✅ | Advance-past + `samples_skipped_total`, monotonic. §4.5, F9, OP2 |
| Cdx-C3 | partial-accept granularity | ✅ | Per-bucket emit granularity; advance-with-gap. §4.5, F10/F39 |
| Cdx-C4 | `loopID` checkpoint identity too weak | ✅ | Key = `source_instance/loop/schema-fingerprint`. §4.4, F37 |
| Cdx-C5 | grouped analytics not prod-designed | ✅ | PoC-proven, then BUILT as the snapshot `groupsLoop`. OP5b, RP1 |
| Cdx-C6 | source iface too pure for logs lifecycle | ↩ | Export-lifecycle variant spec'd for logs loop. RP2 |
| Cdx-C7 | data-min ≠ full content boundary | ✅+↩ | Outbound allow/denylist + conformance **release gate** when logs ship. §7a, followup §1/§7 |
| Cdx-C8 | OTLP→Loki label mapping assumed | ✅ | Validate target stack / route via collector. F41, OP5g, §7a |
| Cdx-C9 | post-gateway name collision | ✅ | Ownership validates normalized post-gateway name. §4.1, F42 |
| Cdx-C10 | self-o11y fails with its sink | ✅ | Separate self endpoint + absence-aware alerts + stated residual risk. §4.6, F46 |
| Cdx-C11 | timestamp semantics under-proven | ✅(PoC) | Prove start/end + prefer server time + skew margin. OP5e |
| Cdx-C12 | queue bounded by count not bytes | ✅ | Max bytes/records + chunking + streaming. §4.5, §5, F40 |
| Cdx-C13 | catch-up scheduling underspecified | ✅ | Catch-up algorithm (bounded/fair/abandon rule). §7a, F44 |
| Cdx-C14 | Lease ≠ write fence | ✅ | Monotonic + epoch-fenced `Save`. §4.3, §4.4, F38 |
| Cdx-H1 | Alloy-centred vs direct divergence | ✅ | Standalone vs regulated mode. followup §7 |
| Cdx-H2 | README "settled" overclaim | ✅ | Softened to v1-spine-settled. README |
| Cdx-H3 | LangSmith dev→prod conflation | ✅ | Kept as PoC/deploy gate, not prod guarantee. ARCH §15 |
| Cdx-H4 | no capability detection | ✅ | Startup capability detection (6 states). F5 |
| Cdx-H5 | ConfigMap fragile hot path | ✅ | Monotonic+epoch+observable; external store noted. §4.4, followup §2 |
| Cdx-H6 | cardinality guard UUID-only too weak | ✅ | Allowlist + cardinality budget + new-values metric. §7a |
| Cdx-H7 | field-is-policy doesn't gate body | ✅ | Outbound allow/denylist on **all** fields. §7a |
| Cdx-H8 | first-run/long-outage = data loss | ✅ | Framed as explicit retention/availability decision. F47 |
| Cdx-H9 | metric semantics misuse risk | ✅ | Ship recording rules / example queries. §7a |
| Cdx-H10 | missing zero buckets | ✅(PoC) | Prove omit-vs-zero; emit zeros or pair w/ self-metrics. OP5f, F43 |
| Cdx-H11 | multi-source/workspace unmodeled | ✅ | One-process-per-scope + per-instance identity. §7a |
| Cdx-H12 | not retrying 500 framed as obvious | ✅ | Conscious tradeoff; counter, no advance, retry next cadence. §4.5 |
| Cdx-H13 | health/readiness HA semantics | ✅ | Distinguish process/leadership/scheduler/source/sink. §7a |
| Cdx-H14 | determinism broader than maps | ✅ | Full-payload determinism test. §7a, §8 |
| Cdx-H15 | enterprise network unmodeled | ✅ | CA/proxy/mTLS/PrivateLink/NetworkPolicy in httpx+deploy. §7a |
| Cdx-M1 | "slow sink never stalls" contradiction | ✅ | Corrected wording (transient vs sustained). ARCH §3 |
| Cdx-M2 | `MetricKind` incomplete for Sum | ✅ | Add temporality/monotonicity before any Sum. §7a |
| Cdx-M3 | "survive Adaptive Metrics" not actionable | ✅ | Must be a provisioned+tested exemption. §7a |
| Cdx-M4 | series ownership too broad/narrow | ✅ | Final normalized series identity. §4.1, F42 |
| Cdx-M5 | file vs configmap semantics differ | ✅ | Discourage file checkpoint for HA prod. §4.4 |
| Cdx-M6 | source-auth needs deployment alerting | ✅ | Per-source health alerts, not pod readiness. §7a, F2 |
| Cdx-M7 | repo layout aspirational | ✅ | "No code yet / target shape" status. README, ARCH §13 |
| Cdx-M8 | spec not tracked / docs contradict dispositions | ✅ | **This spec promoted to tracked `docs/DESIGN.md`** (round 2); `ARCHITECTURE.md` interfaces + prose corrected to match (CheckpointKey/Epoch, conditional gap-free, guard, label-mapping, config). Plans only stay gitignored. |
| Cdx-M9 | provenance present + external link | ◻ | Internal repo; provenance stripped before handoff; link fragility noted |
| Cdx-M10 | no image/runtime hardening | ✅ | Deploy-hardening checklist. §7a |

**Attack scenarios** A1→C2/C3/F39 · A2→C14/F38 · A3→C4/F37 · A4→C5/RP1 · A5→C7/H7/§7a · A6→C8/F41 ·
A7→H8/F25/F47 · A8→C12/F40 · A9→C11/F6/F36 · A10→F45 — all covered above. **Document mismatches** map
to H2/M1/H1/H3/C7/M8 (resolved/accepted). **Codex's 14 required tests** are adopted in §8 + OP5.

---

## 15. OP5 measured — live Portkey API (2026-06-19)

GET-only probe with the dev `portkey_apikey` across four windows (50m/59m/61m/24h) × six graphs; all
**200**. Record: `docs/superpowers/poc/OP5-findings.md` (scratch). These are the measured facts the
Portkey source encodes; they **supersede** the corresponding assumptions in §3.1/§3.2/§3.3.

- **OP5a — latency shape (schema change).** The `latency` graph is structurally different from the
  count graphs: summary `{avg,p50,p90,p99}`, data points `{timestamp,avg,p50,p90,p99}` (**no `total`**).
  → `portkey_api_latency_seconds` emits **one gauge per percentile** with `{quantile="p50|p90|p99"}`
  (dedicated derive path); requests/cost/tokens/errors/users use the generic `{timestamp,total}` path.
  **Guard knock-on:** the deny-all-default guard must allow `quantile` in the composition root (Task 15).
  **Unit = ms, confirmed** by live soak (a1d140d, p99 ~155633 ms); derive divides by 1000. (The zero-traffic dev probe OP5d could not reveal it; the live soak did.)
- **OP5c — granularity.** ≤59m → 1-min; ~61m → 10-min; 24h → 1-hour. Flip is between 59m and 61m. The
  ≤55m window clamp (H5) is safely inside the 1-min regime.
- **OP5e — timestamps are bucket-START**, and the in-progress bucket is included → `startSemantics=true`,
  `bucket_end = timestamp + granularity`; the settle gate excludes the in-progress bucket.
- **OP5f — empty buckets are explicit `total: 0`, never omitted** → emit 0-valued gauges; "zero traffic"
  is distinguishable from "poller down" from the data alone.
- **OP5d — `bucket_settle` measured on a live workspace (2026-06-19).** Max post-close late-arrival
  lag observed: **~185s**. Default raised to **10m** for headroom. Settle-exceedance is now **actively
  detected**: `genai_otel_bridge_bucket_revised_after_settle_total{loop}` fires when a settled bucket changes —
  alert on `rate(...) > 0` to signal that `bucket_settle` should be bumped if load grows (followup.md §8).
  The **Mimir accept window** (caps `max_backfill`) is a stack-config lookup → GS2 deploy prereq, not a
  Portkey probe.
