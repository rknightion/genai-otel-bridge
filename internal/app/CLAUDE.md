# internal/app — composition root (wiring only, no business logic)

`Build` assembles the app from injected dependencies; `Run` serves health + runs the scheduler under
the coordinator. Dependencies (`checkpoint.Checkpointer`, `coordinate.Coordinator`, `emit.Emitter`,
`schedule.Metrics`) are injected so `cmd/genai-otel-bridge` supplies real ones and tests supply fakes.

```go
func Build(ctx, cfg *config.Config, cp checkpoint.Checkpointer, coord coordinate.Coordinator,
           em emit.Emitter, m schedule.Metrics, deps source.Deps) (*App, error)
func (a *App) Run(ctx, health http.Handler, healthAddr string,
                  markReady, beat func(), setLeader func(bool)) error
```

`deps source.Deps` carries composition-root hooks that aren't config data (today the upstream-request
observer, forwarded to each source's HTTP client). The guard's per-metric cardinality budget comes from
`cfg.Governance.PerMetricCardinalityBudget` (config-keyed, default 10k) — not a literal.

## Wiring order & rules

1. Build the source registry + register all source types (`portkey.Register`) — extension point for new
   sources.
2. `cfg.Validate(registry.Known())` — fails if a configured source type isn't registered.
3. Build the cardinality Guard: **content denylist** = `contentDenylist(optedInContentFields(cfg))` — the
   never-subtractable FLOOR (`source.AbsoluteNeverDenyKeys`, enumerated in `internal/source/CLAUDE.md`;
   defence beyond minimisation, Cdx-H7) PLUS the gray backstop tier MINUS any field a loop opted in via ANY
   of its three content-governance knobs (`settings.extra_record_fields` ∪ `extra_indexed_fields` ∪
   `metadata_record_fields`), so a default deployment keeps the full backstop and only explicitly-opted-in
   gray fields are released (review HIGH-1). Considering all three knobs (not just `extra_record_fields`) is
   what stops a gray key promoted via `extra_indexed_fields`/`metadata_record_fields` from being
   auto-allow-listed yet still deny-dropped — deny beats allow — which silently ate every affected record
   (#51). Floor keys are denied regardless of opt-in. The **indexed/label allow-list** is the UNCONDITIONAL
   union of EVERY registered vendor package's `AllowedLabelKeys()` — NOT gated on which sources are enabled
   (#75: every key is content-free by declaration and chosen by source code, never from upstream data, so a
   disabled vendor's keys widen the default-deny surface with no live-leak path). The keys live in the
   vendor packages, not hardcoded here — decoupling. PLUS the
   operator's promotions: `governance.allow_label_keys` (top-level) and each loop's
   `settings.extra_indexed_fields` (per-loop, gathered by `optedInIndexedFields` and AUTO-allow-listed so
   a strip-promoted indexed attr can't be silently dropped). A content-floor key named in either promotion
   is rejected fail-fast (Guard is otherwise default-deny, CP-C6).
4. Build enabled sources → `ValidateOwnership` (no duplicate output series) → loop runners → specs.
   Error if zero enabled loops. **Loki stream-label budget:** for each loop implementing the optional
   `source.IndexedKeyDeclarer` (the two logs loops), fail fast if `len(cfg.Identity.ProductIdentity())` +
   `len(IndexedKeys())` > `governance.max_stream_label_keys` (default 15 = GC Loki
   `max_label_names_per_series`; tenant-overridable) — Loki silently drops a stream over the limit. The
   ceiling is re-defaulted at point-of-use (struct-built test configs bypass `config.Load`'s defaulting).

## Gotchas

- **Health-server bind failure must abort synchronously** (CP-R3) — it cancels `runCtx` and surfaces in
  the return, never swallowed.
- `coord.Run(runCtx, onElected)` blocks until leadership lost or ctx cancelled; `setLeader(true)` on
  entry, `setLeader(false)` on exit. Only the leader runs the scheduler; the beat hook feeds `/healthz`.

Tests: `minimalConfig()` helper; fake Portkey + fake OTLP gateway via `httptest.Server`. The
`acceptance` build tag (`acceptance_test.go`, `recorder_test.go`) runs §9 gates with a Mimir-model
recorder that **400s on a series value change** — catching value-divergent re-emits (failover handoff
contiguity, stale-queue drop on re-election, empty-window advance).
