# internal/source/langsmith — eval-platform source (sessions/eval metrics)

Pulls per-session aggregate gauges (run count, latency/first-token percentiles, token/cost totals,
error/streaming rates, numeric feedback scores) from LangSmith's `GET /api/v1/sessions?include_stats=true`.
`langsmith.go` (Config/New/source/loop/fetch/pagination), `derive.go` (pure response→samples + the
nullable stat fields), `money.go` (number-or-string cost), `testhooks.go` (`SetLoopClockForTest`).

Two loops behind the common interface, sharing ONE httpx client (one rate limiter — the LangSmith
~10 req/10s budget is tenant-wide): **`sessions`** (metrics, below) and **`runs`** (per-run content-free
OTLP logs → Loki; see the runs section). `New` builds whichever are enabled; `newClient` +
`newSessionsLoop` + `newRunsLoop` are the constructors.

## Aggregate-now, NOT time-bucketed (the load-bearing difference vs portkey)

- Stats are a **rolling snapshot** over `[now-StatsWindow, now]`: one current value per session, no
  `data_points`, no per-minute buckets. So **every sample is stamped at `now.Truncate(1m)`** (minute
  resolution — review-M3): two polls in the same wall-clock minute share a sample timestamp so Mimir
  LWW dedups them to exactly 1DPM (CoalesceDPM is per-batch, can't dedup across polls). The watermark
  advances `Time = now` (precise, un-truncated) as a forward-only liveness cursor; `since.Time` is unused.
- **No settle / bucket-revision / gap-free backfill.** `deps.OnBucketRevised` is omitted (nothing to
  revise). `ErrGranularityUnexpected` is not a path. `bucket_settle`/`max_backfill`/`bootstrap_lookback`
  are portkey-bucket concepts and do not apply here.

## Content + cardinality (release gate — do not weaken)

- `feedback_stats` interleaves numeric eval scores with **id-like categorical keys** (`portkey_trace_id`,
  `request_id`, `session_id`, `user_id`) whose `values` maps hold raw identifiers. derive emits a
  feedback gauge **only when `avg != null`** (numeric) — structurally excluding every id-like key.
- The decode structs **omit** `values`, `extra`, `description`, `type`, `stdev`, … so those identifiers
  **never enter process memory** (the JSON decoder skips them). Content-safety by construction.
- Session names are often **ephemeral** (per-experiment hashes) ⇒ `SessionLabelValue` defaults to `id`,
  and `SessionFilter` SHOULD be set in prod to bound cardinality. The per-metric guard budget is the backstop.
- `quantile`, `feedback_key`, and the session label key are guard-policed (default-deny) — the
  composition root must allow-list them.

## Gotchas

- **Units (live-confirmed against 0.13.5):** latency/first_token are **SECONDS** — NO ms conversion
  (unlike portkey). `cost` is a JSON **number** in 0.13.5 but a **string** in the 0.16.5 spec ⇒ `money`
  accepts both (+ null/""). Response timestamps are **naive (no tz)** — we never parse them (stamp at `now`).
- **null vs 0:** nullable stats are `*float64`/`*int`; nil ⇒ skip, `0`/`0.0` ⇒ emit (real zero).
- **Pagination:** offset/limit, plain JSON array (no cursor); stop on a short page, the `MaxSessions`
  cap (logged, never silent), OR a no-progress page (defends against an offset-ignoring server — never hangs).
- **Error taxonomy:** 429 ⇒ `source.ErrQuotaExceeded` (discard/back off, F34); 401/403/5xx/timeout ⇒
  retryable Collect error (no advance; loud window_lag). A 404 on the sole endpoint is a real error.
- **Config is package-local** (`langsmith.Config`): nothing leaks into `internal/config` or the Helm
  generator. `New` maps the generic `config.SourceConfig`/`LoopConfig["sessions"]` + LangSmith defaults;
  the coordinator wires the langsmith-specific root-config block (WIRING TODO in the PoC spec).

Tests: `httptest` fakes (offset/limit pagination, status overrides); `SetLoopClockForTest` for
deterministic snapshots; sanitised `testdata/` fixtures from a real 0.13.5 capture (synthetic ids).
`TestCollectNeverLeaksContent` guards both the request projection and the `values`-id non-leak (FR10).

## runs loop (forward-only windowed log pull → OTLP logs — `runs.go`)

A NEW archetype: per-run records from `POST /api/v1/runs/query` → content-free **OTLP logs** (→ Loki),
for per-run correlation/debugging. The LangSmith analogue of Portkey `logs_export`, but with NO export-
job lifecycle (runs/query is a synchronous paginated POST). Files: `runs.go` (loop + `Collect` state
machine + `runsCursor`), `runs_client.go` (`queryRuns` + content-free `select` + `respCap`),
`runs_strip.go` (default-deny strip + naive-UTC ts + status→severity), `runs_settings.go` (knobs +
`newRunsLoop`), `runs_discover.go` (filter-bounded session auto-discovery).

- **State machine in `Watermark.Cursor`** (JSON: win_min/win_max/next/page). ONE cursor page per
  `Collect`; `LoopConfig.Window==0` (snapshot-scheduled; the real span is `settings.window`).
  `Watermark.Time` = last fully-drained window's win_max (forward-only); advances ONLY when the API
  `cursors.next` is exhausted. The runner's `Cursor!=""` commit arm persists in-flight progress (first
  window runs at `Time==zero`). **Delivery is AT-LEAST-ONCE** — a mid-window leader change resumes at
  `cur.Next` (no re-emit of completed pages); an emit-then-checkpoint failure may re-emit a page (Loki
  tolerates dups). A window-boundary run (inclusive `start_time` bounds) may dup — negligible.
