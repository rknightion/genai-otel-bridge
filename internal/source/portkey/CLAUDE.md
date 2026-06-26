# internal/source/portkey — first concrete source (LLM gateway)

Two loops behind the common `source.Loop` interface, each independently enabled via its own
`loops.<name>` config block; `New` builds whichever are enabled and **shares one `httpx` client / rate
limiter** across them (so both stay within Portkey's tenant-wide request budget). `New`/`newClient`/
`newAnalyticsLoop` live in `portkey.go`.

- **`analytics`** (`portkey.go` + `derive.go`) — TIME-BUCKETED. Workspace-aggregate gauges (requests,
  cost, tokens, latency, errors, users) from the validated graph API. Gap-free per-bucket watermark.
- **`groups`** (`groups.go` + `groups_derive.go`) — WINDOW-TOTAL SNAPSHOT. Per-dimension gauges
  (ai-models → `{ai_model}`; request-metadata → `{metadata_key,metadata_value}`). See the section below.

## Window / settle / watermark math (the load-bearing logic — analytics loop)

- Granularity pinned to **1 minute**; window clamped to **≤ 55m** defensively (H5) regardless of config.
- `Collect`: `start = max(since.Time, now-maxBackfill, bootstrap-if-zero)`;
  `until = min(start+window, now-settle)`. If `until ≤ start` → return empty batch, watermark
  unchanged (nothing settled yet — does not stall the loop, CP-C2).
- **`startSemantics = true`** (OP5e): Portkey stamps bucket-START times; `derive` converts to
  bucket-END by adding granularity. Samples are forward-only (`bend.After(since)`).
- Settled cutoff = `now-settle`; later samples dropped. Omitted zero-buckets (gaps that are multiples
  of granularity) are accepted; a non-multiple step (90s vs 60s) → `ErrGranularityUnexpected`.

## Gotchas

- **Latency is special:** only the latency graph emits per-percentile gauges (p50/p90/p99) with a
  `{quantile}` label. The composition root must allow-list `quantile` (Guard is default-deny). Other
  graphs emit a single gauge from `total`.
- **404 handling:** a single graph 404 is logged + skipped (capability probe, F5); but if **all**
  configured graphs 404, `Collect` errors loudly (config/permission problem, no advance, CP-R3) rather
  than masking it as empty data.
- **Quota:** any graph with `is_quota_exceeded=true` discards the whole batch, no advance (`ErrQuotaExceeded`, F34) — no partial success.
- Unknown graph name in config fails fast at `New` (`unknown graph %q (supported: cost,errors,latency,requests,tokens,users)`).
- Response body capped at 1MB. Series name = `metric_prefix + "_" + suffix[graph]`.
- **Settle-exceedance detection (`revision.go`):** each `Collect` FETCHES from a widened lower bound
  (`start − band`, band ≈ `2×bucket_settle`) so the response re-includes recently-settled buckets;
  emit still uses the forward-only `start` (byte-identical), but a bounded in-memory `revisionHistory`
  compares re-observed buckets and fires the injected `Deps.OnBucketRevised("analytics", age)` hook when
  an already-emitted bucket's value changes — `observe()` returns the revised buckets' bucketEnds, the
  caller passes `age = now − bucketEnd` (how late, always ≥ settle). The hook records BOTH
  `bucket_revised_after_settle_total` (count) and the `bucket_revised_after_settle_age_seconds` histogram
  (lateness) so `bucket_settle` can be tuned to p95-of-age rather than guessed — metrics CAN'T be
  backfilled (Mimir rejects a changed value at a settled `(series,ts)`), so settle is the only lever.
  Detection only — never re-emits (DESIGN §3.3/F6). History is in-memory ⇒ resets on failover (accepted blind spot).
- **`api_key_use_cases` — N internal passes, per-slug `revisionHistory`:** when `api_key_use_cases` is
  configured, analytics `Collect` loops over the resolved use-cases (`analyticsLoop.passes`) and for
  each pass fetches all graphs filtered by that use-case's `api_key_ids` CSV, derives samples, and stamps
  `Labels["api_key_use_case"]` with the slug. The settle-exceedance `revisionHistory` is **per-slug**
  (`analyticsLoop.histories map[slug]*revisionHistory`) — a late arrival for one key must not inflate
  the revision counter for another. `Key()` is **unchanged** regardless of use-case count (one Key per
  analytics instance, no watermark reset on migration). NEVER add fan-out (separate analyticsLoop
  instances per use-case) — `ValidateOwnership` (M7) would reject it at startup; see DESIGN §7 RP3.

## groups loop (window-total snapshot — `groups.go`)

A DIFFERENT shape from analytics, NOT a re-parameterised graphs loop. It deliberately imports **none**
of the bucket/settle/watermark/granularity/revision machinery above.

- **Snapshot, `Window==0`.** Each tick fetches a flat per-dimension TOTAL over the fixed trailing window
  `[now-window_span, now-settle]` (no per-bucket series). The validator treats `Window==0` as a snapshot
  (skips bucket-math) and the scheduler never accelerates it / never counts `backfill_unstorable` for it.
  `Watermark.Time = now` is a forward-only liveness heartbeat (window_lag = staleness); `since` is unused.
- **1DPM via minute-truncated timestamps.** Gauges are stamped at `(now-settle).Truncate(1m)` so two
  polls in the same wall-clock minute share a timestamp (Mimir LWW/dup-ts dedup) ⇒ exactly 1 point/
  series/minute regardless of cadence/jitter. The FETCH still uses the precise `now-settle` bound.
- **Per-endpoint independent.** `ai-models` (always on) + one `metadata/<key>` per configured
  `settings.metadata_keys`. A failed endpoint emits nothing for itself, fires `Deps.OnGraphSkipped(loop,
  endpoint)`, and does NOT block the others. Only when EVERY endpoint fails does `Collect` error (loud).
- **All-or-nothing across pages.** `current_page` 0-indexed; accumulate, discard the whole endpoint set
  on any page error / non-200 / `is_quota_exceeded` / parse failure (free to re-fetch next poll — it's a
  snapshot). Terminate on an empty/short page; a **page cap** (`ceil(max_groups/page_size)`) is the
  offset-ignoring-server backstop (we do NOT early-break on "no new dim value" — that would silently
  truncate if the metadata shape collapses values).
- **Distinct `_by_model` / `_by_metadata` names** (`groupMetricName`) never collide with the analytics
  aggregate names (M7). Guard must allow-list `ai_model`, `metadata_key`, `metadata_value` (done in `app`).
- **Workspace-scope guardrail (`settings.expected_workspace`, optional — `scope.go`):** analytics/groups
  data is bound to the API key's workspace and is NOT request-targetable (Portkey ignores the workspace
  param on the analytics endpoints — live-probed, followup §4). When set, the loop asserts (one-time, lazy,
  on first Collect) that `GET /analytics/groups/workspace` returns EXACTLY this workspace slug
  (e.g. `ws-acme-001`); a too-broad/global key (multiple rows, or a different one) ⇒ **refuse to emit**
  (loud Collect error, no advance) + `OnGraphSkipped(loop,"workspace_scope_mismatch")` — the resilient
  "stay up, emit nothing wrong" posture, recovers without restart. A transient probe failure is retried
  (no false alarm); no traffic in the 7d probe window ⇒ proceed unverified + re-check. Empty ⇒ no check.
  Set the SAME value on BOTH analytics and groups (`logs_export` is already hard-scoped via `workspace_id`,
  which Portkey DOES respect). The analytics loop reads it from its own `settings:` block (it otherwise
  uses structured LoopConfig fields).
