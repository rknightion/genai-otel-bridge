# internal/app - composition root

Wiring only, no business logic. `Build(ctx, cfg, cp, coord, ef, tf, opts)` assembles the app; `Run`
serves health and runs the scheduler under the coordinator. `checkpoint.Checkpointer` and
`coordinate.Coordinator` are injected, and the emitter and the self-telemetry come from two factories,
so `cmd` supplies real ones and tests supply fakes. The guard's per-metric cardinality budget comes from
`cfg.Governance.PerMetricCardinalityBudget`, never a literal.

## Factories (`factories.go`)

- `PipelineTelemetry` is `schedule.Metrics` plus `ObserveUpstreamRequest`, `ObserveEmitRequest` and
  `ObserveEmitPartialReject`. `*selfobs.Metrics` satisfies it, so this package does not import
  `selfobs`. `SeedLoop` stays an optional capability: `Build` type-asserts it on the
  `PipelineTelemetry` value, so an adapter that wraps another value must forward it
  (`helpers_test.go`'s `telemetryAdapter` does).
- `TelemetryFactory(tenant, pipeline)` returns a pipeline's hooks; `main` passes one over
  `metrics.With`. The legacy shape asks once, for `("", "")`, so every label set is today's.
- `EmitterFactory(EmitterSpec, PipelineTelemetry)` builds one emitter. `Build` calls it once, with the
  spec from `emit.telemetry.otlp` and `identity` (`ProductIdentity()`, the same keys the stream-label
  budget counts). `OTLPEmitterFactory(maxBatchBytes, retry, warn)` is the production one: it wires
  the emit-leg observer and the partial-success counter to the telemetry and owns the rate-limited
  partial-reject warning. Its limit key is `partialRejectLimitKey(pipeline, plane)`:
  `partial:<plane>` with no pipeline (the legacy key), `partial:<pipeline>:<plane>` otherwise; with a
  pipeline the warning also carries `tenant` and `pipeline`.
- `Build` builds `source.Deps` itself from the telemetry: the upstream-request observer and the
  settle-exceedance, capability, data-incompleteness and auth-error hooks, forwarded to each source.
- `ValidateConfigFile` passes an inert emitter factory and `noopTelemetry`. `Options` is empty
  for now.

## Build order (two passes, and the order is load-bearing)

1. Build the source registry and register every source type. This is the extension point for a new
   source.
2. `cfg.Validate(reg.Known())` - fails if a configured source type is not registered.
3. Compute the indexed/label allow-list, then reject content-floor keys in it (below). Then ask the
   telemetry factory for the unlabelled hooks and build `source.Deps` from them: a source takes its
   hooks when it is built.
4. **First pass:** build every enabled source and its loops, `source.ValidateOwnership` (no duplicate
   output series), error if zero enabled loops. Loops are built **before** the guard so the per-loop
   content denylist can be keyed by each loop's real identity (`CheckpointKey.String()`), which
   `SanitizeLogs` receives, and so a content opt-in is honoured only on loops that consume it.
5. Build the guard from the now-known per-loop denylists.
6. Ask the emitter factory for the one emitter.
7. **Second pass:** build the loop runners and specs against the shared guard and that emitter.

## Guard composition

- **Global content denylist is `contentDenylist(nil)`: the never-subtractable floor
  (`source.AbsoluteNeverDenyKeys`) plus the *entire* gray backstop, nothing subtracted.** It polices
  the metrics path and is the fallback for a logs loop with no per-loop entry. Per-loop gray-field
  releases live in `DenyFieldKeysByLoop`, so one loop's opt-in can never weaken another loop's
  backstop, and a content opt-in on a metrics loop has no effect on the guard at all.
- A loop's per-loop release is the union of **all three** of its content-governance knobs
  (`settings.extra_record_fields`, `extra_indexed_fields`, `metadata_record_fields`). Considering
  only the first lets a gray key promoted through one of the other two stay deny-dropped while
  looking allow-listed - deny beats allow - which silently eats every affected record. Floor keys
  stay denied regardless of opt-in.
- **The indexed/label allow-list is the unconditional union of every registered vendor package's
  `AllowedLabelKeys()`, not gated on which sources are enabled.** Every such key is content-free by
  declaration and chosen by source code, never derived from upstream data, so a disabled vendor's
  keys widen the default-deny surface with no live-leak path. The keys live in the vendor packages,
  not here - that is the decoupling rule.
- On top of that come the operator's promotions: `governance.allow_label_keys` and each loop's
  `settings.extra_indexed_fields`. The per-loop one is **auto-allow-listed** so a strip-promoted
  indexed attribute cannot be silently dropped by the default-deny guard.
- A content-floor key named in either promotion is rejected fail-fast at Build. `IsContentFloorKey`
  matches by exact key **and** by `gen_ai` content-namespace prefix, so a flattened attribute such as
  `gen_ai.prompt.0.content` is caught too.

## Loki stream-label budget

For each loop implementing the optional `source.IndexedKeyDeclarer` (the logs loops), Build fails
fast if `len(cfg.Identity.ProductIdentity()) + len(IndexedKeys())` exceeds
`governance.max_stream_label_keys`. Loki *silently drops* a stream over its
`max_label_names_per_series`, which the operationally-honest rule forbids, so this must stay a
startup failure rather than an emit-time surprise. The ceiling is re-defaulted at point of use
because struct-built test configs bypass `config.Load`'s defaulting.

## Run

- **A health-server bind failure aborts synchronously.** It cancels `runCtx` and surfaces in the
  return value; it is never swallowed.
- `coord.Run(runCtx, onElected)` blocks until leadership is lost or ctx is cancelled, with
  `setLeader(true)` on entry and `setLeader(false)` on exit. Only the leader runs the scheduler; the
  beat hook feeds `/healthz`.

## Tests

`minimalConfig()` plus fake source and fake OTLP gateway `httptest.Server`s. Tests call
`Build(ctx, cfg, cp, coord, ef, tf, Options{})` with `ef, tf := fixedFactories(em, m)`, which wraps a
fixed emitter and any `schedule.Metrics` (observers are no-ops). The `acceptance` build
tag runs the acceptance gates against a Mimir-model recorder that **400s on a series value change**,
which is what catches value-divergent re-emits: failover handoff contiguity, stale-queue drop on
re-election, empty-window advance.