- **Scope is REQUIRED (live probe: runs/query 400s without one; 100+ projects ⇒ no firehose):**
  `settings.session_ids` (static csv) wins; else filter-bounded auto-discovery via `GET /sessions` +
  `settings.session_filter`, capped at `max_sessions`, cached in-memory (`session_refresh` TTL; resets
  on failover). Fail-fast if NEITHER is set. A "session" = a LangSmith **project** (one per app), each a
  UUID — NOT an environment.
- **Content governance (release gate — `select` does NOT TRIM content on 0.13.5, PROBED — the full field
  set returns regardless; BUT `select` IS enum-validated server-side, so a value outside the accepted set
  422s the WHOLE runs/query — the 2026-06-21 `execution_order` outage, now guarded by
  `TestRunsSelectFieldsAreValidServerEnum`):** the
  default-deny `runs_strip.go` allow-list is authoritative (indexed: `run_type`/`status`;
  record: content-free operational fields incl. high-card ids `id`/`trace_id`/`session_id`; everything
  else — `inputs`/`outputs`/`messages`/`events`/`extra`/`serialized`/`error`/`name` — dropped; nested
  objects skipped; `Body` never set). `error` is free text → dropped by default; `status` is the error
  signal. `app.go` allow-lists the 3 indexed keys (an un-listed indexed key drops the WHOLE record) and
  the content **denylist** is a backstop: a never-subtractable FLOOR (`source.AbsoluteNeverDenyKeys` —
  message bodies + injected PII, enumerated in `internal/source/CLAUDE.md`) plus a gray tier that is on the denylist by default
  but RELEASED per-deployment when a loop opts it into `extra_record_fields` (so a default deployment
  keeps the full backstop). Opting in a floor field — or the LangSmith raw-blob pointers
  `inputs_s3_urls`/`outputs_s3_urls` (signed URLs to the raw blobs = a body + a credential) — is rejected
  fail-fast (`hardDeniedRunsFields`, derived from the shared floor so it can't drift). End-to-end gates (app package): the
  `TestLangsmithRunsContentLeakConformanceGate` (default content-free) + `TestLangsmithRunsExtraRecordFieldFlows`
  (opt-in flows; backstop holds) — keep both green before shipping.
- **Producer identity (`source` record attr):** `strip` stamps `RecordAttributes["source"]="langsmith"`
  on every run (`stampSource`, content-free constant) so portkey vs langsmith log data is distinguishable
  in Loki as structured metadata (`| source="langsmith"`). RECORD tier, not indexed — no stream-label
  budget / fingerprint impact. The portkey logs_export strip mirrors it with `"portkey"`.
- **Configurable governance — `settings.extra_record_fields` (csv).** Default-on guards stay content-free;
  the operator can opt extra fields into the strip's RECORD (structured-metadata) allow-list. The
  `select` projection mirrors the active allow-list. The strip renders a **scalar array as a csv** (a
  JSON array of string/number/bool joins with `,`; objects/nested-arrays/null/empty → dropped, never
  flattened), so operational arrays are opt-in-able. Opting in a hard-denied body
  (`inputs`/`outputs`/`messages`/`request`/`response`/`metadata`) is REJECTED at config time (loud — the
  guard would otherwise drop the whole record silently). Field buckets to inform an opt-in:
  - **A — default-emitted, content-free** (always on): indexed `run_type`/`status` (`trace_tier` dropped —
    NULL at scale, reclaims a stream-label slot; re-addable via `extra_indexed_fields`); record
    `id`/`trace_id`/`session_id`/`parent_run_id`/`thread_id`/`start_time`/`end_time`/`first_token_time`/
    `total|prompt|completion_tokens`/`total|prompt|completion_cost`/`dotted_order`. (NOTE: `execution_order`
    was removed 2026-06-21 — it is NOT a valid `select` enum value on 0.13.5 and 422'd the whole query.)
  - **B — operational, safe to opt in** (scalars): `app_path`/`reference_example_id`/`reference_dataset_id`/
    `in_dataset`/`price_model_id`/`ttl_seconds`/`trace_upgrade`/`last_queued_at`/`trace_first_received_at`/
    `trace_min|max_start_time`; (arrays→csv) `tags`/`child_run_ids`/`parent_run_ids`; (objects, still
    dropped) `*_token_details`/`*_cost_details`; ⚠ `share_token` is an access token — do NOT opt in.
  - **C — content / free-text** (opt-in = explicit content decision): `inputs`/`outputs`/`messages`/
    `events`/`inputs_preview`/`outputs_preview`/`extra`/`serialized`/`manifest`/`*_s3_urls`/`error`/
    `name`. (`inputs`/`outputs`/`messages` are hard-denied and CANNOT be opted in; the rest can.)
  - INDEXED (Loki stream-label) opt-in IS now offered via `settings.extra_indexed_fields` (csv): routes a
    content-free field into the strip's INDEXED tier (`withExtraIndexedFields`) and the composition root
    auto-allow-lists it in the guard (so a promotion can't be silently dropped). Hard-denied content/floor
    fields are rejected fail-fast; a startup WARN flags the cardinality blast (the per-loop budget is the
    runtime backstop). Still GS1-gated to be queryable as `{label=…}`.
- **Timestamps are NAIVE** (no tz) → parsed as UTC; the run's `start_time` IS the LogRecord timestamp
  (forward-only log, unlike the sessions poll-now snapshot). `cost` (number or string) emitted as the raw
  scalar string (logs need no arithmetic). `feedback_stats`/`*_details` are objects → dropped by the
  scalar-only strip.
- **Failure honesty:** 429 → `ErrQuotaExceeded`; other non-200 → retryable Collect error (no advance,
  loud); an oversize window (> `max_pages_per_window`) advances PAST with a loud counted gap
  (`OnGraphSkipped("runs","window_truncated")`) — never stalls, never silent. `max_backfill` floor skips
  an unstorable old span loudly.
- **Decoupled knobs** (`settings`, no `internal/config` change): `session_ids` | `session_filter`
  (one req'd) `max_sessions`(100) `session_refresh`(5m) `window`(1h) `settle`(10m) `max_backfill`(24h)
  `page_size`(≤100) `max_pages_per_window`(50) `max_response_bytes`(32MiB) `root_only`(false)
  `run_type`("") `extra_record_fields`(csv, empty — opt-in RECORD fields, see content governance)
  `extra_indexed_fields`(csv, empty — opt-in INDEXED/stream-label fields; auto-allow-listed, GS1-gated).
  `data_source_type` is inert on 0.13.5 (probed) — no knob. **GS1 prereq** (not a code blocker): the
  indexed attrs need stack-side Loki stream-label promotion to be queryable.
- **Content-filtering POLICY is a `followup.md` item** pending the customer's requirements — the loop is
  content-free by default (safe), but whether to strip more/less (or ever emit content) is deferred.

## Chart config surfacing

All `sessions` + `runs` settings knobs are surfaced in the Helm chart example block via
`langsmith.ExampleSource()` + `langsmith.ExampleSettingsComments()` (every key at its package default,
with per-key head-comments). `make generate` regenerates `deploy/helm/values.yaml`.
