# internal/selfobs — the integrator's own metrics + health + profiling

`provider.go` (OTLP MeterProvider + memory limit), `metrics.go` (self-metrics, implements
`schedule.Metrics`), `health.go` (`/readyz`, `/healthz`), `profiling.go` (opt-in self-profiling).

## Self-profiling (`profiling.go`, opt-in / default-off)

`StartProfiling(ProfilingConfig) (stop func(ctx) error, err error)` profiles the integrator's OWN
runtime (self-APM). `ProfilingConfig` is **selfobs-owned** (decoupled — selfobs does NOT import
`config`; `main.go` maps the YAML into it, exactly as for `ProviderConfig`). Two modes:
- **pull** — `servePprof` registers the stdlib pprof handlers on a **private mux** (never
  `DefaultServeMux`) + a dedicated listener (`pull.addr`, default `:6060`, NOT the health port).
  `pprof.Index` on the `/debug/pprof/` prefix dispatches `/heap`, `/goroutine`, etc. — they are not
  separate handlers. mutex/block endpoints exist but are **empty** (we don't set the runtime rates).
- **push** — `pyroscope.Start(buildPyroscopeConfig(cfg))` → Grafana Cloud Profiles. `buildPyroscopeConfig`
  is a **pure seam** (unit-tested without a network); H4 identity travels in `Tags`
  (`service_namespace` = `-meta`, `deployment_environment`, `service_instance_id` = POD_NAME).

Disabled ⇒ pure no-op (no listener, no agent, no global state). Start failure returns a no-op stop
**and** the error → `main` fatals (operationally honest: never run silently un-profiled). Runs on
**leader and standby** (wired before the coordinator). Decision-ledger #12. First non-OTel push dep
(`github.com/grafana/pyroscope-go`). Tests: `servePprof` over an ephemeral listener; disabled no-op;
`buildPyroscopeConfig` field mapping.

## Distinct self-identity (H4)

Self-telemetry uses a **separate `ServiceNamespace`** (e.g. product `decant` → self `decant-meta`) so
it never mixes with republished product series. `service.instance.id = POD_NAME` (CP-H8) to diagnose
leader overlap. Meter is `decant/selfobs`; metrics use the `decant_*` prefix.

## 1DPM clamp (self plane)

`NewProvider` clamps the OTel-Go PeriodicReader interval to `60s/max_dpm` (enforced, not assumed). The SDK emits exactly 1 point per interval, so the clamp is the entire self-plane DPM cap — no coalesce stage needed.

## Gotchas

- **Endpoint path consistency (CP-H9):** config `endpoint` is the base URL; `/v1/metrics` is appended
  inside `NewProvider` — identical to the product emitter, so they can't diverge.
- **Schemaless resource (AR-C5):** uses `resource.NewSchemaless()` with raw OTLP key strings, *not*
  `resource.Merge(resource.Default(), …)` — the latter returns `ErrSchemaURLConflict` (fatal) on schema
  mismatch and couples us to semconv version churn.
- **Health liveness is leadership-aware:** `/readyz` is 503 until `MarkReady()`. `/healthz` is 200 for a
  standby (never judged on heartbeat — it isn't running the scheduler) and for a leader with a fresh
  beat; a leader past the stale threshold → 503. `Beat()` records an **attempt**, so a leader inside an
  intended emit-retry backoff stays healthy (threshold = max cadence + retry budget + margin, CP-C5).
- **`SetMemoryLimit(fraction, containerLimitBytes)`** sets `GOMEMLIMIT` (`debug.SetMemoryLimit`) so GC
  applies backpressure before a cgroup OOM-kill. No-op if either input ≤ 0.

Tests: `var _ schedule.Metrics = (*Metrics)(nil)` seam check + manual reader collection; health state
machine is table-driven with an injectable clock.

## Self-o11y review items (external review, 2026-06)

**Logs go to STDOUT, scraped by the k8s-monitoring helm chart → Loki — NOT pushed via OTLP.** Only
metrics take the OTLP path to the `-meta` self endpoint. The log handler is built in `internal/logging`
and set as the slog default in `cmd/decant` (logfmt by default; `log.format: json` to switch).

DONE this pass:
- **Upstream-API request histogram — BUILT.** `decant_upstream_request_duration_seconds{target,method,
  status_class}` (Float64Histogram on the selfobs meter, second-shaped buckets). `_count` per
  status_class gives request totals + error ratio, so no separate counter. Instrumented at the
  **`httpx` chokepoint** via `httpx.Observer` → `Metrics.ObserveUpstreamRequest`, wired in `main.go`
  (httpx and selfobs stay decoupled — neither imports the other). Covers all sources; the emit POST
  uses a plain `http.Client`, not httpx, so it's NOT yet covered (follow-up if wanted). No FROZEN-model
  impact — selfobs uses the real OTel SDK, not the hand-rolled encoder.
- **App-log format → logfmt** (`internal/logging`, config `log.format`, default logfmt). See
  `docs/superpowers/specs/logfmt-spike.md` Path A. Data-plane logs stay OTLP structured attrs (Path B).

Still open:
- **Metamonitoring mixin (self-o11y only).** Promote `deploy/dashboards/recording-rules.yaml` + the
  README guidance into a jsonnet mixin (dashboards + alerts + rules), parameterizing the placeholder
  thresholds (CP-H12, `cadence + bucket_settle + failover_margin`). Self-health panels/alerts select
  the `-meta` identity; data-plane panels the product identity. Include an alert on guard-drop /
  cardinality-overflow being present (today: `rate(decant_guard_dropped_total[…]) > 0`).
- **Tracing, opt-in / default-off (self-APM only).** Spans over the integrator's OWN loop
  (tick→collect→sanitize→encode→emit) to debug *our* latency — NOT the Portkey/LangSmith data.
  Default sampler = never; config/env flag flips it on. Caveats: (a) amends ARCHITECTURE decision #4
  (metrics+logs only) → needs a decision-ledger entry; (b) new Tempo egress channel — apply the same
  no-content discipline as the guard (trivial here, the self-obs path never holds customer content).
  Lower priority than the histogram (which answers most latency questions cheaper).
