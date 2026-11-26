# internal/app - composition root

Wiring only, no business logic. `Prepare(cfg)` registers the source types, validates the config and
resolves it into a `tenancy.Plan`; `BuildFromPlan(ctx, cfg, plan, cp, coord, ef, tf, opts)` assembles
the app from that plan, one pipeline per row; `Build(ctx, cfg, cp, coord, ef, tf, opts)` is the two in
turn. `main` calls `Prepare` before its startup summary and then `BuildFromPlan`; the release gate
calls `BuildFromPlan` with rows it constructs. `Run` serves health and runs the scheduler under the
coordinator. `checkpoint.Checkpointer` and
`coordinate.Coordinator` are injected, and the emitter and the self-telemetry come from two factories,
so `cmd` supplies real ones and tests supply fakes. The guard's per-metric cardinality budget comes from
`cfg.Governance.PerMetricCardinalityBudget`, never a literal.

## Factories (`factories.go`)

- `PipelineTelemetry` is `schedule.Metrics` plus `ObserveUpstreamRequest`, `ObserveEmitRequest`,
  `ObserveEmitPartialReject` and the scope gate's two signals, `ScopeState` and `ScopeAssertion`
  (`noopTelemetry` has no-op ones). `*selfobs.Metrics` satisfies it, so this package does not import
  `selfobs`. `SeedLoop` stays an optional capability: `BuildFromPlan` type-asserts it on each row's
  `PipelineTelemetry` value, so an adapter that wraps another value must forward it
  (`helpers_test.go`'s `telemetryAdapter` does).
- `TelemetryFactory(tenant, pipeline)` returns a pipeline's hooks; `main` passes one over
  `metrics.With`. `BuildFromPlan` asks once per distinct `(tenant, pipeline)` and reuses the value, so
  the legacy shape asks once, for `("", "")`, and every label set is today's.
- `EmitterFactory(EmitterSpec, PipelineTelemetry)` builds one emitter. `BuildFromPlan` calls it once
  per row (pass 3), with the row's destination (endpoint, instance id, token) and
  `pipelineIdentity(row)`: the row's identity map plus `genai_otel_bridge.pipeline` when the row has a
  pipeline id. A legacy row's identity is `ProductIdentity()` and it has no pipeline id, so its emitter
  stamps exactly what it did before. The stream-label budget counts the same map. `OTLPEmitterFactory(maxBatchBytes, retry, warn)` is the production one: it wires
  the emit-leg observer and the partial-success counter to the telemetry and owns the rate-limited
  partial-reject warning. Its limit key is `partialRejectLimitKey(pipeline, plane)`:
  `partial:<plane>` with no pipeline (the legacy key), `partial:<pipeline>:<plane>` otherwise; with a
  pipeline the warning also carries `tenant` and `pipeline`.
- `sourceDeps(t, authLim)` builds a row's `source.Deps` from its telemetry: the upstream-request
  observer and the settle-exceedance, capability, data-incompleteness and auth-error hooks. One
  auth-warning limiter serves every row; its key carries the source instance.
- `ValidateConfigFile` validates and resolves the plan itself, then passes it to `BuildFromPlan`
  with an inert emitter factory and `noopTelemetry`. A tenant-shape plan is assembled with
  `placeholderSecrets` (no file is read; a value distinct per tenant and file), so every assemble
  rule that needs no secret (scope assertion support, the reserved `scope-match` loop, no loop that
  emits log records, per-group series ownership) fails the check, its error returned as it is. A
  tenant-shape plan that assembles returns its table and hash with no error, as a legacy plan does.
  An unreadable secret and a duplicate API key are found only when a pod reads the files.
- `Options.Secrets` (`tenancy.SecretReader`) reads the secret files a row names. A legacy row names
  none, so `nil` is fine for it. `Options.Now` is the clock of every scope gate (nil: `time.Now`).
  `Options.OnTenantBlocked(tenant, reason, blocked)` is called for every tenant of the plan and both
  reasons (`secret_unreadable`, `duplicate_key`), in tenant order; `main` passes
  `selfobs.Metrics.TenantBlocked`. A legacy plan has no tenant, so it is never called.

## Build order (three passes, and the order is load-bearing)

1. `newRegistry()` registers every source type. This is the extension point for a new source;
   `Prepare` validates against the same registry (`knownSourceTypes`), and only `Prepare` runs
   `cfg.Validate`. `BuildFromPlan` reads only install-wide settings (governance, queue) from `cfg`.
2. Reject a content-floor key in `governance.allow_label_keys` or any row's
   `extra_indexed_fields` (below), before any source is built.
3. **Pass 1, secrets, over the whole plan** (`readPlanSecrets`, `pipeline.go`; design §5.5, §9):
   every row's secret files are read through `opts.Secrets` onto a copy of the row
   (`readRowSecrets`: `AuthValueFile` into `Source.Auth.Value`, `TokenFile` into
   `Destination.Token`; both are tried, and a read error, an empty value or a nil reader is an error
   naming the tenant and the file, never a value). A tenant row is one with a tenant id:
   - a tenant with any unreadable secret is blocked (`secret_unreadable`);
   - the `source.KeyFingerprint` of every API key read from a file is compared across every target of
     every tenant, a blocked tenant's included, and every tenant with a target whose fingerprint
     another target also has is blocked with all its targets (`duplicate_key`). Fingerprints are
     compared, never logged or returned;
   - one error line per unreadable tenant (tenant and the reader's error) and one per duplicate set
     (`pipelines`, the set's pipeline ids only); `opts.OnTenantBlocked` for every tenant and reason;
   - a blocked tenant's rows are not built at all (no telemetry, emitter or loop); when every tenant
     is blocked the build fails with each tenant's reasons (tenant, file, pipeline ids).
   A legacy row names no file; one that did and could not read it still fails the build, and legacy
   rows are never compared for duplicate keys (G6). `TestTenant*`, `TestDuplicateKey*` and
   `TestAllTenantsBlockedFails` in `tenant_assembly_test.go` cover this.
   **Pass 1, per row that is built:** the row's telemetry, its `source.Deps` (a source takes its
   hooks when it is built), its source and loops, the loop-name typo check, the row's scope gate
   (`rowScopeGate`, `pipeline.go`) and the stream-label budget. `rowScopeGate` refuses, in every row, a
   loop named `scope-match` (the reserved remembered-match loop name, design §7). A row with no scope
   (a zero `Source.Scope`, every legacy row) gets no gate. A row with a scope (a name or an id) fails
   unless its source is a `source.ScopeAsserter`, so a scope is never collected unchecked, and gets one
   gate: `Store` the checkpointer, `Digest` `ScopeMatchDigest(KeyFingerprint(Auth.Value), name, id)` of
   the key read in this pass, `Now` `opts.Now`, and its hooks the row's `ScopeAssertion` and
   `ScopeState`. Then `refuseLogsLoops`: a row with a pipeline id (every tenant-shape row) fails if
   any of its loops implements `source.IndexedKeyDeclarer` (a loop that emits log records, design §2).
   Loops are built **before** the guards so the per-loop content denylist can be keyed by each loop's
   real identity (`CheckpointKey.String()`), which `SanitizeLogs` receives.
4. **Pass 2, per group** of rows sharing a stack (`tenancy.StackKey` of endpoint and instance id)
   and an identity map (`groupPipelines`, first-seen order): `source.ValidateOwnership` over the
   group's sources, then (after the zero-enabled-loops error) one guard per group. Two groups write to
   different stacks or identities, so they may declare the same metric name. A legacy plan is one
   group: one ownership check and one guard, as before.
5. **Pass 3, per row** (`pipeline.go`, the only production caller of `schedule.NewLoopRunner`): one
   emitter from the row's destination, identity and telemetry, and one runner and `LoopSpec` per
   loop with `Metrics` the row's telemetry, `Pipeline` the row's tenant and pipeline id
   (`WithPipeline`) and `Gate` the row's one gate (nil with no scope). Nothing at run time chooses a destination.
6. The `ha.coordinator=none` epoch auto-heal over every row's loops, then seeding: `SeedLoop` is
   asserted on each row's telemetry and seeds that row's loops only.

## Guard composition

- **One guard per group (pass 2).** Its allow-list is the vendor keys, `governance.allow_label_keys`
  and that group's own `extra_indexed_fields`; its per-loop deny map covers that group's loops; its
  `OnNewLabelValue` is the group's first row's telemetry (in the tenant shape a group is one
  pipeline; in the legacy shape every row has the unlabelled telemetry).
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
- A content-floor key named in either promotion, in any row, is rejected fail-fast at assembly. `IsContentFloorKey`
  matches by exact key **and** by `gen_ai` content-namespace prefix, so a flattened attribute such as
  `gen_ai.prompt.0.content` is caught too.

## Loki stream-label budget

For each loop implementing the optional `source.IndexedKeyDeclarer` (the logs loops), assembly fails
fast if `len(pipelineIdentity(row)) + len(IndexedKeys())` (for a legacy row,
`len(cfg.Identity.ProductIdentity())`) exceeds
`governance.max_stream_label_keys`. Loki *silently drops* a stream over its
`max_label_names_per_series`, which the operationally-honest rule forbids, so this must stay a
startup failure rather than an emit-time surprise. The ceiling is re-defaulted at point of use
because struct-built test configs bypass `config.Load`'s defaulting.

## Run

- **A health-server bind failure aborts synchronously.** It cancels `runCtx` and surfaces in the
  return value; it is never swallowed.
- `coord.Run(runCtx, onElected)` blocks until leadership is lost or ctx is cancelled, with
  `setLeader(true)` on entry and `setLeader(false)` on exit. Each entry first logs `leader elected`
  with `plan_hash` and `plan_rows` (design §5.4), so a leader's plan can be compared with the hash
  `-validate-config` printed. Only the leader runs the scheduler; the
  beat hook feeds `/healthz`.

## Tests

`minimalConfig()` plus fake source and fake OTLP gateway `httptest.Server`s. Tests call
`Build(ctx, cfg, cp, coord, ef, tf, Options{})` with `ef, tf := fixedFactories(em, m)`, which wraps a
fixed emitter and any `schedule.Metrics` (observers are no-ops). `assembly_test.go` builds hand-made
plan rows through `BuildFromPlan` with a recording emitter factory (`specRecorder`). A test that needs a
source no vendor package provides sets the package variable `testSourceTypes` (and resets it in
`t.Cleanup`); production never sets it. The
`acceptance`-tagged `TestLegacyPayloadGolden` pins a legacy install's OTLP bytes in
`testdata/legacy-payload.bin` (generated with `-update` on the unit 3b-1 commit; regenerate only for a
deliberate wire change). The `acceptance` build
tag runs the acceptance gates against a Mimir-model recorder that **400s on a series value change**,
which is what catches value-divergent re-emits: failover handoff contiguity, stale-queue drop on
re-election, empty-window advance.

The cross-tenant release gate (design §5.3, a hard rule in the root `AGENTS.md`) is
`crosstenant_gate_test.go` with its helpers in `crosstenant_helpers_test.go`, both `acceptance`-tagged:
`CGO_ENABLED=1 go test -race -tags acceptance ./internal/app/ -run TestCrossTenant`. It builds tenant
rows by hand through `BuildFromPlan` and `OTLPEmitterFactory`, every destination and the self-telemetry
on one fake receiver's URL, and drives each leadership with one `RunOnceForTest` tick per loop. Its
four checks (delivery, slug, self endpoint, self identity) return findings; `TestCrossTenantScenarios`
needs none in any scenario, and `TestCrossTenantSeededFaults` needs each seeded fault reported by
exactly the checks it names. A new seeded fault is another `faultCase`; unit 6b-2 added the duplicate key (tenant-b's key
file holds tenant-a's key: both unbuilt, both reported `duplicate_key` on `OnTenantBlocked` and the
self `tenant_blocked`, tenant-c delivers). `TestCrossTenantFromFixture` runs the gate on plans from
`internal/config/testdata/tenants.yaml` and `tenants-reordered.yaml` through `config.Load` and
`tenancy.Resolve`, with only each row's base URL and endpoint rewritten to the fakes (and private
addresses allowed): both orders need one plan hash and the same deliveries. A credential may be held
by two pipelines (two targets of one tenant on one destination); the delivery check then judges each
resource by the pipeline id it carries.

`tenant_golden_test.go` holds the tenant shape's G8 goldens as literals, from the fixture's
`team-a.prod`: the pipeline id, the checkpoint keys (`mt.team-a.prod/analytics/<fp>`, `.../groups/<fp>`,
`mt.team-a.prod/scope-match/v1`), the OTLP bytes of one analytics cycle
(`testdata/tenant-payload.bin`, regenerated only with `-update-tenant-payload` for a deliberate wire
change) and the label set of every self-metric recorded through `With("team-a", "team-a.prod")`. Their
ConfigMap data keys are in `internal/checkpoint/configmap/datakey_test.go`. A change to any is a
migration, not a test update.

`isolation_test.go` (`TestIsolation*`) drives three pipelines on simulated time (`isoStart` and `step`:
`Options.Now`, the loop clocks and `RunOnceForTest` move together, fast retry policy) and fails one: a
destination that answers 503 for ten minutes, a revoked key (35 minutes, longer than a matched gate's
grace) or a key that reads another workspace. Every other pipeline's `window_lag` must stay within settle
plus two cadences. The faulty pipeline is run first and last in the plan, because a leak by sharing one
emitter or one gate shares the first row's. `acceptance_multi_test.go` (`TestMultiPipeline*`) runs the three
single-pipeline acceptance scenarios again with three pipelines, each with its own vendor values and its
own Mimir-model recorder, over one checkpoint file.
