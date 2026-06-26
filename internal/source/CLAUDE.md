# internal/source — Source interface, registry, cardinality Guard

The pluggable-source seam. `source.go` defines the interfaces; `guard.go` enforces cardinality/label
governance; concrete sources live in subpackages (`portkey/`).

## Interfaces (FROZEN — do not add methods)

```go
type Source interface { ID() string; Loops() []Loop }
type Loop interface {
    Key() model.CheckpointKey
    Cadence() time.Duration
    Collect(ctx context.Context, since model.Watermark) (model.Batch, error)
}
type SeriesDeclarer interface { SeriesNames() []string }       // optional capability (ownership check)
type IndexedKeyDeclarer interface { IndexedKeys() []string }   // optional: logs loops, Loki stream-label budget
type Constructor func(config.SourceConfig) (Source, error)
```

## Adding a new source

1. New package `internal/source/<vendor>/`.
2. Implement `Source` + one or more `Loop`s; optionally `SeriesDeclarer`.
3. Expose `func Register(reg *source.Registry)` and `func New(cfg config.SourceConfig) (source.Source, error)`.
4. Wire `Register` into the composition root (`internal/app`). Build via `Registry.Build`.
5. Validate at construction (unknown graph/option → fail fast, don't silently no-op).

## Invariants & gotchas

- **Sentinel errors map to scheduler behaviour** — `ErrQuotaExceeded` (discard batch, no advance, back
  off, F3/F34) and `ErrGranularityUnexpected` (alert, no advance, F27). Neither crashes; both become
  self-metrics.
- **Ownership is checked post-gateway.** `ValidateOwnership(sources)` uses `NormalizeSeriesName`
  (dots→`_`, lowercase) — kept in sync with the gateway's metric-name transform — to catch duplicate
  output series across sources that only collide *after* normalization (M7).
- **Guard is shared and default-deny.** One `*Guard` instance is shared across all loop runners; its
  `seen` map is mutex-protected (see `race_test.go`). Empty `AllowLabelKeys` denies **all** labels
  (v1 no-label policy, CP-C6). `Sanitize` never poison-pills — over-budget/denied data is counted +
  fires `OnNewLabelValue`, never errored. `DenyFieldKeys` blocks content fields.

## Content floor — `AbsoluteNeverDenyKeys` (canonical list, owned here in `content.go`)

The never-subtractable denylist FLOOR: message-body + injected-PII field keys that must NEVER egress and
cannot be released by any `extra_record_fields` opt-in. **This is the single source of truth** —
`internal/app`, `internal/source/langsmith`, `internal/source/portkey` reference it by name, don't re-enumerate.

`gen_ai.*`, `input.value`, `output.value`, `request`, `response`, `inputs`, `outputs`, `messages`,
`metadata`, `portkeyHeaders`.

Effective denylist (wired in `internal/app`) = this floor + a gray backstop tier − fields a loop opted into
its record allow-list. Floor keys are denied regardless of opt-in (defence beyond minimisation, Cdx-H7).

Tests: focused cases (allow-list, deny-list, budget), explicit hook verification, and a concurrency
race test on shared `Sanitize`. Not heavily table-driven.

## Composition deps (`Deps`)

`Constructor`/`Registry.Build` take a `source.Deps` alongside `config.SourceConfig` — cross-cutting
hooks that aren't YAML data. Today it carries `UpstreamObserver httpx.Observer` (wired into the
`httpx.Client` so outbound calls feed the self-obs `genai_otel_bridge_upstream_request_duration_seconds` histogram)
plus the injected self-metric hooks `OnBucketRevised`/`OnGraphSkipped`/`OnAuthError(loop,source)` — the
last fires on a 401/403 (use `source.IsAuthStatus(code)`) → `genai_otel_bridge_auth_errors_total{loop,source}`, so a
credential failure is its own alertable signal. Every hook's zero value is a no-op, so tests pass
`Deps{}`. Add future cross-cutting deps (tracer, logger) here rather than widening the constructor again.

## Cardinality follow-ups

From an external review (2026-06):

- **Per-metric budget is config-keyed (default 10k), DONE.** `governance.per_metric_cardinality_budget`
  → `GuardConfig.PerSeriesBudget` (was a hardcoded `1000` in `internal/app/app.go`). It is a PER-METRIC
  cap (distinct label-sets per metric name), not a global cap — total cardinality is the sum and is far
  higher. Unset ⇒ 10000 (0 would mean unlimited in the guard, so config never passes 0 through). The
  *real* ceiling is the **downstream Mimir / Adaptive Metrics** limit (DESIGN §7 GS2/GS3, Cdx-M3), not
  our guard — validate 10k against the target stack.
- **OTel SDK cardinality limit does NOT bite the product plane.** The SDK's cardinality-limit/overflow
  feature only applies to the SDK aggregation pipeline; the product data plane uses the **hand-rolled
  `emit/otlp` encoder**, which bypasses it entirely — so our `Guard.PerSeriesBudget` is the only
  cardinality control there. (The Go SDK's limit is also experimental + opt-in via
  `OTEL_GO_X_CARDINALITY_LIMIT`, off by default, and *overflows* — `otel.metric.overflow=true` — rather
  than silently dropping.) The selfobs path *does* use the SDK but is low-cardinality.
- **Overflow-collapse instead of drop (future).** Today over-budget series are dropped (counted via
  `genai_otel_bridge_guard_dropped_total`). When higher-cardinality labels get allow-listed (beyond v1's
  `quantile`-only), collapse over-budget series into a single `otel.metric.overflow=true` bucket to
  preserve the aggregate — **but only for additive series** (request/error/token counts). Quantile/
  latency *gauges* stay drop-with-counter: summing p99s across collapsed series is meaningless. Use the
  standard `otel.metric.overflow` attribute, not a homegrown sentinel. Pairs with the overflow-present
  alert in the self-obs mixin.
