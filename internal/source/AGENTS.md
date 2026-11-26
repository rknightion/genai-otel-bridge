# internal/source

The pluggable-source seam and the governance guard. Vendor packages live in subpackages and register
themselves here.

## Adding a source

New package under `internal/source/<vendor>/`, exposing `New(config.SourceConfig, source.Deps)` and a
`Register(*source.Registry)` wired into the composition root (`internal/app`). Validate at
construction: an unknown graph or option fails fast, never silently no-ops.

- **A duplicate type string PANICS at `Registry.Register`** (#133). Registration runs once at
  composition time, so a copy-pasted type string would otherwise silently shadow the original vendor
  and every configured source of that type would build the wrong loops.
- **Every loop MUST implement `SeriesDeclarer`, including logs-only loops** (#63). They declare an
  empty slice. A non-declaring loop is silently exempt from `ValidateOwnership`, and a
  post-normalisation series collision then surfaces only at runtime as an endless stream of
  duplicate-timestamp rejects with nothing naming the collision as the cause.
- A logs loop that promotes fields to `LogRecord.IndexedAttributes` also implements
  `IndexedKeyDeclarer`, returning the FULL set it may emit (base allow-list plus
  `settings.extra_indexed_fields`). The composition root sums those against
  `governance.max_stream_label_keys`; a stream over Loki's limit is dropped silently by Loki, so this
  is the only place that catches it.

## Ownership is checked post-gateway

`ValidateOwnership` normalises through `NormalizeSeriesName` (dots to `_`, lowercase), mirroring the
gateway's metric-name transform, to catch duplicate output series that only collide after
normalization (M7). It does NOT yet model the gateway's unit-suffixing or `_total` appending
(AR-H-F): two names differing only by a suffix the gateway would add can still collide post-gateway.
Extend it when unit-suffixed or counter names are added.

## Sentinel errors map to scheduler behaviour

`ErrQuotaExceeded` (discard batch, no advance, back off, F3/F34) and `ErrGranularityUnexpected`
(alert, no advance, F27). Neither crashes; both become self-metrics. `ErrAuth` marks a 401/403: wrap it
with `source.WrapAuth(code, err)` into the retryable Collect error (it stays a counted collect failure)
and the scheduler stretches the wait as for quota [#537].

## Scope gate (`scope.go`, design §5.1 and §7)

`ScopeAsserter` is an optional capability a source implements when it can report what its API key
reads: `ScopeMatched`, `ScopeMismatch` or `ScopeUnverifiable`, or an error for a failed call. A 401 or
403 is returned as `*AuthError{Status}`; the asserter never calls `Deps.OnAuthError`, because the
scheduler raises the auth signal for it. No source implements it yet (unit 4b, #526).

`ScopeGate` is the vendor-neutral policy, one per pipeline, shared by every loop of the pipeline and
asked by the scheduler before each collect (`Before` returns `ScopeProceed`, `ScopeWait` or
`ScopeMismatched`, or an error for a failed call). It holds one in-memory state (no result, matched,
unverified, mismatched) and makes one assertion call at a time: the mutex is held across the call, so
concurrent ticks wait for the first call's result rather than calling again.

- Re-assertion is due one interval (10 minutes) after a fresh match; while unverified or mismatched,
  or after a failed call, at most one call a minute. `ScopeUnverifiable` keeps a match. A failed call
  keeps a match for two intervals after the last successful assertion or restored match, then is a
  failure (the ticks between its calls wait). Only `ScopeMatched` ends a mismatch. None of these is a
  config key. A call cut short by the leader context's cancellation changes and counts nothing.
- `Elected(ctx)` resets the gate and reads the remembered match at `ScopeMatchKey(sourceInstance)`
  (`<source instance>/scope-match/v1`; the loop name `scope-match` is reserved). A stored digest equal
  to the configured `ScopeMatchDigest(KeyFingerprint(header), name, id)` starts the gate matched with
  its first re-assertion at `Jitter(10m)`; anything else, or a read error, starts it with no result.
  `Reset()` sets no result. Both report the state through `OnState` (`""` is no result).
- Writes go through `ScopeMatchStore` (a `checkpoint.Checkpointer` satisfies it), with the leader
  epoch from the context. `Time` is the later of now and one second after the stored time the gate
  holds, so `checkpoint.CheckMonotonic` never refuses it when this leader's clock is behind the last
  one's. A match is written on `ScopeMatched` when the stored digest differs; an empty `Cursor` on every
  `ScopeMismatch` whose stored digest is not already empty. The gate's view of the store changes only
  after a write succeeds, so a refused write is tried again at the next such result. Until a read or
  write succeeds the view is unknown, and both writes are made.
- `KeyFingerprint` and the digest are never logged or emitted.

## Guard

One `*Guard` instance per group is shared across the loop runners of that group (the rows that share a
stack and an identity; a legacy install is one group, so one guard), and its `seen` map is
mutex-protected. It is
default-deny: an empty `AllowLabelKeys` denies **all** label keys (v1 no-label policy, CP-C6).
`Sanitize` never poison-pills - over-budget or denied data is counted and fires `OnNewLabelValue`,
never errored.

`PerSeriesBudget` (config `governance.per_metric_cardinality_budget`, default 10000) is a PER-METRIC
cap on distinct label-sets per metric name, not a global cap; total cardinality is the sum and is far
higher. The real ceiling is the downstream Mimir / Adaptive Metrics limit (DESIGN §7 GS2/GS3, Cdx-M3),
so validate the configured value against the target stack.

**The budget counts ACTIVE label sets, not every label set ever seen (#383).** Each admitted signature
keeps its last-seen time (read from `GuardConfig.Now`, nil meaning `time.Now`, so tests inject a fake
clock). A known signature is always admitted and refreshed. A new one is admitted while fewer than budget
signatures are held; when the set is full, signatures idle for `seenIdleWindow` (a fixed hour, not a
config knob) are evicted first, and only then is the new one dropped. The same rule applies to the logs
budget keyed on `"logs:" + CheckpointKey.String()`.

- Memory stays bounded: a signature is inserted only while the set holds fewer than budget entries, so
  each metric name or logs loop holds at most budget signatures and their times.
- Eviction runs only when the set is full and a new label set arrives, and is skipped until the oldest
  survivor of the previous sweep can have expired, so the O(budget) sweep is not paid per sample.
- A set under budget is never swept, so a label set returning after a long idle spell is still "known"
  there and does not fire `OnNewLabelValue` again. After an eviction it is new again and fires it.
  `OnNewLabelValue` still fires on every new admission and on every over-budget drop.
- The hour must exceed every loop's poll cadence by a margin (default cadence 60s). A loop polling less
  often than hourly sees its label sets expire between polls: the budget then counts one poll's label
  sets, which is still the active count.
- State is in-process only: a restart or a new leader starts empty, which with active semantics only
  re-admits what is still live.

**A budget of 0 means UNLIMITED in the guard**, not "block everything". `internal/config` therefore
defaults an unset `per_metric_cardinality_budget` to 10000 and never passes 0 through. Keep that
mapping if you add another path into `GuardConfig`.

**The OTel SDK's cardinality limit does not protect the product plane.** That feature only applies to
the SDK aggregation pipeline; the product data plane uses the hand-rolled `emit/otlp` encoder and
bypasses it entirely, so `Guard.PerSeriesBudget` is the only cardinality control there. (`internal/selfobs`
does use the SDK, but is low-cardinality.)

Decision for when higher-cardinality labels get allow-listed: collapse over-budget series into a
single `otel.metric.overflow=true` bucket **only for additive series** (request/error/token counts).
Quantile and latency gauges stay drop-with-counter - summing p99s across collapsed series is
meaningless. Use the standard `otel.metric.overflow` attribute, not a homegrown sentinel.

## Content floor - `content.go` owns the canonical list

`AbsoluteNeverDenyKeys()` is the never-subtractable FLOOR of the content denylist: message-body and
injected-PII field keys that must never egress and that no `extra_record_fields` opt-in can release.
**It is the single source of truth** - `internal/app` and the vendor packages reference it by name and
must not re-enumerate it.

**Match with `IsContentFloorKey`, never with an exact-match loop of your own.** The `gen_ai` entries
are matched by PREFIX so flattened content forms (`gen_ai.prompt.0.content`) are caught; using the
raw list gives exact-match behaviour and lets the guard layer and the source opt-in validators drift
apart (#97). The prefixes are deliberately scoped to the content namespaces, so content-free
operational attrs (`gen_ai.request.*`, `gen_ai.usage.*`) are NOT covered.

Effective denylist wired in `internal/app` = this floor + a gray backstop tier - fields a loop opted
into its record allow-list. Floor keys are denied regardless of opt-in (Cdx-H7).

## `Deps`

Cross-cutting composition-root hooks that are not YAML data, passed alongside `config.SourceConfig`.
Every hook's zero value is a no-op, so tests pass `Deps{}`. Add future cross-cutting dependencies
(tracer, logger) here rather than widening the constructor again.

- `UpstreamObserver` is wired into the source's `httpx` client so every outbound call feeds the
  self-obs upstream-latency histogram. It is passed here, not imported, so `httpx` and
  `internal/selfobs` never import each other.
- `OnBucketRevised`, `OnCapability`, `OnDataIncomplete` and `OnAuthError(loop, source)` are the
  injected self-metric hooks. Capability `state` and incompleteness `reason` are closed types from
  `capability.go`; call sites pass their constants, never vendor or config strings. `OnAuthError`
  fires on a 401/403 so a credential failure is its own alertable signal - use
  `source.IsAuthStatus(code)` rather than re-spelling the codes.
