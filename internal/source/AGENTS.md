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
(alert, no advance, F27). Neither crashes; both become self-metrics.

## Guard

One `*Guard` instance is shared across all loop runners and its `seen` map is mutex-protected. It is
default-deny: an empty `AllowLabelKeys` denies **all** label keys (v1 no-label policy, CP-C6).
`Sanitize` never poison-pills - over-budget or denied data is counted and fires `OnNewLabelValue`,
never errored.

`PerSeriesBudget` (config `governance.per_metric_cardinality_budget`, default 10000) is a PER-METRIC
cap on distinct label-sets per metric name, not a global cap; total cardinality is the sum and is far
higher. The real ceiling is the downstream Mimir / Adaptive Metrics limit (DESIGN §7 GS2/GS3, Cdx-M3),
so validate the configured value against the target stack.

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
- `OnBucketRevised`, `OnGraphSkipped` and `OnAuthError(loop, source)` are the injected self-metric
  hooks. `OnAuthError` fires on a 401/403 so a credential failure is its own alertable signal - use
  `source.IsAuthStatus(code)` rather than re-spelling the codes.
