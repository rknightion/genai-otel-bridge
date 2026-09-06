# internal/app - composition root

Wiring only, no business logic. `Build` assembles the app from injected dependencies; `Run` serves
health and runs the scheduler under the coordinator. `checkpoint.Checkpointer`,
`coordinate.Coordinator`, `emit.Emitter` and `schedule.Metrics` are injected so `cmd` supplies real
ones and tests supply fakes.

`deps source.Deps` carries composition-root hooks that are not config data (the upstream-request
observer, the settle-exceedance, graph-skip and auth-error hooks), forwarded to each source. The
guard's per-metric cardinality budget comes from `cfg.Governance.PerMetricCardinalityBudget`, never a
literal.

## Build order (two passes, and the order is load-bearing)

1. Build the source registry and register every source type. This is the extension point for a new
   source.
2. `cfg.Validate(reg.Known())` - fails if a configured source type is not registered.
3. Compute the indexed/label allow-list, then reject content-floor keys in it (below).
4. **First pass:** build every enabled source and its loops, `source.ValidateOwnership` (no duplicate
   output series), error if zero enabled loops. Loops are built **before** the guard so the per-loop
   content denylist can be keyed by each loop's real identity (`CheckpointKey.String()`), which
   `SanitizeLogs` receives, and so a content opt-in is honoured only on loops that consume it.
5. Build the guard from the now-known per-loop denylists.
6. **Second pass:** build the loop runners and specs against the shared guard.

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

`minimalConfig()` plus fake source and fake OTLP gateway `httptest.Server`s. The `acceptance` build
tag runs the acceptance gates against a Mimir-model recorder that **400s on a series value change**,
which is what catches value-divergent re-emits: failover handoff contiguity, stale-queue drop on
re-election, empty-window advance.