- **Decoupled knobs ride `settings`** (no `internal/config` change): `window_span` (1h), `settle` (10m),
  `page_size` (1000), `max_groups` (10000), `metadata_keys` (csv, empty), `emit_cost` (true),
  `expected_workspace` (csv-free, empty — the scope guardrail above). A
  malformed known key fails fast; an unknown key warns. `settle >= window_span` is rejected (inverts the
  query window).
- **`api_key_use_cases` — N internal passes (same ownership rule as analytics):** groups uses ONE loop
  instance with N passes (`groupsLoop.passes []resolvedUseCase`). Per pass, each dimension endpoint is
  fetched with the pass's `api_key_ids` CSV and the slug is stamped on every returned sample. `Key()` is
  **unchanged** (one ownership entry, no watermark reset). Do NOT fan out into N groupsLoop instances —
  same M7 / ValidateOwnership hazard as analytics (see DESIGN §7 RP3).

### Resolved constraints (confirmed 2026-06-20 live probe)

- **Cost unit = USD cents (RESOLVED).** Confirmed by a physical token-ceiling argument: text-embedding-
  3-small's 72,045 reqs × 8,191-token max × $0.02/1M ≈ $11.80 max-possible dollars, but the field read
  136.85 → must be cents. Cost is now emitted as `_cost_usd_by_*` (÷100 → dollars) under
  `portkey_api_cost_usd_by_model` / `portkey_api_cost_usd_by_metadata`, with `emit_cost` **ON by
  default**. Note: this is a 2× series count on the ai-models endpoint vs the old provisional state (a
  new metric name `portkey_api_cost_usd_by_model`); downstream dashboards and Adaptive Metrics must
  expect it. Within the per-metric budget; no 1DPM concern (snapshot minute-truncates).
