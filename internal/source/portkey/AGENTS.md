# internal/source/portkey

LLM-gateway source. Three loops of three different shapes, each independently enabled via its own
`loops.<name>` block, all sharing ONE `httpx` client and so one rate limiter, keeping them inside
Portkey's tenant-wide request budget.

- **`analytics`** - TIME-BUCKETED workspace-aggregate gauges from the graph API, gap-free per-bucket
  watermark.
- **`groups`** - WINDOW-TOTAL SNAPSHOT, per-dimension gauges (ai-models, request-metadata, prompts).
- **`logs_export`** - a stateful export-job lifecycle producing OTLP logs.

`labels.go` `AllowedLabelKeys()` declares this source's content-free label and indexed-attribute keys.
Add a new key there, not in `internal/app`: the guard is default-deny, and an un-listed key drops the
sample or record.

## analytics: window / settle / watermark math

- Granularity is pinned to 1 minute and the window is clamped to **<= 55m defensively (H5)**
  regardless of config. A request window over 59m flips Portkey to 10-minute buckets, so the widened
  revision-detection fetch is clamped to the same 55m bound.
- `Collect`: `start = max(since.Time, now-max_backfill, bootstrap-if-zero)`;
  `until = min(start+window, now-settle)`. If `until <= start` it returns an empty batch with the
  watermark unchanged - nothing has settled yet, and that is not a stall (CP-C2).
- **`startSemantics = true`** (OP5e): Portkey stamps bucket-START times and `derive` converts to
  bucket-END by adding granularity. Samples are forward-only.
- Omitted zero-buckets are accepted when the gap is a multiple of granularity; a non-multiple step
  (90s where 60s was expected) raises `ErrGranularityUnexpected`.

## analytics traps

- **Only the latency graph emits per-percentile gauges** with a `{quantile}` label. Other graphs emit
  a single gauge from `total`.
- **Do NOT bare-sum `<prefix>_tokens` across `token_type`.** The tokens graph is split into
  `{token_type}`-labelled gauges including a `total`, so summing them all double-counts. Select one
  token_type, or sum `{token_type=~"input|output"}`.
- **404 is a capability probe only while some graph answers.** A single graph 404 is logged and
  skipped (F5); if **all** configured graphs 404, `Collect` errors loudly with no advance (CP-R3)
  rather than masking a config or permission problem as empty data.
- Any graph with `is_quota_exceeded=true` discards the whole batch with no advance
  (`ErrQuotaExceeded`, F34). There is no partial success.
- An unknown graph name fails fast at `New`. The supported set is `cost,errors,latency,requests,
  tokens,users`. The response body is capped at 1 MiB.
- **Settle-exceedance detection (`revision.go`) is detection only, never a re-emit.** `Collect`
  fetches from a widened lower bound so the response re-includes settled buckets, while emit keeps
  using the forward-only `start` and stays byte-identical. A bounded in-memory `revisionHistory`
  fires `Deps.OnBucketRevised("analytics", age)` into a counter and an age histogram, so
  `bucket_settle` is tuned to p95-of-age instead of guessed. Metrics cannot be backfilled (Mimir
  rejects a changed value at a settled `(series,ts)`), so settle is the only lever. The history
  resets on failover - an accepted blind spot.

## `api_key_use_cases`: N internal passes for metrics, fan-out for logs

- **analytics and groups use ONE loop instance with N passes.** Each pass fetches with that
  use-case's `api_key_ids` CSV and stamps the slug as `Labels["api_key_use_case"]` on every sample.
  `Key()` is unchanged regardless of use-case count, so a migration never resets a watermark.
  **NEVER fan out into one loop instance per use-case**: `ValidateOwnership` (M7) rejects it at
  startup (DESIGN §7 RP3).
- analytics keeps a **per-slug `revisionHistory`** so a late arrival for one key cannot inflate
  another's revision counter.
- **`logs_export` DOES fan out**, one loop per use-case, which is ownership-safe because
  `logsExportLoop` is not a `SeriesDeclarer`. `Key()` folds the slug into the naming component so each
  instance owns a distinct cursor watermark. There the slug is a RECORD attribute
  (`RecordAttributes["api_key_use_case"]`, queryable as `| api_key_use_case="..."`), so it needs no
  GS1 stream-label promotion.

## groups: window-total snapshot

Deliberately imports **none** of the bucket/settle/watermark/granularity/revision machinery above. It
is not a re-parameterised graphs loop.

- **`Window == 0` marks it a snapshot.** Each tick fetches a flat per-dimension TOTAL over the fixed
  trailing window `[now-window_span, now-settle]`. The validator skips bucket-math and the scheduler
  never accelerates it or counts `backfill_unstorable` against it. `Watermark.Time = now` is a
  forward-only liveness heartbeat; `since` is unused.
- **`settle >= window_span` is rejected at `New`** (review-H1): settle comes off the UPPER bound and
  `window_span` off the LOWER one, so an equal or larger settle inverts the query window
  (`time_of_generation_min > max`) into a silent-empty or nonsense result.
- **1DPM comes from minute-truncated timestamps.** Gauges are stamped at `(now-settle).Truncate(1m)`
  so two polls in the same wall-clock minute share a timestamp and Mimir dedups. The FETCH still uses
  the precise `now-settle` bound.
