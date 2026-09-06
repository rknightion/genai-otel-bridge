# internal/source/langsmith

Eval-platform source. Three loops behind the common interface, each independently enabled, all
sharing ONE `httpx` client and therefore one rate limiter: the LangSmith request budget (~10 req/10s)
is tenant-wide.

- **`sessions`** - per-session aggregate gauges from `GET /sessions?include_stats=true`.
- **`runs`** - per-run content-free OTLP logs from `POST /runs/query`, for correlation and debugging.
- **`usage`** - platform cost-driver gauges (traces and spans ingested per project, by retention tier).

Paths above are relative to the configured `base_url`, which carries the `/api/v1` prefix.

## Aggregate-now, NOT time-bucketed (the load-bearing difference vs portkey)

`sessions` and `usage` read a **rolling snapshot** over `[now-stats_window, now]`: one current value
per session, no `data_points`, no per-minute buckets.

- **Every sample is stamped at `now.Truncate(1m)`** (review-M3). Two polls in the same wall-clock
  minute share a sample timestamp so Mimir last-write-wins dedups them to exactly 1DPM. `CoalesceDPM`
  is per-batch and cannot dedup across polls, so this truncation is the whole mechanism.
- The watermark advances `Time = now` (precise, un-truncated) as a forward-only liveness cursor;
  `since.Time` is unused.
- **No settle, bucket-revision or gap-free backfill.** `deps.OnBucketRevised` is deliberately omitted
  (nothing to revise) and `ErrGranularityUnexpected` is not a path here. `bucket_settle`,
  `max_backfill` and `bootstrap_lookback` are portkey-bucket concepts that do not apply.

The `runs` loop is the exception: it is a forward-only windowed log pull (see below).

## Content and cardinality (release gate - do not weaken)

- `feedback_stats` interleaves numeric eval scores with **id-like categorical keys**
  (`portkey_trace_id`, `request_id`, `session_id`, `user_id`) whose `values` maps hold raw
  identifiers. `derive` emits a feedback gauge **only when `avg != null`**, structurally excluding
  every id-like key.
- The decode structs **omit** `values`, `extra`, `description`, `type`, `stdev` and friends, so those
  identifiers never enter process memory. Keep new decode structs equally narrow.
- Session names are often ephemeral per-experiment hashes, so `session_label_value` defaults to `id`
  and `session_filter` should be set in production to bound cardinality. The per-metric guard budget
  is the backstop.
- `AllowedLabelKeys()` in `labels.go` declares this source's content-free label keys; the composition
  root unions and dedupes them against the other vendors. Add a new label key there, not in
  `internal/app`.

## Traps

- **Latency and first-token stats are SECONDS.** No ms conversion, unlike portkey.
- **`cost` is a JSON number on 0.13.5 and a string in the 0.16.5 spec**, so `money` accepts both plus
  null and `""`. Response timestamps are naive (no timezone); the snapshot loops never parse them.
- **null is not 0.** Nullable stats are `*float64`/`*int`: nil means skip, `0` means emit a real zero.
- **Pagination is offset/limit with a plain JSON array, no cursor.** Stop on a short page, on the
  `max_sessions` cap (logged, never silent), OR on a no-progress page - that last arm defends against
  an offset-ignoring server and is what stops the loop hanging.
- **Error taxonomy:** 429 to `source.ErrQuotaExceeded` (discard, back off, F34); 401/403/5xx/timeout
  to a retryable Collect error (no advance, loud `window_lag`). A 404 on the sole endpoint is a real
  error, not a capability probe.
- **Config is package-local** (`langsmith.Config`); nothing leaks into `internal/config` or the Helm
  generator. Knobs and defaults live in `settings.go` / `runs_settings.go` and are surfaced in the
  generated Helm example via `ExampleSource()` / `ExampleSettingsComments()`. A new knob without a
  `just gen` run fails `gen-check`.

## runs loop

The LangSmith analogue of portkey `logs_export`, but with no export-job lifecycle - `runs/query` is a
synchronous paginated POST.

- **State lives in `Watermark.Cursor`** (JSON: win_min/win_max/next/page). One cursor page per
  `Collect`; `LoopConfig.Window == 0` so the scheduler snapshot-gates it and the real span is
  `settings.window`. `Watermark.Time` is the last fully-drained window's win_max and advances ONLY
  when the API's `cursors.next` is exhausted. The runner's `Cursor != ""` commit arm persists
  in-flight progress; without it the first window (at `Time == zero`) loops forever.