- **Metadata dim field = `metadata_value` (RESOLVED).** Confirmed by live probe. `parseGroupRows` now
  reads the dimension value from the EXPLICIT field name passed by the caller (`ai_model` for the
  ai-models endpoint, `metadata_value` for each metadata endpoint) — no more "lone non-reserved field"
  heuristic. Metadata rows also carry `avg_tokens` / `avg_weighted_feedback` / `last_seen` /
  `requests_with_feedback` (extra stat fields) which are ignored; we read only `metadata_value` /
  `requests` / `cost`. A `""` dimension value = the unattributed bucket (non-string dimField falls
  back to ""). KEEP the `_user` PII/cardinality caveat: metadata VALUES become label values; the
  per-series cardinality budget is the backstop; `metadata_keys` defaults to empty (opt-in).

`users` dimension is deferred (per-end-user → high cardinality; keep out + budget if ever wanted).

## logs_export loop (stateful export lifecycle → OTLP logs — `logs_export.go`)

A THIRD archetype: not a GET but a multi-step job lifecycle (create→start→poll→download→page). It
emits **OTLP logs** (`Batch.Logs`) to the gateway `/v1/logs` — same base/auth as metrics; lands in Loki
downstream. Files: `logs_export.go` (loop + `Collect` step machine + `exportCursor`), `logs_client.go`
(lifecycle calls, shared control-plane httpx client), `logs_download.go` (signed-URL host allow-list +
streaming JSONL chunker + a SECOND httpx client for the S3 object), `logs_strip.go` (content-free field
allow-list), `logs_settings.go` (decoupled knobs + `newLogsExportLoop`).

- **`api_key_use_cases` — fan-out, record-tier stamp:** unlike analytics/groups, logs DOES fan out —
  `New` builds one `logsExportLoop` per use-case (ownership-safe: `logsExportLoop` is NOT a
  `SeriesDeclarer`). Each instance carries its own `useCase` slug and `apiKeyIDs` CSV. The slug is
  set on `fieldPolicy.useCase` (logs_settings.go) **after** the `with*` builder chain — the builders
  return a fresh `fieldPolicy{}` literal that does NOT copy `useCase`, so setting it during the chain
  would silently drop it (see the CRITICAL comment in `newLogsExportLoop`). `stampUseCaseRecord` stamps
  `RecordAttributes["api_key_use_case"]` on every record (record-tier — queryable as
  `| api_key_use_case="data_gen"`, no GS1 stream-label promotion needed). `Key()` folds the slug into
  the naming component so each fan-out instance owns a distinct cursor watermark.