- **Per-endpoint independent.** `ai-models` (always on), one `metadata/<key>` per configured
  `settings.metadata_keys`, and the prompt dimension (`emit_prompts`, on by default). A failed
  endpoint emits nothing for itself, fires `Deps.OnGraphSkipped`, and does NOT block the others. Only
  when EVERY endpoint fails does `Collect` error.
- **All-or-nothing across pages.** `current_page` is 0-indexed; discard the whole endpoint's set on
  any page error, non-200, `is_quota_exceeded` or parse failure - it is a snapshot, so re-fetching
  next poll is free. Terminate on an empty or short page; the page cap
  (`ceil(max_groups/page_size)`) is the offset-ignoring-server backstop. **Do not add an early break
  on "no new dimension value"** - that silently truncates if the metadata shape collapses values.
- `groupMetricName` produces `<prefix>_<metric>_by_<dim>`, deliberately distinct from the analytics
  aggregate names so they never collide (M7).
- **Portkey's cost field is USD CENTS**, divided by 100 on emit as `<prefix>_cost_usd_by_*`.
  `emit_cost` is on by default and doubles the series count on the ai-models endpoint, so downstream
  dashboards and Adaptive Metrics rules must expect the `_cost_usd_by_model` name.
- **Metadata VALUES become label values.** `metadata_keys` defaults to empty precisely because
  opting a key in is a PII and cardinality decision; the per-metric budget is the only backstop. A
  `""` dimension value is the unattributed bucket. The `users` dimension is deliberately not
  implemented (per-end-user cardinality).

## Workspace-scope guardrail (`scope.go`)

analytics and groups data is bound to the API key's workspace and is **not request-targetable**:
Portkey ignores the workspace parameter on the analytics endpoints. With
`settings.expected_workspace` set, the loop asserts once, lazily, on first `Collect` that
`GET /analytics/groups/workspace` returns EXACTLY that slug. A too-broad or global key means **refuse
to emit** - loud `Collect` error, no advance, `OnGraphSkipped(loop, "workspace_scope_mismatch")` -
and it recovers without a restart. A transient probe failure is retried rather than raising a false
alarm; no traffic inside the 7d probe window means proceed unverified and re-check.

Set the SAME value on BOTH analytics and groups. `logs_export` is already hard-scoped via
`workspace_id`, which Portkey does respect. Empty means no check.

**analytics reads `expected_workspace` out of its own `settings:` map**, and it is the only knob it
takes that way - everything else on that loop comes from structured `LoopConfig` fields.

## logs_export

A job lifecycle (create, start, poll, download, page), not a GET. It emits `Batch.Logs` to the
gateway `/v1/logs` on the same base and auth as metrics, landing in Loki.

- **State lives in `Watermark.Cursor`** (phase, job id, window bounds, page and byte offsets, poll
  deadline). One non-blocking step per `Collect`; `LoopConfig.Window == 0` so the scheduler
  snapshot-gates it and the real window is `settings.window`. `Watermark.Time` advances only when
  every page of a window is emitted; the same-Time/cursor-change checkpoint relaxation persists
  in-flight progress, and the runner's `Cursor != ""` commit arm is what stops window 1 looping
  forever.
- **Delivery is AT-LEAST-ONCE**, not the metric plane's exactly-once gap-free. In-flight pages resume
  from the cursor by re-downloading the stable S3 object; a job failure or mid-window leader change
  restarts the window at page 0 and may re-emit a page. A completed window is never re-pulled.
- **The `useCase` slug is set on `fieldPolicy` AFTER the `with*` builder chain.** Each builder returns
  a fresh `fieldPolicy{}` literal that does not copy `useCase`, so setting it mid-chain silently
  drops it.
- **Content governance (release gate).** `requested_data` is NOT an egress filter - Portkey injects
  `metadata` (PII) and `portkeyHeaders` (config) regardless. `logs_strip.go` is a default-deny
  allow-list: `Body` is never set, nested objects and arrays under an allowed key are skipped rather
  than stringified, a JSON `null` is dropped rather than emitted as `""`, and unlike the langsmith
  runs strip arrays are never rendered as csv. `source.Guard.SanitizeLogs` is the backstop.
- **The end-to-end content gate lives in `internal/app`:** `TestLogsExportContentLeakConformanceGate`.
  A strip change that only passes this package's tests has not been checked.

Read `reference/logs-export.md` before changing the download path, the strip, or the failure
handling: it holds the SSRF and credential-redaction rules, the chunker and Range-resume mechanics,
the self-metric skip reasons, and the parked-window behaviour.

## Config surfacing

Knobs are package-local `settings` maps - no `internal/config` change. A malformed known key fails
fast; an unknown key warns. Defaults come from `defaultGroupsSettings()` / `defaultLogsSettings()` and
are rendered into the Helm example by `ExampleSource()` and `ExampleSettingsComments()`, so the
example cannot drift. `just gen` regenerates `deploy/helm/values.yaml`; a new knob without it fails
`gen-check`.

## Deeper references

- `reference/logs-export.md` - read before setting a `logs_export` knob (especially the trace-id and
  metadata opt-in keys) or touching the download, strip or failure paths.