- **Delivery is AT-LEAST-ONCE**, not the metric plane's exactly-once. A mid-window leader change
  resumes at `cur.Next`; an emit-then-checkpoint failure may re-emit a page. Loki tolerates dups.
- **Scope is REQUIRED**: `settings.session_ids` (static csv) wins, else filter-bounded auto-discovery
  via `GET /sessions` plus `settings.session_filter`, capped at `max_sessions` and cached in memory
  (`session_refresh` TTL, reset on failover). Fail-fast if NEITHER is set. A LangSmith "session" is a
  **project** (one per app), each a UUID - it is NOT an environment.
- **`select` is enum-validated server-side and does NOT trim content.** A value outside the accepted
  set 422s the WHOLE `runs/query`, taking the entire loop down from one bad field.
  `validLangsmithSelectEnum` in `runs_strip.go` is the single source of truth, captured verbatim from
  a server 422; `TestRunsSelectFieldsAreValidServerEnum` pins the projection to a subset of it and
  `validateRunsSettings` rejects an out-of-enum opt-in at config load (#65). `execution_order` is NOT
  in the enum.
- **The strip is the authority, the guard is the backstop.** `runs_strip.go` is a default-deny
  allow-list; `Body` is never set, nested objects and arrays under an allowed key are skipped rather
  than stringified, a JSON `null` is dropped rather than emitted as `""`, and a scalar array renders
  as a csv. `error` is free text and is dropped by default - `status` is the error signal.
- **An un-allow-listed INDEXED key drops the WHOLE record**, so `extra_indexed_fields` promotions are
  auto-allow-listed in the guard at the composition root rather than left to the operator.
- **`hardDeniedRunsFields` is derived from `source.AbsoluteNeverDenyKeys()`** so it cannot drift from
  the guard backstop, plus the LangSmith-specific `inputs_s3_urls`/`outputs_s3_urls` - signed URLs to
  the raw blobs, i.e. a message body plus a live credential. Opting one in is rejected fail-fast.
- **Producer identity:** `strip` stamps `RecordAttributes["source"] = "langsmith"` on every run, at
  RECORD tier, so portkey and langsmith log data are distinguishable in Loki
  (`| source="langsmith"`) with no stream-label budget cost. The portkey strip mirrors it.
- **Timestamps are naive** and parsed as UTC; the run's `start_time` IS the LogRecord timestamp,
  unlike the snapshot loops. `cost` is emitted as the raw scalar string - logs need no arithmetic.
- **Failure honesty:** 429 to `ErrQuotaExceeded`; other non-200 to a retryable Collect error. A window
  over `max_pages_per_window` advances PAST with a loud counted gap
  (`OnGraphSkipped("runs", "window_truncated")`) rather than stalling. The `max_backfill` floor skips
  an unstorable old span loudly.
- Whether to ever emit content is deferred pending the customer's requirements (`followup.md`). The
  loop is content-free by default; do not change that on your own judgement.

## usage loop

Answers a different question from `sessions`: not LLM/token cost but the **platform** usage that
drives LangSmith's own bill.

- `{prefix}_usage_traces` is the billable unit (root runs, from `session.run_count`).
  `{prefix}_usage_spans` is the storage / "excessive spans" driver (all runs, one `runs/stats` call
  per project per poll). Both are gauges labelled by the session dimension and `retention_tier`
  (`longlived` ~400d, the expensive tier / `shortlived` ~14d / `unknown` for an empty `trace_tier`) -
  the billing multiplier.
- **`emit_span_counts` is on by default and costs one extra call per project per poll.** Bound the
  fan-out with `session_filter` and `max_sessions`, or set it false to emit trace counts only. The
  loop already logs a warning when it is on with no filter.
- The generated Helm example sets the loop's `cadence` equal to `stats_window` so windows tile and
  `sum_over_time` approximates a period total. Changing one without the other double-counts or gaps.

## Deeper references

- `reference/runs-fields.md` - read before adding a field to `extra_record_fields` or
  `extra_indexed_fields`, or before widening the default projection.