- **State machine in `Watermark.Cursor`** (JSON: phase/job_id/win_min/win_max/page/pages/total_records/
  page_offset_done/poll_deadline). One non-blocking step per `Collect`; `LoopConfig.Window==0` (the real
  window is `settings.window`) so the scheduler snapshot-gates it (no catch-up acceleration / no
  backfill count — correct: one window per tick, oldest-first). `Watermark.Time` = last fully-emitted
  window's win_max (forward-only); it advances ONLY when all pages of a window are emitted. The
  same-Time/cursor-change checkpoint relaxation persists in-flight progress (and the first window runs
  at `Time==zero`, so the runner commit gate has a `Cursor!=""` arm — without it window 1 loops forever).
- **Delivery is AT-LEAST-ONCE** (NOT the metric plane's exactly-once gap-free). In-flight pages resume
  via the cursor (re-download the stable S3 object, skip to `page_offset_done`); but a job failure or a
  mid-window leader change restarts the window from page 0 and may re-emit a page — Loki tolerates dup
  operational records, and a completed window is never re-pulled.
- **Content governance (release gate, do not weaken):** `requested_data` is NOT an egress filter — the
  PoC proved Portkey injects `metadata` (PII) + `portkeyHeaders` (config) regardless. `logs_strip.go` is
  a default-deny ALLOW-LIST (indexed: `ai_org`/`ai_model`/`response_status_code`; record: content-
  free operational fields; everything else dropped; `Body` never set; nested objects/arrays under an
  allowed key are skipped, never stringified; a JSON `null` is dropped, never emitted as `""`).
  `source.Guard.SanitizeLogs` is the defence-in-depth backstop (`app.go`'s effective denylist = the
  never-subtractable floor `source.AbsoluteNeverDenyKeys` + a gray tier MINUS opted-in fields).
  Configurable: `settings.extra_record_fields` (csv) appends content-free fields to the strip RECORD
  allow-list (and to `requested_data`, so Portkey includes them); opting in a hard-denied content field
  (`hardDeniedLogFields` = the shared floor + Portkey content fields incl. bare `prompt`) is rejected
  fail-fast. NOTE the asymmetry with the langsmith runs strip: this one does NOT render arrays as csv (no
  Portkey export field is a scalar array worth opting in; arrays stay dropped — defensive). The end-to-end
  gate is `TestLogsExportContentLeakConformanceGate` (app package) — keep it green before shipping.
- **Producer identity (`source` record attr):** `strip` stamps `RecordAttributes["source"]="portkey"`
  on every record (`stampSource`, content-free constant) so portkey vs langsmith log data is
  distinguishable in Loki as structured metadata (`| source="portkey"`). RECORD tier, not indexed — no
  stream-label budget / fingerprint impact. The langsmith runs strip mirrors it with `"langsmith"`.
- **Signed-URL SSRF (§7):** the download URL is a SERVER-controlled input → `validateSignedURLHost`
  exact-matches `settings.signed_url_allow_hosts` before any fetch, independent of the dlClient egress
  guard (whose `AllowHosts` is the SAME list — belt-and-braces). `signed_url_allow_hosts` + `workspace_id`
  are REQUIRED (fail-fast at construction). `requested_data` is validated content-free.
- **Streaming chunker:** the ≤page_size file is never buffered whole — `bufio.Scanner` line-by-line,
  skip `page_offset_done`, take ≤`chunk_max_records`. `page_offset_done` counts LINES (a malformed line
  is skipped-loud yet still advances). A download exceeding `download cap` errors loudly (no silent
  truncation). Per-chunk re-download is the resume mechanism — bounded memory, but a >chunk_max_records
  page re-GETs the whole object per chunk (tune `chunk_max_records`/`window` for high-traffic windows).
- **Failure honesty:** failed/stopped/stuck jobs `slog.Error` AND fire `Deps.OnGraphSkipped` (→
  `decant_source_graph_unavailable_total{loop=logs_export,graph=export_failed|export_stuck}`).
  `max_pages_per_window` over-size → loud error (no silent tail-drop). `max_backfill` floor skips an
  unstorable old span loudly (mirrors analytics F25).
- **Decoupled knobs** (`settings`, no `internal/config` change): `window`(1h) `settle`(10m)
  `max_backfill`(24h) `page_size`(≤50000) `max_pages_per_window`(50) `chunk_max_records`(5000)
  `job_poll_timeout`(30m — a full 50k page takes ~10-20m to generate server-side, live-probed §9)
  `download_timeout`(5m) `requested_data`(content-free)
  `extra_record_fields`(csv, empty — opt-in RECORD fields, see content governance)
  `extra_indexed_fields`(csv, empty — opt-in INDEXED/stream-label fields via `withExtraIndexedFields`;
  also merged into `requested_data`; auto-allow-listed in the guard; hard-denied content rejected; WARN on
  cardinality; GS1-gated)
  `metadata_record_fields`(csv, empty — lift named sub-keys OUT of the hard-denied `metadata` blob into the
  RECORD tier via `withMetadataFields`/`strip.liftMetadata`; the ONLY sanctioned path into `metadata`; only
  scalar, non-hard-denied sub-keys are lifted, the rest of `metadata` PII stays dropped; NOT added to
  requested_data — we never ASK for `metadata`, Portkey injects it and we extract client-side)
  `metadata_trace_id_field`(single key, empty — the one metadata sub-key whose UUID also populates
  `model.LogRecord.TraceID` → OTLP log `trace_id` for logs↔traces correlation, ledger #15; auto-lifted into
  the record tier; non-UUID value ⇒ TraceID unset but the attr still ships, and the record is counted via
  `OnGraphSkipped(logs_export,"trace_id_unparsed")` so a systematically broken upstream format is alertable;
  single key only — a comma is rejected fail-fast, csv goes in `metadata_record_fields`)
  `trace_id_field`(single key, empty — the ALTERNATIVE trace-id source: a TOP-LEVEL export field (e.g.
  Portkey's native `trace_id`) whose UUID populates `model.LogRecord.TraceID` → OTLP `trace_id`, for
  deployments that stamp the trace id as a first-class field rather than a metadata sub-key. MUTUALLY
  EXCLUSIVE with `metadata_trace_id_field` (both feed the one OTLP trace_id; both-set is rejected
  fail-fast); auto-unioned into the record allow-list + `requested_data`; shares the `trace_id_unparsed`
  counter via `policy.traceIDAttrKey()`. Live-probe note: this workspace's native `trace_id` is ~55% real
  UUIDs / ~45% free-text caller labels, so the unparsed counter tracks the un-mappable fraction)
  `signed_url_allow_hosts`(req'd) `workspace_id`(req'd). Example config block:
  `loops.logs_export: {enabled: true, cadence: 60s, settings: {workspace_id: ws-…, signed_url_allow_hosts: "signed-url-host.example.com", window: 1h, settle: 10m}}`.
- **GS1 is a SHIP prerequisite, not a code blocker:** the indexed attrs need stack-side Loki stream-label
  promotion (`ai_org`/`ai_model`/`response_status_code`) to be queryable as `{label=…}`; until then
  they land as structured metadata. Grafana-staff stack-side action.

## Chart config surfacing

All `groups` + `logs_export` settings knobs are surfaced in the Helm chart example block via
`portkey.ExampleSource()` + `portkey.ExampleSettingsComments()` (every key at its package default,
with per-key head-comments rendered as `# # comment` in the commented-out block). Groups defaults
are derived from `defaultGroupsSettings()` (extracted from `newGroupsLoop`) so the example can't
drift. `make generate` regenerates `deploy/helm/values.yaml`.

Tests: `fakePortkey`/`fakeGroups` `httptest.Server`s; in-package `lp.now` clock injection; table-driven
`derive`/`deriveGroups`; `TestCollectNeverSelectsContentFields` + `TestGroupsCollectQueryParams` assert
queries never request content/PII fields (FR10). logs_export: `fakeExport` lifecycle server + injected
clock — full happy/multi-page/chunked/empty paths, failover resume, SSRF host reject, backfill clamp,
failure metric, truncation guard (`logs_collect_test.go`/`logs_download_test.go`/`logs_settings_test.go`).
