# Follow-up & deferred topics

Things deliberately **out of scope** of the committed build but captured so they are not lost. None of
these block it; each is a discrete piece of work. The **open code** follow-ups are triaged into priority
streams immediately below; the detailed §-sections that follow are the reference (and also hold the many
items already DONE/RESOLVED/BUILT).

---

## Open code follow-ups — prioritised work plan (v1 / v2 / v3 / vX)

**Triaged 2026-06-22.** Streams: **v1** = do before/at first real deployment; **v2** = next iteration
(post-v1 / productization); **v3** = later, large, or conditional; **vX** = blocked on an external API
surface, revisit only when that surface changes. Grafana-staff (stack-side) actions are NOT here — see `deploy/grafana/README.md`. **Cx**/**Risk** are for the code change itself (Low/Med/High).
Credential-validation (against a live test instance, content-safe, 2026-06-22) is folded into the rows
that depended on it (items 3, 4, 7, 10).

### v1 — before first real deployment

| Item | What / impact | Cx | Risk | Ref |
|---|---|---|---|---|
| ✅ **DONE 2026-06-22 (`699b48c`) — Per-prompt metrics** (Portkey `groups/prompt`) | Pre-aggregated per-prompt request series (`{prompt}` label), mirroring the `ai-models` groups loop. `emit_prompts` (default ON; opt out via `emit_prompts:false`) → `portkey_api_requests_by_prompt{prompt}`. **LIVE-CONFIRMED 2026-06-22:** the `prompt` groupBy dimension returns 200; a live test instance populates **a small number of distinct prompts over a high request volume** — the low cardinality confirms the value is a saved-prompt **identifier**, not rendered prompt text → content-free, guard-allow-listed. No `cost` on this dimension (request volume only). | Med | Low | §9 S12 |
| ✅ **DONE 2026-06-22 (`e7d2687`) — logs_export auth-error instrumentation** | `auth_errors_total` was wired on the 4 GET loops but NOT the `logs_export` job lifecycle (it returns a string error, not a code). Fires `OnAuthError(l.Key().Loop, sourceInstance)` from the `lifecycle()` chokepoint on 401/403 via `source.IsAuthStatus`. (LangSmith `runs` was already instrumented — gap was logs_export only.) | Low | Low | §9 |
| ✅ **DONE 2026-06-24 (`7e2f54b`/`16f136e`) — Bucket-revision lateness histogram** | `OnBucketRevised(loop, age)` now carries `age = now−bucketEnd`; ships `genai_otel_bridge_bucket_revised_after_settle_age_seconds` (+ dashboard p50/p95 panel) alongside the count, so `bucket_settle` is tunable to p95-of-age rather than guessed. Metrics CAN'T be backfilled (Mimir rejects a changed value at a settled `(series,ts)`) ⇒ settle is the only lever. | Low | Low | §11 |
| **Tune `bucket_settle` from the age histogram** (post-deploy) | Once a binary with the histogram is deployed + ~days of data, read p95 of `bucket_revised_after_settle_age_seconds`; raise `bucket_settle` to it if it materially exceeds 10m, else confirm 10m is fine. A clean fixed-window probe (2026-06-24) suggested genuine settling ~3m (10m likely already generous), but the in-product revised COUNT is bursty — measure before changing. Config-only. | Low | Low | §11 |

### v2 — next iteration / productization

| Item | What / impact | Cx | Risk | Ref |
|---|---|---|---|---|
| **LangSmith bulk-export → S3 (Parquet)** | Batch pipeline for full/regulated records + long-range history into customer-owned S3 (the regulated-records path the content-free poller can't serve). Needs a more-privileged key. | High | Low | §4 |
| **Self-APM: emit-pipeline span (`loop.emit`)** | The emit POST uses a plain `http.Client`, not `httpx`, so emit latency is in NO histogram (only `emit_errors_total`) — the one real self-obs blind spot. Needs trace ctx threaded through the bounded queue into the worker goroutine. | Med–High | Med | §10 |
| **Self-APM: other spans** | `loop.commit` (checkpoint write/fence), logs_export per-step lifecycle, httpx request spans under the tick, election span. Enrichment over the existing logs + counters. | Low–Med | Low–Med | §10 |
| **`source_capability` 6-state metric** | Richer attribution (endpoint-absent / plan-unsupported / permission-denied / no-data / transient-404 / schema-changed) over today's auth-error + graph-unavailable counters. Error coverage already exists in logs+counters → this is enrichment. | Med | Low | §8/§9 |
| **Portkey S3-side log ingestion (Option A)** | New logs source mode: bucket-notification → download, for operator-owned WORM S3. Only if an operator deployment uses Option A. | High | Low–Med | §9 |
| **httpx SSRF: proxy + DNS-rebinding residual** | The direct dial path is already fully guarded (confirmed 2026-06-22); this only closes the proxy-path rebinding race, reachable only when an HTTP(S)_PROXY is configured. Pin resolution / push egress policy to the proxy. | Med | Med | §8 |

### v3 — later / large / conditional

| Item | What / impact | Cx | Risk | Ref |
|---|---|---|---|---|
| **More gateway / eval-platform vendors** | Each new vendor = a new `source/<vendor>` package behind the frozen interface. | Med / vendor | Low | §4 |
| **LangSmith OAuth auth model** | Only if a deployment mandates OAuth sessions (the built path uses static service keys). Adds token-refresh handling on the critical-path poller. | Med | Med | §9 |
| **Plugin runtime / dynamic SPI** | Out-of-tree / dynamic source loading so third parties add sources without touching core. Only on concrete demand. | Very High | High | §5 |

### vX — blocked on Portkey API surface (revisit only when it changes)

| Item | What / impact | Cx | Risk | Ref |
|---|---|---|---|---|
| **Portkey multi-workspace targeting + auto-discovery** | Emit all workspaces under one admin key + TTL-refresh discovery. The safety guardrail (`expected_workspace`) is BUILT; the fan-out is **blocked**: Portkey IGNORES per-request workspace targeting on `/analytics/groups/*` (scope is key-bound), so multi-workspace stays key-per-workspace. **Surface to re-check** (an agent can probe in minutes): does `/analytics/groups/*` RESPECT the `x-portkey-workspace` header or a `workspace_id` query param — i.e. does sending it CHANGE the returned row set? Today: **no** (re-confirmed 2026-06-22). When that flips to yes, the fan-out becomes buildable. | Med–High | Med | §4 |

---

## 0. ✅ DONE — Hard 1DPM emit cap shipped

**Status:** implemented and green. Enforcement points:
- **Product plane:** `emit.CoalesceDPM` in `schedule.ProcessBatch` — stateless per-(series,minute) LWW
  coalesce before `splitByBucket`; suppressions counted via `genai_otel_bridge_samples_capped_total{loop,reason="dpm"}`.
- **Self plane:** `selfobs.NewProvider` clamps the OTel-Go PeriodicReader interval to `60s/max_dpm` — the
  SDK is structurally 1-point-per-interval, so the clamp is sufficient and stateless.
- **Config knob:** `governance.max_dpm` (int, default 1). Decision ledger: ARCHITECTURE.md §16 #13.

**Why stateless intra-batch is sufficient.** Cross-batch / cross-loop deduplication is already covered by
the existing stack (emit-once-after-settle + monotonic forward-only watermark + `ValidateOwnership` +
Mimir duplicate-timestamp rejection). Adding a cross-batch seen-set would add mutable state to the gap-free
path with no correctness gain; declined (recorded in ledger #13).

~~**Requirement.** Every series we export must be hard-capped at **≤ 1 data point per minute (1DPM)** —
the Grafana Cloud default. Make the cap a **configurable global rate** (default 1DPM), enforced on the
**emitter side**, applied uniformly to **both planes** (product `emit.telemetry` and self
`emit.self`). We must **not rely on polling/bucket mechanics to *imply* 1DPM** — it has to be enforced.~~

## 1. Data-sensitivity / regulated-content governance (parked)

v1 keeps a single, cheap habit — **request only the fields we need** (never select
`request`/`response` from a gateway, never select `inputs`/`outputs`/`events` from an eval
platform) — framed purely as **data minimisation** (less data = faster, cheaper, simpler),
not as a compliance control. The full governance apparatus below is **deferred** until a
regulated customer needs it.

When required, layer in (defence-in-depth):

1. **Source-side exclusion** (already the v1 default): field selectors omit content at the API.
2. **Collector-side strip**: an OTLP `transform` stage that deletes any content attribute
   (`gen_ai.prompt*`, `gen_ai.completion*`, `gen_ai.input.messages`, `gen_ai.output.messages`,
   `gen_ai.system_instructions`, OpenInference `input.value`/`output.value`) before export —
   independent of source behaviour, so a config drift can't leak content.
3. **Conformance gate**: a CI/runtime test that fails the deploy if a content attribute is
   detected egressing, and asserts the source API requests never carry body-field selectors.
4. **Records / residency framing**: regulated records-integrity rationale —
   operational telemetry (latency/tokens/cost/errors) is non-record; inference content is a
   regulated record that belongs in a customer-owned validated immutable store, never in the
   observability backend. Grafana/Mimir/Loki hold operational metadata only.

**✅ RESOLVED (2026-06-22): no LLM content (prompts/responses) goes to Grafana Cloud — ever.** This
closes the previously-open content-filtering POLICY question: content-free is the final
posture, **do NOT build a content-emitting mode**. This is the safe-direction resolution of what was
deferred — (a) operational fields to promote are governed per-loop by `extra_record_fields` /
`extra_indexed_fields` (opt-in, content-free only); (b) run content does not belong in Loki under this
posture; (c) so no audited content opt-in mechanism is needed. If full records are ever required (regulated
regulated-records path), they go to a **customer-owned** S3/WORM store (§4 bulk-export), never to GC.

No code change required — content-free is already the **enforced default**, verified 2026-06-22 across
five layers: (1) the never-subtractable floor `source.AbsoluteNeverDenyKeys` (`internal/source/content.go`
— `gen_ai.*`, `request`/`response`, `inputs`/`outputs`/`messages`, `input.value`/`output.value`, plus
Portkey-injected `metadata`/`portkeyHeaders`); (2) per-source hard-denied sets (langsmith
`inputs_s3_urls`/`outputs_s3_urls`, portkey bare `prompt`); (3) default-deny strips on both logs loops
(`defaultLogFieldPolicy` allow-lists only content-free operational fields; `Body` never set); (4) the
default-deny Guard backstop + denylist; (5) opting content IN via
`extra_record_fields`/`extra_indexed_fields`/`allow_label_keys` is **rejected fail-fast at config load**.
Enforced by `make gate`: `TestLogsExportContentLeakConformanceGate` + `TestLangsmithRunsContentLeakConformanceGate`
(end-to-end Build→strip→guard→encode→emit; both verified to fail on a real leak).

The live probe also proved LangSmith's `select` is NOT an egress filter on 0.13.5 (the full field set
returns regardless), so the client-side strip — not the projection — is the load-bearing control.

**✅ BUILT 2026-06-20 — configurable content-governance (default-on guards, customer opt-in).** The
field allow/deny apparatus is now config-extendable:
- The app `contentDenylist()` was SHRUNK to the **absolute-never message bodies** only (`gen_ai.*`,
  `input.value`/`output.value`, `request`/`response`, `inputs`/`outputs`, `messages`, plus the
  Portkey-injected `metadata`/`portkeyHeaders`). The "gray" fields (`error`/`events`/`extra`/
  `serialized`/`*_preview`/`s3_urls`/`manifest`/`name`/`tags`/…) are no longer hard-denied — they are
  governed by the per-loop default-deny strip, so a customer CAN opt them in without the guard silently
  eating the whole record.
- The langsmith runs strip gained `settings.extra_record_fields` (csv) → appends to the strip's RECORD
  (structured-metadata) allow-list; the `select` projection mirrors it. The strip also gained
  array→csv (a scalar array under an allow-listed key joins with `,`; objects/nested/null still dropped),
  so operational arrays (`tags`/`child_run_ids`/`parent_run_ids`) are opt-in-able. Opting in a
  hard-denied body (`inputs`/`outputs`/`messages`/`request`/`response`/`metadata`) is rejected at config
  time (loud), not silently dropped.

**indexed (Loki stream-label) opt-in — part (a) ✅ BUILT 2026-06-20, part (b) still deferred.**
(a) ✅ The composition-root guard `AllowLabelKeys` is now **config-driven**: each enabled source declares
its content-free keys (`portkey.AllowedLabelKeys()` / `langsmith.AllowedLabelKeys()` — no vendor strings
in `app.go` anymore), and the operator opts EXTRA content-free keys in additively via
`governance.allow_label_keys` (default empty). A floor key (message body / injected PII) in the opt-in is
rejected fail-fast at `Build`. The `values.yaml` field doc-comment documents the GS1 LIMITATION: a key is
allowed past the guard but only becomes a **queryable Loki stream label** if it is in the Grafana OTel
gateway's default label config; any label not in that default needs a support ticket (GS1) — until then
it lands as structured metadata. (b) ✅ **BUILT 2026-06-20:** a per-loop `settings.extra_indexed_fields`
(csv) on both logs loops promotes a content-free field into the INDEXED tier (`withExtraIndexedFields` on
each strip; auto-allow-listed in the guard via `optedInIndexedFields` so a promotion can't be silently
dropped; for portkey it's also merged into `requested_data`). Gated more carefully than the record opt-in:
hard-denied content/floor fields are rejected fail-fast (vendor + app backstop), a startup WARN flags the
Loki-stream cardinality blast (the per-loop budget is the runtime backstop). Still GS1-gated to be
queryable as `{label=…}`. Conformance: `TestLangsmithRunsExtraIndexedFieldFlows` proves a promoted field
flows end-to-end without dropping the record + the content floor holds.

**✅ BUILT 2026-06-20 — same `extra_record_fields` pattern for portkey `logs_export`.** Mirrored the
langsmith knob into `source/portkey/logs_strip.go`/`logs_settings.go`: `extra_record_fields` (csv)
appends content-free fields to the strip RECORD allow-list and to `requested_data`; opting in a
hard-denied field (`hardDeniedLogFields` = shared floor + Portkey content fields incl. bare `prompt`) is
rejected fail-fast. Also fixed a latent null→`""` bug in the portkey strip (the same guard langsmith
got). Deliberate asymmetry: portkey does NOT get array→csv (no Portkey scalar-array field needs it;
arrays stay dropped, defensive). The `TestLogsExportContentLeakConformanceGate` release gate stays green.

## 2. Additional emit backends

**OTLP is the committed, sole destination wire format — we do not plan to change backends.** The
`Emitter` interface is retained only as a clean test/queue seam (and a theoretical escape hatch), not
because alternative emitters are roadmapped. The in-process `model` stays a neutral *domain* type for
source-author ergonomics, the guard chokepoint, deterministic hand-encoding, and proto-version
decoupling — **not** for backend-swapping (see ARCHITECTURE §16 #3/#11).

- ~~**Prometheus remote_write + Loki push**~~ — **not planned.** Would give exact control over series
  names/labels/timestamps, but OTLP (→ Mimir/Loki via the Grafana Cloud gateway or a collector) is
  committed. Revisit only if a hard, unworkaroundable OTLP→Mimir transform limitation ever forces it.
**Durability under a downstream (Grafana Cloud) outage.** v1 default = in-memory queue +
replay-from-watermark (covers crashes, failover, transient outages — the source is replayable). For
stronger durability the **recommended topology is to front `genai-otel-bridge` with a persistent-queue Alloy**
(`otelcol.storage.file` on a PVC, `block_on_overflow=true`, `num_consumers=1` for metrics) — zero
`genai-otel-bridge` code, and better than hand-rolling a spool.
A built-in **S3 dead-letter spool** stays a deferred fallback, justified only by the one niche where it
beats Alloy — holding data for hours/days to re-emit on demand. **Crucial caveat for both:** for
*metrics*, no local buffer beats the backend's out-of-order / too-old accept window — surviving a long
outage means **widening that window** (Grafana Support), not buffering. For *logs* the buffer also
avoids an expensive re-fetch (re-running a Portkey export job).
The per-plane backfill defaults are now sized to the real Grafana Cloud ceilings (both per-tenant
overridable): **metrics `max_backfill` 90m** (≤ Mimir `out_of_order_time_window` 2h, leaving ~30m margin
for clock skew + the catch-up walk) and **logs loops `max_backfill` 24h** (≤ Loki
`reject_old_samples_max_age` 168h/7d; operator may raise toward 7d). The too-old honesty path is BUILT
and now regression-tested for BOTH planes: pre-emptive `backfill_unstorable_total` (watermark older than
`now-max_backfill` skipped loud) + reactive `ReasonTooOld` advance-past for Mimir AND Loki
age/order-reject strings. The persistent-queue-Alloy topology recommendation is unchanged.

**Config-surfacing review polish (2026-06-20) — ✅ ALL RESOLVED.** The three Minor items the final
review raised are now done: (1) `defaultSessionsSettings()` extracted so `New` + `ExampleSource()` share
one source of truth for the langsmith *sessions* defaults (no more hand-duplicated literals; chart output
byte-identical); (2) the `helmgen` comment-aware render path collapsed into `renderValueStruct`/
`renderValueField` via a nil-able `comments` param (~100 net lines removed; byte-identity held by the
drift gate); (3) `compactDuration` added so config.Duration FIELDS render `1m`/`24h` not `1m0s`/`24h0m0s`
on the example path (cadence now `1m`). All gate + acceptance green.

**In-cluster plain-http emit to a local collector (alloy-receiver) — ✅ BUILT 2026-06-20.** The realistic
customer/EKS topology is `genai-otel-bridge → in-cluster Alloy OTLP receiver (→ Grafana Cloud)`, i.e. emit to
`http://<alloy-receiver>.<ns>.svc.cluster.local:4318` (or :4317 grpc). The emit cleartext-credential gate
`[CP-M7]` now has a gated opt-out: `emit.telemetry.otlp.allow_insecure` + `emit.self.otlp.allow_insecure`
(field on `OTLPConn`, default false). With the flag, an http NON-loopback endpoint is permitted ONLY
token-less (`validateEmitEndpoint` rejects a non-empty instance_id/token on a cleartext endpoint — nothing
credential-shaped rides the link; the collector holds the real GC creds) and ONLY for a private target (an
IP literal must be RFC-1918/loopback/link-local — a public IP is rejected; a DNS host like a k8s Service
is permitted, unresolvable at config-load time). The product emitter (`emit/otlp`) AND the self-obs OTel
exporter (`selfobs/provider.go`) both omit the `Authorization` header entirely when creds are empty (no
`Basic Og==` over cleartext). https endpoints ignore the flag. Captured 2026-06-20 during EKS
test prep.

## 3. Vendor SDKs

v1 hand-rolls thin HTTP clients per source (full control, no coupling to SDK version churn,
and the raw endpoints are already validated). If/when official Go SDKs for Portkey / LangSmith
are mature, evaluate them as an internal implementation detail of the relevant source package
— never let an SDK type leak across the `model` boundary.

**LangSmith DOES have an official Go SDK — evaluated 2026-06-21, DECLINED for now.**
`github.com/langchain-ai/langsmith-go` (official LangChain, v0.15.0 / Jun 2026, **Stainless OpenAPI-
generated**, Go 1.25+). Covers runs query / sessions / feedback / datasets + bulk operations + its own
OTel tracing. Declined as a dependency on three grounds:
- **Pre-1.0, API-unstable by its own disclaimer** ("certain backwards-incompatible changes may be
  released as minor versions") — unacceptable churn for a production-critical-path poller.
- **Cross-vendor dependency bloat.** Its `go.mod` direct-requires OTHER vendor LLM SDKs
  (`anthropics/anthropic-sdk-go`, `sashabaranov/go-openai`, `google.golang.org/genai`) plus its own
  otel v1.43.0 / grpc v1.80.0 / cloud.google.com/go stack — exactly the coupling a vendor-neutral
  decoupled poller must avoid. (No conflict TODAY with our pins: it indirect-pins protobuf **v1.36.11**
  = our CP-M1 pin, and has no `k8s.io/*` deps vs our client-go v0.35.6.)
- **It does NOT help the load-bearing control.** Probe proved LangSmith `select` is not an egress
  filter (0.13.5) — content returns regardless; an OpenAPI-gen SDK deserializes content into typed
  fields, so the client-side strip + content-leak gate stays load-bearing either way (arguably the SDK
  makes leak-avoidance harder, not easier). The boilerplate it saves (pagination/auth) is the cheap part.
- **Still useful as a free OpenAPI reference** for confirming wire field names (e.g. the §9 S12
  prompt-identity names) without a live probe. **Portkey:** still no official Go SDK found — status
  unchanged.

## 4. Future source categories & loops

- **LangSmith bulk-export → S3 (Parquet)** — batch/backfill/archive path; needs broader key
  scopes (create export destination/job). Optional; only if a customer wants long-range archive.
  **What it gives that the poller structurally CANNOT** (it's a different pipeline shape — batch/
  records, not real-time OTLP — so it COMPLEMENTS the `runs`/`sessions` loops, it doesn't replace them):
  - **Full content / regulated records.** The `runs` logs loop is content-free by hard rule (never
    requests `inputs`/`outputs`/`messages`/`events`; default-deny strip drops them). Bulk export to a
    *customer-owned* S3 bucket can carry the COMPLETE record (prompts/completions/full I/O) into a
    WORM-capable validated immutable store — the §1 regulated-records path. Inference content
    is a regulated *record* that belongs in customer-owned storage, **never** in Loki/Mimir. The poller
    cannot emit content by design (content-minimisation release gate).
  - **Long-range history.** The poller is forward-only from a watermark, logs `max_backfill` capped at
    24h (≤ Loki 7d). Bulk export is a batch job over an ARBITRARY time range — full historical depth, no
    accept-window ceiling (lands in S3, not a time-bounded backend).
  - **Completeness without ingestion limits.** No per-tick rate limit, no 1DPM cap, no Loki
    cardinality / 15-stream-label budget — the ENTIRE dataset to object storage, unsampled.
  - **Analytics-ready columnar format.** Parquet in S3 is queryable by lakehouse/warehouse tooling
    (Athena/Spark/DuckDB/dbt) for ad-hoc analysis, offline eval, ML training — a different consumption
    model from time-series + log streams in Grafana.
  - **What the poller gives that bulk export does NOT** (why it's an addition, not a swap): near-real-time
    operational signals on a 1m cadence (latency/tokens/cost/errors as live OTLP metrics + correlation
    logs) for dashboards/alerts/SLOs; vendor-neutral OTLP straight into the existing GC stack; content-
    safety by construction.
  - **Costs / why parked:** needs a more privileged key (create export destination + job, vs the
    read-only `runs/query` the poller uses); it's NOT OTLP operational telemetry but a separate
    object-storage pipeline (arguably a stack/terraform concern more than poller code); justified only by
    a concrete need for long-range archive or a regulated content-records store.
- **More gateway / eval-platform vendors** (other LLM gateways, other eval platforms) — each a
  new source package behind the existing interface. The interface stays bounded to these two
  archetypes.
- **Self-traces** — an HTTP poller's causal story is well-covered by its logs; OTel-Go span
  emission is cheap to add later if a customer wants spans of the poll/emit pipeline.
- **Multi-workspace / multi-project targeting + auto-discovery (Portkey, and parity for LangSmith).**
  Today the Portkey loops have **no workspace selector**: analytics/groups return whatever the API
  key is scoped to, and `logs_export` takes a single static `workspace_id`. There is no way to
  target a chosen workspace for analytics/groups, nor to "emit all workspaces under an admin key,
  noticing new ones." Wanted: (a) optional explicit workspace targeting for analytics/groups (a
  workspace filter on the request, or a `workspace`-keyed fan-out), and (b) **filter-bounded
  workspace auto-discovery** — list workspaces via `/admin/workspaces`, match a configured filter,
  emit each (with the per-workspace label), refresh on a TTL to pick up new ones — mirroring the
  LangSmith runs loop's session (project) auto-discovery. **Live key-scope finding (2026-06-21,
  validated against a live test instance):** a Portkey key can *list* a large number of workspaces
  (broad management read; forbidden on `/admin/users/me` + `/admin/api-keys`), but its **analytics
  DATA is scoped to a single scoped workspace** — `/analytics/groups/workspace` returns a single row,
  and the `x-portkey-workspace` header / `workspace_id`
  query param are both **ignored** by the analytics graph endpoints (scope is bound to the key). So
  in a single-workspace deployment the analytics/groups data is already workspace-scoped and `logs_export` is
  pointed at that one workspace UUID. Where there are multiple parent workspaces — covering them all
  needs this feature (or one source entry / key per workspace). LangSmith, by contrast, IS tenant-scoped
  at the key and the runs loop already auto-discovers all projects in the tenant via `session_filter`
  (+ `session_ids`) — so LangSmith targeting already exists; only Portkey lacks it.
  **✅ SAFETY GUARDRAIL BUILT 2026-06-21 (`scope.go`, `settings.expected_workspace`):** since per-request
  workspace targeting on analytics/groups is IMPOSSIBLE (Portkey ignores the param), the realistic design
  for the workspace envs is one workspace-scoped **key per env** (per-env source entry + `deployment.environment`
  label). To catch a mis-paste (someone using a too-broad/global key), the optional `expected_workspace`
  knob makes analytics/groups assert (one-time, lazy) that `GET /analytics/groups/workspace` returns
  EXACTLY that workspace slug; a mismatch ⇒ **refuse to emit** (loud, no advance) +
  `OnGraphSkipped(loop,"workspace_scope_mismatch")`, recovering without restart (resilient posture, a design decision). `logs_export` is unaffected — it already passes `workspace_id`, which Portkey DOES
  respect. This is the guardrail, NOT the fan-out feature: emitting MULTIPLE workspaces under one admin key
  still needs real targeting, which the analytics API can't do — so multi-workspace coverage remains
  key-per-workspace until/unless Portkey adds per-workspace analytics scoping.

## 5. Plugin runtime (only if a real third-party-extension need appears)

v1 is "plugin-ready, not a plugin runtime": sources are compile-time modules; adding one is a
code contribution + rebuild. A formal versioned plugin SPI with dynamic/out-of-tree loading
would only be justified if customers or third parties must add sources **without** touching
core. Revisit only on concrete demand; the bounded interfaces built in v1 are the prerequisite
either way.

## 6. Provenance note (decoupling)

The tool is decoupled from any originating engagement by hard rule — no customer names, endpoints, label values, workspace ids, or topology appear in core code or default config; they appear only as *example* configuration. See ARCHITECTURE.md §16 (decision ledger) for the decoupling rule.

## 7. Deployment modes (Cdx-H1)

Two modes with **different risk profiles** — don't imply they're equivalent:

- **Generic standalone mode** — `genai-otel-bridge` emits OTLP directly to Grafana Cloud. Simplest; the default
  for a customer running it themselves. Durability = replay-from-source (+ optional persistent-queue
  Alloy, §2).
- **Regulated / customer-controlled mode** — route through an **in-cluster Alloy** for centralised
  egress, auth, queueing, and a **content-strip transform** + content-leak conformance gate (§1). In
  a regulated-customer context that local-Alloy path is part of the *control design*, not a
  mere deployment preference — direct-to-cloud bypasses those controls. A regulated deployment uses
  this mode and treats the content/cardinality conformance test as a **release gate** once logs or
  run-index ship.

## 8. Out of the v1 slice until shape/behaviour is proven (PoC-gated)

**Standing rule:** anything scoped out of v1 *"until its shape is known"* is recorded here, with what
must be proven before it enters scope. The v1 slice is **Portkey workspace-aggregate graphs → OTLP
metrics** only. (Detail/IDs: `docs/DESIGN.md` §7/§7a; live-API findings already gathered in §15 there.)

| Item | Out until we prove… | Refs |
|---|---|---|
| **Portkey `groups` analytics** (per-`ai_model` / `use_case` labelled series) | ✅ BUILT (2026-06-19; shape RESOLVED by a live-probe PoC, then implemented). `groupsLoop` in `internal/source/portkey/groups.go`: **WINDOW-TOTAL** stateless periodic-SNAPSHOT loop (`Window==0`; fresh Gauge per poll stamped at the window upper bound `now-settle`; no per-bucket watermark/settle/granularity; no Mimir duplicate-timestamp hazard). Per-endpoint independent (`ai-models` + opt-in `metadata/{key}`); all-or-nothing across pages within an endpoint; `current_page` 0-indexed, stop on empty/short/no-progress page; tolerant dim-field decode (unconfirmed metadata field name); distinct `_by_model`/`_by_metadata` Gauge names; new label keys allow-listed in `app.go`. Shared httpx client/limiter with the analytics loop. **Two flags ✅ RESOLVED 2026-06-20 (content-safe live probe of a live test instance):** **(i) cost field unit** = USD **cents** (physical-ceiling proof: for a representative model with known per-token pricing, the request volume × token cap × per-token rate yielded a max-possible dollar figure well under the field value read → field is in cents, not dollars). Cost gauge is now **ON by default** (`settings.emit_cost`) and emitted as `_cost_usd_by_*` (÷100 → dollars). **(ii) metadata-group row field name** = `metadata_value` (explicit; the decoder reads the dim value from the per-endpoint field name, not the old lone-non-reserved-field heuristic — real metadata rows carry extra stat fields `avg_tokens`/`last_seen`/… that broke the heuristic). `users` dimension deferred (per-end-user cardinality). Full spec: `docs/superpowers/specs/portkey-groups-poc.md`. | Cdx-C5, DESIGN OP5b/RP1 |
| **Portkey `logs_export` loop** (→ OTLP logs → Loki) | ✅ BUILT (2026-06-20; lifecycle proven by a 2026-06-19 live-probe PoC). Emits **OTLP logs** to `/v1/logs` (same gateway/auth as metrics; lands in Loki downstream). `internal/source/portkey/logs_export.go` step machine (idle→created→polling→downloading) + `logs_client.go` lifecycle + `logs_download.go` signed-URL host allow-list + streaming JSONL chunker (never buffers the ≤50k file) + `logs_strip.go` content-free field allow-list + `logs_settings.go` decoupled knobs. Job state in `Watermark.Cursor`, one step/tick, `Time` advances only on full-window completion (no FROZEN seam change). **Delivery is at-least-once** (in-flight page resumes via cursor; a job failure / mid-window leader change restarts the window from page 0 and may re-emit a page — Loki tolerates dup operational records; a completed window is never re-pulled). Guard wired in `app.go` (allow-list `ai_org`/`ai_model`/`response_status_code`; denylist backstop now includes `metadata`/`portkeyHeaders`). **Content-leak conformance gate Cdx-C7 PASSES** (`TestLogsExportContentLeakConformanceGate`, end-to-end Build→strip→guard→encode→/v1/logs; verified it fails on a real leak). Review hardening: `max_backfill` floor, failed/stuck-job metric via `OnGraphSkipped`, loud download-truncation error. **STILL REQUIRED BEFORE SHIP: GS1** (Loki stream-label promotion of `ai_org`/`ai_model`/`response_status_code` — Grafana-staff stack-side action; un-promoted, indexed attrs land as structured metadata and `{label=…}` matches nothing). **New v1.1 followups** (deliberately deferred, all documented in code): (a) `field_denylist` settings key not wired — the hardcoded denylist already covers the injected fields, so this is extensibility not safety; (b) per-chunk re-download amplification (a >chunk_max_records page re-GETs the whole S3 object per chunk) — correct + bounded-memory, tune `chunk_max_records`/`window` for high-traffic windows; (c) failed-export error taxonomy + in-VPC host re-probe (unchanged from PoC open items). Full spec: `docs/superpowers/specs/portkey-logs-export-poc.md`. | Cdx-C6, Cdx-C7, DESIGN RP2; GS1 |
| **LangSmith** sessions/eval (metrics) | ✅ RESOLVED (2026-06-19, d01927d + wired 191fdc8). The **sessions/eval metrics** source (aggregate-now / snapshot archetype, `Window==0`) is built and fully wired: registered, decoupled per-loop `settings` knobs, snapshot-aware validator, guard label allow-list (`quantile`/`session`/`feedback_key`), generated chart example. Metrics-only — requests no `inputs`/`outputs`/`events`. **Live-validated 2026-06-20** against a live test instance (content-safe): cost=number, latency=seconds, multiple numeric feedback keys emitted, id-like keys correctly excluded incl PII `user_id`. **Still deferred:** the validated-platform + service-key + tenant (`X-Tenant-Id`) prod semantics (dev≠prod, Cdx-H3). The **runs/run-index logs** loop is now BUILT (next row). | DESIGN §14, OP5 |
| **LangSmith** runs/run-index (logs → Loki) | ✅ BUILT (2026-06-20; lifecycle/shape proven by live probing). The `runs` loop (`internal/source/langsmith/runs*.go`) is a forward-only **windowed log pull**: per-run records from `POST /runs/query` → content-free **OTLP logs** → Loki, for per-run correlation. New archetype (analytics-style window/settle/forward-watermark + cursor pagination + the logs_export-style strip & content-leak gate, no export-job lifecycle). `Window==0` (snapshot-scheduled; real span `settings.window`); ONE cursor page per Collect; `Time` advances only on whole-window completion; **AT-LEAST-ONCE** delivery. **Scope is required** — `session_ids` (static) or filter-bounded auto-discovery via `/sessions` + `session_filter` (cached, capped) — never firehoses (a deployment may have many projects; a "session"=a project, not an env). **Content gate (`select` does NOT strip content on tested instances — PROBED):** default-deny strip (indexed `run_type`/`status`; record content-free operational fields; everything else dropped) + guard allow-list/denylist + `TestLangsmithRunsContentLeakConformanceGate` (app pkg, green). Naive-UTC timestamps; loud counted truncation on an oversize window; 429→quota. **STILL REQUIRED BEFORE SHIP: GS1** (Loki stream-label promotion of `run_type`/`status` — Grafana-staff, not a code blocker). **Content-FILTERING policy deferred** (next item): the loop is content-free by default; what to strip / whether to ever emit content awaits operator requirements. **Field/data re-validation 2026-06-21 (a sample of runs across several projects, content-safe):** all code field NAMES are real columns (and unlike Portkey, a wrong `select` name 422s the whole query — so dead names can't ship silently; `TestRunsSelectFieldsAreValidServerEnum` pins the projection). POPULATION findings: `run_type`(chain/llm) `status`(success/error) `id`/`trace_id`/`session_id` `start_time`/`end_time` `dotted_order` `total_tokens`/`prompt_tokens`/`completion_tokens` = 100% populated; `parent_run_id`/`thread_id`/`first_token_time` null (root/non-streaming — expected). TWO gaps: **(a) `trace_tier` is NULL across the sampled runs** (a single-run sample earlier saw `longlived` but at scale it's empty) — an indexed/stream-label that's effectively dead on this instance — **RESOLVED: dropped from the indexed set** (`runs_strip.go` now indexes only `run_type`/`status`), freeing a stream-label slot, so GS1 no longer needs to promote it. **(b) per-run `total_cost`/`prompt_cost`/`completion_cost` are NULL across the sampled runs** (tokens ARE populated) — no per-run cost signal here; likely needs a LangSmith price model, or it lives in the `*_cost_details` nested objects (currently dropped by the scalar-only strip). Both are DATA-availability findings, not name bugs — no code change forced; deployment-specific investigation required (per-run cost would need `*_cost_details` decoding). Spec: `docs/superpowers/specs/langsmith-poc.md` §5. | DESIGN §14; Cdx-C8, GS1 |
| **OTLP-log → Loki label promotion** (any logs feature) | that the target stack promotes our intended stream labels — **else set them up via GS1** | Cdx-C8, Grafana-staff stack-side action GS1 |
| **Portkey `latency` graph schema** (quantiles?) | ✅ RESOLVED (OP5a, 2026-06-19): summary `{avg,p50,p90,p99}`, points `{timestamp,avg,p50,p90,p99}` → per-percentile gauges `{quantile}`. **Unit:** ✅ RESOLVED (a1d140d): live soak measured p99 ~155633 ms; `derive.go` divides by `msPerSecond`; the `_seconds` suffix is correct. | DESIGN §15 OP5a |
| **Zero-bucket behaviour** | ✅ RESOLVED (OP5f, 2026-06-19): Portkey returns **explicit `total: 0`**, never omits → emit zero gauges; "no traffic" ≠ "poller down" is data-distinguishable | DESIGN §15 OP5f |
| **`bucket_settle` re-measurement** | ✅ RESOLVED (2026-06-19): measured **~185s** max post-close late-arrival lag on a live traffic-bearing workspace; default raised to **10m** for headroom. Settle-exceedance is now **actively detected** via `genai_otel_bridge_bucket_revised_after_settle_total{loop}` (alert on `rate(...) > 0` to bump `bucket_settle` if load grows). | C1/H3, DESIGN §15 OP5d |
| **Mimir accept window** (caps `max_backfill`) | ✅ RESOLVED (2026-06-20). Grafana Cloud defaults: Mimir `out_of_order_time_window` = **2h**, Loki `reject_old_samples_max_age` = **168h (7d)** — both per-tenant overridable via GS2. Metrics `max_backfill` default is now **90m** (was 55m; ≤ 2h with ~30m margin). Logs loops default **24h** (≤ 7d). Raise via GS2 if tolerated downtime exceeds these ceilings. | H3, GS2 |
| **Multi-window-per-tick catch-up acceleration** | ✅ RESOLVED (2026-06-19, cf4c2c7/f422c05). Contiguity-preserving by construction: catch-up only shortens the tick **WAIT** (`catchupInterval` 2s) when a windowed loop is >1 window behind `now`; every collect still goes through the single-flight `Busy()`-gated path + the source's own rate limiter, so windows never overlap and the watermark advances exactly as the slow path. Bounded by `governance.max_catchup_per_tick` (default **1 = no acceleration = exact v1 behaviour**; N>1 drains up to N windows per cadence period, then a steady-cadence breather). Pure `tickPlan` state machine unit-tested (`TestTickPlan`); snapshot loops (Window==0) never accelerate. | DESIGN §7a (Cdx-C13/F44); `schedule/scheduler.go` |
| **Scheduler-side degraded/backoff for a persistent granularity flip** | ✅ RESOLVED (2026-06-19, cf4c2c7). On `ErrGranularityUnexpected` the scheduler now calls `Runner.Degrade("granularity_unexpected")` (counted via `EmitError`), so the loop enters the same degraded state terminal rejects use and backs off to `DegradedBackoff` (10m) instead of re-pulling — and hammering the source with the same loud error — every cadence tick. A later good collect+commit clears the degrade. Covered by `TestGranularityFlipDegrades`. | Cdx-M5; `schedule/scheduler.go` |
| **`source_capability{state}` / skipped-404 counter** | ✅ RESOLVED (2026-06-19, 2358717). A skipped graph (single-graph 404 = by-design capability detection) now fires the `source.Deps.OnGraphSkipped(loop, graph)` hook, wired in `app` to `selfobs.SourceGraphUnavailable` → `genai_otel_bridge_source_graph_unavailable_total{loop,graph}`. A graph flapping 404 is now observable (alert on `rate(...) > 0`) and distinct from a permanently-absent capability. Covered by `TestCollect404FiresGraphSkippedHook`. | round3-#4; `source/portkey/portkey.go` |
| **Robust lease-transition epoch read** | ✅ **Retrying read BUILT (`afd1928`); checkpoint-epoch variant declined.** `lease.epoch()` now retries a transient LeaseTransitions Get (`epochReadAttempts=5`, `epochReadBackoff=200ms`, ~800ms worst case « renew deadline) so an API blip at election no longer forces the runner's forward-write fence; a SUSTAINED failure still falls back to epoch 0 → the fence trips LOUDLY (`checkpoint_fenced`, round3-#3), never silent. The remaining "checkpoint-epoch-aware read" (seed the epoch from the durable checkpoint when the Lease API is down AT election) was **declined**: it only helps the rare API-down-exactly-at-election case the retry already softens, it trades a loud-honest fence for added `coordinate → checkpoint` coupling (today cleanly separate), and the fence is operationally honest as-is. Revisit only if election-time API outages prove common. | round3-#3; `coordinate/lease/lease.go` |
| **Derive liveness threshold from settle/backoff explicitly** | ✅ RESOLVED (2026-06-19, b38c914). `main.go` now derives the `/healthz` staleness threshold as `max(schedule.DegradedBackoff[=10m], slowestEnabledCadence) + emitRetryBudget[2m] + livenessMargin[4m]` from the real exported constant (`schedule.DegradedBackoff`), not a coincidental literal — same numeric result, but a leader in retry/backpressure isn't killed while a wedged scheduler still is. | final-review LOW; `cmd/genai-otel-bridge/main.go` |
| **Validate replicas>1 + coordinator:none is a footgun** | ✅ RESOLVED (2026-06-19, b38c914). Defence-in-depth: the Helm chart **fails to render** on `ha.coordinator=none` + `replicas>1` (a `fail` guard in `deployment.yaml`), and the binary re-checks at startup via the `GENAI_OTEL_BRIDGE_REPLICAS` downward-API env (`coordinator==none` + parsed replicas>1 → fatal). Also flipped the chart default `replicas: 3 → 2` (active/passive — one standby is enough; raise to 3 for a second standby). | final-review LOW; `config` / `deploy` |
| **LangSmith snapshot 1DPM at sub-/at-minute cadence** | ✅ **BUILT 2026-06-21.** `derive` now stamps sessions gauges at `now.Truncate(time.Minute)` (was raw `now`), mirroring the portkey groups loop: two polls in the same wall-clock minute (sub-60s cadence, or the ~60s+jitter edge) now share a sample timestamp → Mimir LWW/duplicate-ts dedup ⇒ exactly 1 point/series/minute (CoalesceDPM is per-batch and can't dedup across polls). The watermark liveness cursor stays the precise un-truncated `now` (set in `Collect`, separate from the sample stamp). Truncation alone enforces 1DPM, so no validator cadence-floor was added (same disposition as groups). Test: `TestCollectStampsAtMinuteResolution` (sub-minute now → minute-truncated samples, precise watermark). | review-M3 (groups); `source/langsmith/derive.go` |
| **httpx SSRF guard vs proxy + DNS rebinding** | the destination guard (`checkDest`) is exact for IP-literal hosts and resolves+checks hostnames, but when an HTTP(S)_PROXY is configured the *proxy* resolves the hostname, so a DNS-rebinding race between our check and the proxy's resolution can't be fully closed in-process. The dial guard stays authoritative for the direct path. Close it (if ever needed) by pinning resolution or routing egress policy to the proxy itself. | `httpx.checkDest` |

**Build-decision deviations (recorded during the 2026-06-19 build):**

| Decision | Why | Revisit when |
|---|---|---|
| `k8s.io/*` pinned to **v0.35.6**, not the plan's v0.36.2 | stable client-go v0.36.2 transitively **requires an untagged `google.golang.org/protobuf` pseudo-version** (v1.36.12-pre); v1.36.11 is the newest tag. Keeping the load-bearing protobuf pin **v1.36.11** (gateway-acceptance, CP-M1) + a stable, tagged client-go forces v0.35.6 (leaderelection + ConfigMap RMW APIs are unchanged across these minors). Avoids both an alpha k8s and a pseudo-version → fully reproducible. | a tagged protobuf ≥v1.36.12 ships and a client-go release pins it; then re-pin both to current stable and re-test gateway acceptance. |

**Considered & declined (external review 2026-06-19):**

- **Refuse a present zero-`Time` checkpoint record** (Codex finding #13). A present `{}`/zero-`Time`
  record decodes to a zero watermark and is treated as absent (bootstrap). Declined: a legitimate
  Save never writes a zero `Time`, so such a record carries no progress to lose; treating it as
  absent yields a deterministic idempotent re-emit or a *counted* gap (both benign); the prod
  ConfigMap store behaves identically (not file-specific); and refusing would convert a benign state
  into a startup failure (worse for availability, no integrity gain). Full rationale in the external review.

**k8s failover testing — correctness notes (recorded 2026-06-19, ahead of the k3d e2e build):**

- **Failover is NOT sub-second — assert against `LeaseDuration`, not a fast release.** `coordinate/lease/lease.go`
  sets `ReleaseOnCancel: false` *intentionally* (F35): on SIGTERM the leader persists watermarks and lets the
  Lease **expire** rather than releasing it into a standby mid-drain. So on BOTH a graceful `kubectl delete pod`
  (SIGTERM → ctx cancel → renewals stop) and a hard `kill -9`, the standby acquires only after the Lease
  expires — ~`LeaseDuration` (15s) after the old leader stops renewing (params 15s/10s/2s = LeaseDuration/
  RenewDeadline/RetryPeriod, `main.go:121`). e2e failover assertions MUST use tolerances ≥ LeaseDuration +
  RetryPeriod + jitter (a ≥30s `Eventually` budget); the generic "~2s with `ReleaseOnCancel=true`" guidance is
  the WRONG steer here — for us slower failover is a deliberate no-mid-drain-handoff trade, not a gap.
- **`terminationGracePeriodSeconds: 300` ≠ failover delay.** A gracefully-deleted leader lingers in `Terminating`
  up to 300s while it drains an in-flight emit (`values.yaml`), but it stops renewing the Lease at SIGTERM, so
  the standby takes over ~15s later regardless — the e2e must not wait for the old pod to fully terminate before
  asserting takeover. Hard-kill scenarios should use `--grace-period=0 --force` / `kill -9` to skip the drain.
- **Deterministic e2e observables (there is no scrape endpoint).** Self-metrics leave only via OTLP
  (`internal/selfobs/provider.go`) — no Prometheus `/metrics`. The e2e asserts on (1) the Lease `genai-otel-bridge-leader`
  (`holderIdentity`, `leaseTransitions`) and (2) the checkpoint ConfigMap `genai-otel-bridge-checkpoints` (monotonic
  watermark, never rewinds — `checkpoint.CheckMonotonic`), both read via client-go, plus a loopback OTLP sink
  **sidecar** for per-pod emit observation (emit is https-or-loopback per `config.go:223`, so the sink must be
  loopback in-pod, not a ClusterIP Service).

External-review dispositions are folded into the rows above; the Grafana-staff / stack-side actions
these imply are tracked in `deploy/grafana/README.md`.

## 9. Originating design-analysis cross-check — items not previously tracked here

A full re-read of the originating design analysis
against the built tool + this file surfaced the items below: each is **in genai-otel-bridge's decoupled scope**
(or a constraint/boundary the tool must honour) and was **not captured anywhere in this file**. The
*broad* engagement scope in the design analysis — app-tier trace correlation, the operator's Alloy pipeline
(`drop_content`/`gen_ai.*` normalise/spanmetrics/recording rules), AWS CloudWatch/AgentCore/Bedrock
streams, Faro RUM, Terraform/IRSA/PrivateLink, dashboards/SLOs, PDC/Infinity — is **deliberately
excluded**: by the hard decoupling rule those are deployment-specific delivery, not poller code. GS-staff /
stack actions implied below stay in `deploy/grafana/README.md` (GS1–GS4), not here.

| Item | What the design analysis wanted | Current state in genai-otel-bridge | Action / disposition | Refs |
|---|---|---|---|---|
| **S12 prompt-level usage** (`prompt_slug`/`prompt_version_id`/`prompt_id`) | Per-prompt cost/tokens/latency/volume panels — an explicit v1 signal (S12) | ✅ **BUILT + LIVE-CONFIRMED 2026-06-21.** `prompt_slug` + `prompt_version_id` + `prompt_id` are in the logs_export **default record allow-list** (`defaultLogFieldPolicy`), ship as structured metadata out of the box, and `defaultRequestedData()` (derived from the policy) asks Portkey for them — per-prompt cost/token/latency correlation works with no `extra_record_fields` opt-in. Record tier (per-prompt context), not indexed/stream-label. **Live probing (content-safe, field-NAMES only):** `prompt_slug` is a real, populated column (present on a large fraction of sampled rows; a small number of distinct slugs observed, confirming low cardinality). The originating design analysis guessed **`prompt_version` is NOT a real column** — requested without error but never returned; the real field is **`prompt_version_id`** (populated on the *same* saved-prompt rows as `prompt_slug`), plus **`prompt_id`**. Fixed the policy + tests (`TestStripDefaultsPromptIdentity`) + regenerated `values.yaml`/eks `requested_data`. NOTE: requested_data IS a projection (you get the fields you ask for) — the PoC "not an egress filter" finding is specifically that Portkey *injects* `metadata`/`portkeyHeaders` on top, regardless; the strip remains load-bearing for those two. Also dropped 4 confirmed-dead requested names (`status_code`/`request_tokens`/`response_tokens`/`currency` — `response_status_code` + `total_units`/`req_units`/`res_units` carry that data; no `currency` column). **Still deferred:** per-prompt *metrics* (not just log fields) would need a `groups/prompts` dimension (like `groups/ai-models`). | design analysis |
| **Loki 15 / Mimir 40 resource-attribute caps vs the opt-in label knobs** | Loki promotes ≤15 resource attrs to labels and Mimir caps at 40 per metric; **exceeding fails ingestion** | ✅ **BUILT 2026-06-21.** Validated against the real GC backend config: the hard limit is Loki `max_label_names_per_series` = **15** (`deployment_tools` `loki-cloud-mixin/limits.libsonnet`); genai-otel-bridge's product identity (`service.name`/`service.namespace`/`deployment.environment` — all in the GC Loki `default_resource_attributes_as_index_labels` set) + each logs loop's indexed attrs (base ∪ `extra_indexed_fields`) consume it. New knob `governance.max_stream_label_keys` (default 15, tenant-overridable — Grafana staff can raise `max_label_names_per_series` per tenant, so the knob raises to match); the composition root **fails fast at Build** if `identity + a logs loop's IndexedKeys()` would exceed it (via the new `source.IndexedKeyDeclarer` optional interface on both logs loops). The **Mimir 40-attr cap is N/A**: genai-otel-bridge emits only 3 resource attrs on the metrics plane (identity); series labels are data-point attributes governed by the per-metric cardinality budget. **Caveat (documented, not enforceable here):** in the in-cluster-Alloy topology, Alloy's `k8s.*`/`cloud.*` enrichment (also in the default-promoted set) shares the same 15-slot budget — genai-otel-bridge can't see it, so the cap is necessary-but-not-sufficient; operators size with headroom or lower the knob. Tests: `internal/app/stream_label_budget_test.go` + per-vendor `IndexedKeys()` tests. | design analysis |
| **Portkey logs_export 50k-logs/job hard cap → window sizing** | Split export windows when the 50,000-logs/job hard cap would be exceeded | ✅ **Already handled structurally (clarified 2026-06-21).** The originating design analysis concerned a SINGLE whole-window export silently capping at 50k. The built loop does NOT do that: it creates **one export job per PAGE** (`current_page` increments; each page-job requests `page_size ≤ portkeyPageSizeMax`=50000), so a single job structurally cannot exceed the per-job cap. The matched `total` is reported ACCURATELY at create (a large matched total on a busy deployment) and drives `pages = ceil(total/page_size)`, paginated across separate jobs; `maxPagesPerWindow` (default 50) is the loud over-size tripwire that **errors** (never silent-drops) when a window is mis-sized. A "detect `total`==50000" honesty signal was considered and REJECTED: `total` is accurate (≫50k on busy windows by design), so it would false-positive on every busy window and fire spuriously on the prod critical path. **✅ Residual RESOLVED by live probe 2026-06-21 (content-safe, `id`-only):** multi-page `current_page` across SEPARATE export jobs is contiguous, non-overlapping, and complete. A settled-window 4-page test (page size chosen to produce 4 pages) returned full pages + a correct short FINAL page with **union distinct == total, cross-page duplicates == 0** — so the snapshot is stable across separately-created jobs and there are no gaps/overlaps. The `page_size` server-clamp (50000) and accurate matched `total` were re-confirmed (a representative multi-day window produced a large matched total well above the per-page cap; the first-page job generated successfully). NOTE: an over-aggressive burst (170 page-jobs created in a tight loop) provoked `status=failed` on later jobs + one short mid-sequence page — a **rate-limit artifact**, gone under gentle pacing; the loop creates one page-job per tick (rate-limited via the shared httpx limiter), so it does not burst. **New finding → FIXED 2026-06-21:** a FULL 50k-record export is SLOW to generate server-side (observed ~10-20 min in `in_progress`), which exceeded the old default `job_poll_timeout` (10m) — a high-traffic window producing a full 50k page would have been abandoned+restarted and never completed. Defaults raised to handle load: **`job_poll_timeout` 10m → 30m** and **`download_timeout` 2m → 5m** (`defaultLogsSettings`; values.yaml + eks values regenerated). Guidance documented in code: a deployment with even heavier traffic should shrink `window` so pages stay well under the 50k cap rather than raise the timeout further. No page-completeness *detector* is needed (a short non-final page would be a false positive; pagination proven reliable). | design analysis |
| **Auth-failure observability** (`auth_errors_total`) | Poller self-telemetry distinguishing API failure types | ✅ **BUILT 2026-06-21.** `genai_otel_bridge_auth_errors_total{loop,source}` (selfobs counter) fires via the new `Deps.OnAuthError(loop,source)` hook (same injected-hook pattern as `OnGraphSkipped`/`OnBucketRevised`; wired in `app.go`) when an upstream responds 401/403 — a credential failure is now its OWN alertable signal (`alert on rate(...) > 0`), separable from the generic 4xx/timeout the `upstream_request_duration_seconds{status_class}` histogram lumps together. `source.IsAuthStatus(code)` is the shared 401/403 predicate. Instrumented at the four GET-based loop status-check sites that hold the raw code: portkey `analytics`+`groups`, langsmith `sessions`+`runs`. Tests: one hook-fires test per loop + a selfobs counter/attribute test. **Deliberate coverage gap (documented):** the portkey `logs_export` job lifecycle (via `logs_client`, which returns a string error not a code) is NOT yet instrumented — a lifecycle 401 still surfaces loudly via `window_lag` + the `export_failed` graph-skip signal. Thread the code through `logs_client`/`runs_client` if logs-loop auth needs its own counter. | design analysis |
| **Full `source_capability{state}` metric** | Distinguish endpoint-absent / plan-unsupported / permission-denied / no-data / transient-404 / schema-changed | Only `genai_otel_bridge_source_graph_unavailable_total` (the 404-skip hook) exists; the richer 6-state capability metric is deferred. | Low priority. Build the 6-state metric if capability flapping needs finer attribution than the 404 counter gives. (Clarifies the §8 row, which is marked RESOLVED but covers only the 404 counter.) | design analysis |
| **Portkey S3-side log ingestion** (Option A: bucket-notification → download) | When the log-store is operator-owned S3 (WORM-capable, compliance-preferred), ingest via bucket notification → poller download, as an alternative/augment to the export-job loop | **Not a source mode.** v1 logs_export uses the Portkey export-job lifecycle only. | Optional, PoC-gated. A new logs source mode (object-notification driven) behind the existing interface — only if an operator deployment uses Option A. Belongs alongside §4 future source categories. | design analysis |
| **Log-derived operational metrics boundary** (retry/fallback/guardrail tallies, per-status-class splits) | Retry-count / fallback-count / guardrail-verdict tallies via **downstream Loki recording rules** over the body-excluded export stream, written back as Mimir series | **By design NOT emitted by genai-otel-bridge.** genai-otel-bridge ships the body-excluded logs; deriving these counts is a stack-side Loki-ruler concern. (Portkey cache-hit-rate + rescued-requests ARE available as analytics graphs genai-otel-bridge can poll directly.) | Boundary note — not a code gap. Capture so it is not assumed genai-otel-bridge emits these; if an operator wants them as series, that is a recording-rule (deploy/stack) task, not poller code. | design analysis |
| **LangSmith auth model** (OAuth session vs service key) | Flagged an OAuth session-TTL conflict (86400 vs 7200) to design around for token refresh | **N/A for the built path.** genai-otel-bridge uses static service API keys (`lsv2_sk_` + `X-Tenant-Id`), not OAuth sessions — no session-refresh concern. | Boundary note. If a deployment mandates OAuth (not service keys), token-refresh handling would need building in the langsmith source. Captured so it is not re-discovered as a gap. | design analysis |

## 10. Self-APM tracing — future span coverage (opt-in per-tick span BUILT 2026-06-21, decision #14)

**Built:** `selfobs.tracing.enabled` (default-off) installs an OTLP `TracerProvider` on the same self
endpoint/`-meta` identity as self-metrics (→ Tempo, no separate channel); `schedule.runOnce` emits one
`loop.tick` span per tick tagged with the loop name + outcome (sample/log counts, error status). Spans
go through the OTel global tracer, so a disabled build pays only a no-op span. See `internal/selfobs/
tracing.go`, `cmd/genai-otel-bridge/main.go`, `ARCHITECTURE.md` #14.

**Stdout-log visibility (done 2026-06-21):** a configurable `log.level` (default info) plus rate-limited
WARNs for collect/emit/checkpoint/auth failures and Info lifecycle lines (leader elected/lost, first
successful commit, config summary) now make `kubectl logs` a useful triage surface — a flapping upstream
or a bad credential is visible without the self-o11y stack. The spans below add the **timing and causal
detail logs can't** (where a slow emit/export spends time, which tick drove a request); they are not a
substitute for the logging, and vice-versa.

**Future spans worth adding — ranked by debugging value, with emphasis on what is NOT visible from the
app's stdout logs today.** Each is a discrete follow-up; none blocks the current build.

| Span / path | Why it's worth it (what's invisible today) | Notes |
|---|---|---|
| **Emit pipeline (`loop.emit`) — cross-queue propagation** | The tick span stops at `Enqueue`; the actual **encode → OTLP POST → retry/backoff** runs async in the runner's worker. Crucially the **emit POST uses a plain `http.Client`, NOT `httpx`, so it is NOT in the `upstream_request_duration_seconds` histogram** — emit latency / per-attempt retry timing is essentially unobserved beyond the `emit_errors_total` counter. A child span here would show where a slow/with-retries emit spends time. | Needs the trace context carried THROUGH the bounded queue into the worker goroutine (the batch/queue item must ferry the `context`/SpanContext). The single highest-value next span. |
| **Checkpoint commit (`loop.commit`)** | The epoch-fenced `Save` (ConfigMap RMW) latency + fence outcome is only a `checkpoint_fenced` counter today; a slow/contended ConfigMap write or a fence trip has no timing. A span around `commit()`/`Save` shows write latency + fenced-vs-clean. | Lives in `schedule/runner.go`; child of the emit span (or the tick, depending on propagation). |
| **logs_export job lifecycle steps** | The Portkey export is a multi-tick state machine (create → poll → download → page). Logs show coarse phase transitions, but **where a slow export spends time — queued at Portkey vs. download streaming — is not timed**. Per-step spans (or span events across ticks via a stored trace/span id in the cursor) would localise it. | Cross-tick correlation is the hard part (each step is a separate `Collect`); a stored trace id in `exportCursor` + span links is the likely shape. |
| **httpx request spans (`upstream.request`) nested under the tick** | The upstream histogram gives per-target latency, but not the **causal parent** (which tick/collect drove it) nor the in-request detail (DNS, cross-host-redirect block, SSRF-guard reject). A span at the `httpx` chokepoint nested under `loop.tick` answers "why did this poll take 8s". | Wire via the existing `httpx.Observer` seam (or an `otelhttp`-style RoundTripper) so `httpx` stays decoupled. Covers ALL source calls (analytics/groups/sessions/runs/logs control-plane). |
| **Leader-election / failover (`coordinate.elect`)** | Failover handoff duration is only inferable from Lease `leaseTransitions` + logs. A span around acquire/renew would time the standby→leader handoff (the ~LeaseDuration gap) directly. | Lower priority — failover is rare and the lease-transition metric + e2e already characterise it. |

**Guard rail (applies to every future span):** spans describe our OWN timing/counts/outcomes only —
never data-plane payload (same content-floor discipline as the guard; trivially satisfied since the
self path never holds customer content). Keep sampling head-only + low (AlwaysSample is fine at current
tick volumes; revisit if a future span fans out per-record).

## 11. Late-settled data — metrics settle tuning + logs backfill (investigated 2026-06-24)

Question: when upstream data settles AFTER our window passes, is it missing from metrics, logs, or both —
and can we recover it? Investigated with live probes + a code read; full write-up, the adversarial review,
and the two probe scripts live in gitignored `docs/superpowers/late-data-handling-plan.md` (+
`portkey_revision_probe.py`, `langsmith_lateness_probe.py`).

**Backend asymmetry (decisive):**
- **Metrics (Mimir): un-backfillable.** A changed value at an already-written `(series,ts)` is rejected
  (`err-mimir-sample-duplicate-timestamp` / `new-value-for-timestamp`); the OOO window only admits NEW
  past timestamps, never a correction. ⇒ `bucket_settle` is the ONLY lever.
- **Logs (Loki): backfillable in principle.** OOO writes accepted within `reject_old_samples_max_age` (7d)
  AND within ~1h of the stream's newest entry (`max_chunk_age`/2); exact-duplicate (ts+line) entries dedup.

**Findings:**
- **Logs are NOT materially late ⇒ backfill DESIGNED then SHELVED, not built.** LangSmith root-run duration
  (a run is missed only if duration > settle): 779 runs/12h, p99 5m, max 15.2m, **0.1% > 10m settle**. The
  forward loops capture ~99.9% today. Building a backfill sweep (separate trailing cursor, dedup/seen-set,
  separate-stream handling for Loki's per-stream 1h window, new config/metrics — all in the plan) would have
  been seam-touching work for a non-problem. The adversarial **B0 premise-gate** caught this before any code.
  Optional trivial nudge if ever wanted: `runs` settle 10m→16m. `logs_export` (Portkey) unmeasured but same
  fast-completion logic; a one-off export-diff can confirm if it ever matters. **Do not build logs backfill
  without first showing material lateness.**
- **Metrics: shipped the age histogram instead of a blind settle bump** (see the v1 DONE row). A clean
  fixed-window probe showed Portkey 1-min buckets settle ~3m, contradicting the in-product
  `bucket_revised_after_settle` ~29% (which is BURSTY 0–308/h and records count, not age). The "29% is a
  >55m granularity-clamp artifact" hypothesis was REFUTED by code (`portkey.go` clamps both emit and the
  widened detection fetch to ≤55m, `[granularity-safety]`, guarded by `TestCollectFetchWindowNeverExceeds55m`).
  So the age histogram measures how-late continuously; tune settle off p95-of-age post-deploy.

**Probe gotchas (reusable):** Portkey is behind Cloudflare which 1010-bans `Python-urllib` (set a normal
User-Agent); Portkey granularity clamps SHARPLY at ≤55m→1-min, >55m→10-min buckets (use a FIXED probe
window, not a sliding one); per-bucket gauges are STALE to instant PromQL at the loop's ~11m offset (use
`last_over_time(...[20m])`). Creds came from a local API-key env file (not in-repo).
