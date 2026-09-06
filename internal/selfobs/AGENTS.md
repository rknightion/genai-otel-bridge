# internal/selfobs

The bridge's own metrics, health, profiling and tracing. Self plane only - it never carries product
(Portkey/LangSmith) data.

## Decoupling

`selfobs` does NOT import `internal/config`. Every config struct here (`ProviderConfig`,
`ProfilingConfig`, `TracingConfig`) is selfobs-owned and `main.go` maps the YAML into it. Keep new
config that way rather than importing `config`.

Unlike the product data plane (hand-rolled `emit/otlp` encoder), this package uses the real OTel-Go
SDK, so SDK features (cardinality limits, views) apply here and only here.

## Self identity (H4)

- Separate `ServiceNamespace` (product `genai-otel-bridge` to self `genai-otel-bridge-meta`) so
  self-telemetry never mixes with republished product series.
- `service.instance.id = POD_NAME` (CP-H8) to diagnose leader/standby overlap.
- Meter and tracer scope `genai-otel-bridge/selfobs`; metric names carry the `genai_otel_bridge_`
  prefix.
- Config `endpoint` is the BASE URL; `/v1/metrics` and `/v1/traces` are appended inside
  `NewProvider` / `NewTracerProvider` (CP-H9), identically to the product emitter, so they cannot
  diverge on path. Traces ride the SAME gateway and creds as self-metrics - no separate egress channel.

## `signals.go` is a gate, not documentation

It is the static catalogue the docs generator renders into `docs/telemetry.md`, and
`TestSelfObsSignalsParity` fails when an instrument in `metrics.go` has no descriptor or vice versa.
Adding or renaming an instrument means editing `signals.go` and running `just gen` in the same change.

## 1DPM clamp (self plane)

`NewProvider` clamps an explicitly configured PeriodicReader interval up to the floor `60s/max_dpm`.
The SDK emits exactly one point per interval, so this clamp is the whole self-plane DPM cap - there
is no coalesce stage.

An unset interval (`emit.self.metric_interval` absent, and the `emit.self=nil` fallback) resolves to
a flat 60s default, NOT the floor. 60s already satisfies the cap for every `max_dpm>=1`, so raising
`governance.max_dpm` to widen the product plane never silently multiplies the self-plane export rate.
Only a configured sub-floor value is clamped up, and that clamp is logged.

## Traps

- **Schemaless resource (AR-C5).** Use `resource.NewSchemaless()` with raw OTLP key strings. Do not
  use `resource.Merge(resource.Default(), ...)`: it returns `ErrSchemaURLConflict` (fatal at startup)
  on schema mismatch and couples the package to semconv version churn.
- **Health is leadership-aware.** `/readyz` is 503 until `MarkReady()`. `/healthz` is 200 for a
  standby (never judged on heartbeat - it is not running the scheduler) and for a leader with a fresh
  beat; a leader past the stale threshold is 503. `Beat()` records an *attempt*, so a leader inside an
  intended emit-retry backoff stays healthy (threshold = max cadence + retry budget + margin, CP-C5).
- **`SetMemoryLimit(fraction, containerLimitBytes)`** sets `GOMEMLIMIT` so GC applies backpressure
  before a cgroup OOM-kill. No-op if either input is <= 0.
- **pull-mode pprof registers on a private mux**, never `DefaultServeMux`, on its own listener
  (`pull.addr`, default `:6060`, deliberately not the health port). `pprof.Index` on the
  `/debug/pprof/` prefix dispatches `/heap`, `/goroutine` and friends - they are not separate
  handlers. The mutex and block endpoints exist but return empty: the runtime rates are deliberately
  left unset.
- **Profiling disabled is a pure no-op** (no listener, no agent, no global state). A start failure
  returns a no-op stop *and* the error, and `main` fatals: never run silently un-profiled. Profiling
  runs on leader and standby, wired before the coordinator (decision-ledger #12).
- **The tracing sampler is `AlwaysSample`, deliberately.** Own-pipeline spans are low volume (one per
  loop tick), so head-sampling everything is cheap and gives complete causal traces. Tracing itself is
  opt-in and default-off in config.

## Logs do not take the OTLP path

App logs go to stdout and are scraped by the k8s-monitoring chart into Loki. Only metrics and traces
egress via OTLP to the `-meta` self endpoint. The handler is built in `internal/logging` and set as
the slog default in `cmd/genai-otel-bridge` (logfmt by default, `log.format: json` to switch).
Data-plane log *records* are a different thing entirely and do ride OTLP.
