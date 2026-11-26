# genai-otel-bridge Grafana resources (gcx resources-as-code)

Grafana-managed alert rules, recording rules and a dashboard live here as **gcx-native
resource manifests** — the same shape `gcx resources pull` produces — so they can be applied
directly with `gcx resources push`. (This replaces the old Prometheus-rule-group
`dashboards/recording-rules.yaml`.)

## Layout

Manifests are split by **role**, matching the telemetry split (`emit.telemetry` vs `emit.self` —
see `test/eks/README.md`), NOT by stack name. The manifests are **stack-agnostic** — they carry no
`namespace`/stack id; the destination stack is chosen entirely by `--context` at push time.

| Dir | Role | Holds |
|-----|------|-------|
| `self-obs/` | the integrator's own o11y (`genai_otel_bridge_*`) | folder, the self-obs dashboard (`dashboard-self-obs.yaml`), the alert rules (see Self-obs alerts below) and the self recording rules |
| `product/` | product telemetry (`portkey_api_*`, `langsmith_*`) | folder, portkey + langsmith recording rules |

`gcx` auto-detects kind from each file's `apiVersion`/`kind`, so the flat per-role layout is enough.

## Apply

Pick the `--context` for the stack each role targets in your environment (the telemetry split means
self-obs and product can be different stacks). Substitute your own stack contexts — the examples below
use placeholder names `self-obs-stack` and `product-stack`:

```bash
# self-obs rules + alert → your self-obs stack
gcx resources push -p deploy/grafana/self-obs --context self-obs-stack   # add --dry-run to preview

# product recording rules → your product stack
gcx resources push -p deploy/grafana/product  --context product-stack
```

**Applied status:** the self-obs dir is **deploy-by-default** — push the whole dir and you get the
dashboard, all recording rules, and all alerts together (they are designed as one set; the dynamic-
threshold alerts/panels rely on the recording rules being present). Re-pull to reconcile drift:
`gcx resources pull <selector> -p deploy/grafana/<role> -o yaml`.

## Dashboards

`self-obs/dashboard-self-obs.yaml` — **genai-otel-bridge — self-observability** (`dashboard.grafana.app/v2`
`Dashboard`, folder `genai-otel-bridge`). A **tabbed, dynamic** dashboard (`TabsLayout` + responsive
`AutoGridLayout`) covering the integrator across all three of its own signals:

| Tab | Covers |
|-----|--------|
| Overview / SLO | at-a-glance badges: loops-healthy (self-relative), leader present, replicas, worst freshness ratio, max window lag, fatal emit errors; freshness-by-loop + throughput |
| Liveness & leadership | window lag, last-success age vs each loop's own baseline, replicas over time, per-loop freshness gauge (repeats per `$loop`) |
| Emit pipeline | emitted samples/logs, emit errors by kind, queue depth, samples skipped/capped, guard dropped, buckets revised after settle |
| Upstream source health | request rate/latency/error-ratio per target, auth errors, capability outcomes, and data incompleteness |
| Cardinality & governance | new-label-value growth, guard drops, DPM capping |
| Logs | the poller's OWN stdout (`{namespace="$namespace", service_name="genai-otel-bridge"}`, via the dashboard's namespace variable) — log rate by level + a warn/error stream. NOT the high-volume republished product logs (`service_namespace=genai-otel-bridge`) |
| Profiling | the poller's OWN Pyroscope profiles (`service_name=genai-otel-bridge`): CPU, heap in-use, goroutines, CPU flame graph |

Three datasource variables — `${datasource}` (Prometheus, `grafanacloud-prom`), `${loki}`
(`grafanacloud-logs`), `${pyroscope}` (`grafanacloud-profiles`) — plus a multi `$loop` and a multi `$namespace` (a Loki label-values query; the Logs panels use it). `$loop` offers base loop names (a fan-out `logs_export,api_key_use_case=<slug>` instance appears as `logs_export`), so one selection shows both the runner-fed series of every instance and the hook-fed counters, which carry the bare loop name. Stack-agnostic;
pick the datasources per stack. Push: `gcx resources push -p deploy/grafana/self-obs --context
<self-obs-stack>` (push the `Folder` first, or the dashboard 404s on the missing folder).

**Self-relative staleness (the decoupled threshold model).** Loops have wildly different healthy
staleness — a snapshot loop (`sessions`) settles in ~2m, a windowed log-export loop (`runs`) sawtooths
to tens of minutes — so a flat threshold can't serve them. The freshness panels colour on
`genai-otel-bridge:freshness_ratio` = each loop's current staleness ÷ its OWN trailing-6h p90 baseline: `<1.5`
green, `1.5–2` yellow, `>2` red, uniform across every loop with **no hardcoded per-loop or
per-deployment constants**. The freshness/upstream-ratio panels need the recording rules pushed
(deploy the whole dir). The basic age/lag/liveness panels are computed inline and need no rules.

Note: `emitted`/`emitted_logs`/`last_success_timestamp`/`window_lag` are recorded only on a
**successful** emit (the watermark must leave zero), so those panels read "No data" until a loop has
committed at least once — empty there is a real signal that emit is failing, not a broken panel.

The manifest is generated from `self-obs/gen_dashboard.py` (a tracked Python generator) — edit the
generator, then `just gen-dashboard` and commit the emitted YAML. gcx 0.3.x requires the **v2** schema
(it rejects `v1beta1`).

## Self-obs alerts

The `self-obs/` dir ships **twenty** alerts (push the whole dir with `gcx resources push -p deploy/grafana/self-obs --context <self-obs-stack>`). Each queries `genai_otel_bridge_*` metrics directly (self-contained — no dependency on the recording rules being deployed), and uses `noDataState: Ok` so the healthy case (query returns nothing) never fires:

| Alert | Severity | Fires when |
|-------|----------|------------|
| `GenaiOtelBridgeLeaderAbsent` | critical | `absent_over_time(genai_otel_bridge_last_success_timestamp_seconds[15m])` or no replica reporting `genai_otel_bridge_leader_ratio == 1` — no replica has emitted in 15m, or none can lead (poller down / never reached a first commit / lease unavailable). |
| `GenaiOtelBridgePollerStale` | warning | a loop's last-success age exceeds **2× its own trailing-6h p90 baseline AND 300s** — staler than normal *for that loop* — **OR exceeds the 3h absolute ceiling**. The relative arm is self-relative, so it does not false-positive on the slow log-export loops; the ceiling keeps a permanent stall firing (see below). |
| `GenaiOtelBridgeEmitFailing` | critical | fatal emit errors (`retryable_exhausted`/`checkpoint_*`/`granularity_unexpected`/`bad_encoding`/`unknown_4xx`; the benign `collect` upstream-fetch retries are excluded) in 10m. `unknown_4xx` = an unrecognised non-retryable 4xx from the OTLP gateway (e.g. a rotated/invalid emit token). |
| `GenaiOtelBridgeAuthErrors` | critical | `increase(genai_otel_bridge_auth_errors_total[10m]) > 0` — upstream returned 401/403 (credential failure). |
| `GenaiOtelBridgeUpstreamErrorBudget` | warning | >20% of requests to an upstream `target` are 4xx/5xx/error (incl. timeouts) over 10m. |
| `GenaiOtelBridgeWindowTruncatedDroppingRecords` | warning | the LangSmith `runs` loop truncated a window — records dropped (see below). |
| `GenaiOtelBridgeExportPipelineDataIncomplete` | critical | `source_data_incomplete_total{reason=~"export_failed\|export_stuck\|window_oversize"}` increased in 15m — the export pipeline cannot produce its target's data, or a `logs_export` window is parked (see below). |
| `GenaiOtelBridgeWorkspaceScopePermissionDenied` | critical | `source_capability_total{graph="workspace_scope",state="permission-denied"}` increased in 15m — configured credential and expected workspace scope disagree (see below). |
| `GenaiOtelBridgeDataLoss` | warning | samples skipped for a real-loss reason (`too_old`/`payload_too_large`/`duplicate_timestamp`); benign reasons excluded. |
| `GenaiOtelBridgeBucketRevisedAfterSettle` | warning | >30 settled buckets/h still changing after `bucket_settle` — late arrivals; widen the loop's `bucket_settle`. |
| `GenaiOtelBridgeQueueBackpressure` | warning | `genai_otel_bridge_queue_depth_ratio > 0` sustained 15m — emit can't keep up with collect. |
| `GenaiOtelBridgeCardinalitySpike` | warning | sustained creation of new label-value combos on a series — a cardinality early-warning. |
| `GenaiOtelBridgeLoopDegraded` | warning | `max by (loop, reason) (genai_otel_bridge_loop_degraded_ratio) == 1` for 15m — a loop is held in the degraded state. The gauge has OTLP unit `1`, so the gateway exposes it with the `_ratio` suffix (as for `queue_depth`). |
| `GenaiOtelBridgePartialSuccessRejected` | warning | `increase(genai_otel_bridge_emit_partial_success_rejected_total[10m]) > 0` — the gateway rejected records inside a 200 `partial_success` response (the emit looked successful; this counter is the only signal). |
| `GenaiOtelBridgeNoStandby` | warning | fewer than 2 replicas self-reporting `genai_otel_bridge_leader_ratio` — no failover headroom (expected on intentionally single-replica dev stacks). |
| `GenaiOtelBridgeMemoryNearLimit` | warning | `genai_otel_bridge_go_heap_inuse_bytes` above 85% of `genai_otel_bridge_go_memory_limit_bytes` for 15m. |
| `GenaiOtelBridgeScopeMismatched` | critical | `max by (tenant, pipeline) (genai_otel_bridge_scope_state{state="mismatched"}) == 1` for 5m — a tenant-shape pipeline's credential belongs to a different upstream scope than its target's; the pipeline is held back (pages). |
| `GenaiOtelBridgeTenantBlocked` | critical | `max by (tenant, reason) (genai_otel_bridge_tenant_blocked) == 1` for 5m — a tenant cannot run (`secret_unreadable` or `duplicate_key`); none of its pipelines start (pages). |
| `GenaiOtelBridgeScopeAssertionErrors` | warning | every scope assertion call for a pipeline ended in `result="error"` (none matched, mismatched or unverifiable) for 30m — a pipeline stuck with no result. |
| `GenaiOtelBridgeScopeUnverified` | info | `scope_state{state="unverified"}` is 1 for a pipeline for 24h (low severity). |

### Tenant and pipeline grouping

In the tenant shape every per-pipeline self-metric carries `tenant` and `pipeline` beside `source` and `loop`. So the per-pipeline rules (auth errors, bucket revised after settle, data loss, emit failing, loop degraded, poller stale, queue backpressure, window truncated, cardinality spike, partial reject) and the recording rules freshness ratio, last-success age, its 6h baseline, scrape healthy and window-truncated rate group by `tenant, pipeline` as well, so two pipelines never merge into one alert. A series with no `tenant` label (a legacy install) is grouped, and so judged, exactly as before. The window-truncated rules are per pipeline because the Portkey `groups` loop, which the tenant shape keeps, fires `window_truncated`.

`GenaiOtelBridgePollerStale` and the baseline recording rule judge a tenant-shape pipeline only while `genai_otel_bridge_scope_state{state="matched"}` is 1 for it (an `and on (tenant, pipeline)` join), because an unverified or mismatched pipeline is held back on purpose and its age grows by design. A second branch, `tenant=""`, keeps a series with no tenant label judged as today. `self-obs/sim_poller_stale.py` proves the gate's arithmetic; `TestBundledRuleGroupings` pins the join.

Left as they were, with the reason: `GenaiOtelBridgeLeaderAbsent` (no replica emitting is a process-wide fact; a new install whose tenants have never matched records no success and fires it, so say so in the onboarding), `GenaiOtelBridgeMemoryNearLimit` and `GenaiOtelBridgeNoStandby` (per replica), `GenaiOtelBridgeUpstreamErrorBudget` and its recording rule (grouped by host `target`), the scrape-present recording rule, `GenaiOtelBridgeExportPipelineDataIncomplete` (logs export, which the tenant shape refuses) and `GenaiOtelBridgeWorkspaceScopePermissionDenied` (the legacy `expected_workspace` check, which a scoped row refuses).

The four scope alerts take the max across replicas (a replica that is not leading reports `scope_state` all 0) and treat no data as healthy.

`GenaiOtelBridgeLeaderAbsent` and `GenaiOtelBridgePollerStale` are complementary: *absent* = no series at all (gone), *stale* = series present but ageing past its own baseline (wedged).

**Why `GenaiOtelBridgePollerStale` is self-relative, not flat.** Backtested over 24h of live data, the old flat
`>900s` rule fired on ~100 healthy points/day (logs_export 55, runs 48) — the log-export loops
legitimately sawtooth to tens of minutes. The self-relative rule (`> 2× own-6h-p90 AND > 300s`, `for:
15m`) dropped that to a handful of isolated transient points that the `for` suppresses, while keeping
full sensitivity for snapshot loops (`sessions`, normally ~2m). Tune the `2×` multiplier per appetite.

The relative arm alone resolves itself during a permanent stall: once the stall is longer than about 36
minutes (10% of the 6h window) the trailing p90 is itself a stalled sample, the condition collapses to
`age < 72m`, and the alert clears while the loop is still dead (a loop whose healthy age reaches 30
minutes never leaves Pending). So the rule has a second arm, `or age > 10800` (3h, same `for: 15m`),
above the slowest healthy sawtooth of any loop. It is the backstop the relative arm cannot erase; it adds
no metric. `self-obs/sim_poller_stale.py` is the minute-resolution simulation (no rule test harness
exists): it reports the base condition false from about 70 minutes on and the candidate true to the end of
a 24h stall for healthy ages of 2, 10, 30 and 50 minutes. The dashboard "Loops healthy" badge also goes red
when any selected loop's age exceeds the same ceiling.

## The dropped-records alert

`GenaiOtelBridgeWindowTruncatedDroppingRecords` fires (`severity: warning`) when the LangSmith `runs`
loop truncates a window — it advanced past undrained records with a counted gap, so records were
dropped. Only `runs` emits `window_truncated`; the Portkey `logs_export` loop parks an oversize window
instead (see the export-pipeline alert below). Query: `sum by (loop) (increase(genai_otel_bridge_source_data_incomplete_total{reason="window_truncated"}[10m])) > 0`.
The truncated COUNT is unknowable by construction (we stop at the page cap), so it's an event signal.
Remedy: raise `settings.max_pages_per_window` or shrink `settings.window` on the `runs` loop.

## Export-pipeline data incompleteness alert

`GenaiOtelBridgeExportPipelineDataIncomplete` fires (`severity: critical`) when
`genai_otel_bridge_source_data_incomplete_total` increases in 15m with `reason=export_failed`, `reason=export_stuck` or `reason=window_oversize`. The first two mean the export-job
lifecycle cannot produce the target's data; check the export-job status and retry/backoff behaviour.
`window_oversize` means a Portkey `logs_export` window needed more pages than
`settings.max_pages_per_window`: the loop fires the counter once, parks the window and stops advancing.
Nothing was dropped. Shrink `settings.window`; the block clears when the window changes.

Query: `sum by (loop, reason) (increase(genai_otel_bridge_source_data_incomplete_total{reason=~"export_failed|export_stuck|window_oversize"}[15m])) > 0`.

## Workspace-scope permission alert

`GenaiOtelBridgeWorkspaceScopePermissionDenied` fires (`severity: critical`) when
`genai_otel_bridge_source_capability_total{graph="workspace_scope",state="permission-denied"}`
increases in 15m. The loop deliberately refuses to emit data until the configured credential and
expected workspace scope agree; check a recent key rotation and the scope configuration.

Query: `sum by (loop) (increase(genai_otel_bridge_source_capability_total{graph="workspace_scope",state="permission-denied"}[15m])) > 0`.

Capability states `endpoint-absent` and `plan-unsupported` are valid contract values but are currently
producerless, so they have no series. A single 404 cannot distinguish those outcomes; interpret steady
versus intermittent observed capability increments at query time rather than assuming an alert can do so.

## Per-bucket gauge semantics — the most important thing to know

`portkey_api_*` metrics are **per-bucket gauges**, not counters. Each point is the total for one
1-minute bucket, emitted as an OTLP Gauge.

- **Use `sum_over_time(portkey_api_requests[5m])`** to sum across a window (adds per-bucket gauge values).
- **Never use `rate()`/`increase()`** on these — they assume a monotonic counter and produce
  meaningless/negative values on gauges. `portkey:requests:sum_5m` / `portkey:error_ratio:5m` encode the
  correct patterns. (NOTE: `genai_otel_bridge_source_capability_total` and
  `genai_otel_bridge_source_data_incomplete_total` are counters, so `rate()` and `increase()` are
  correct there.)

## "No traffic" is not the same as "poller down"

Zero `portkey_api_requests` could mean a genuine quiet period OR a downed poller. Pair traffic/error
alerts with the poller-health recording rules:

- **`genai-otel-bridge:scrape_healthy`** — 1 when the leader's last successful collect+emit is recent.
- **`genai-otel-bridge:scrape_present`** — 1 if `genai_otel_bridge_last_success_timestamp_seconds` was exported in the last 15m
  (missing entirely ⇒ leader gone, distinct from stale).
- **`genai_otel_bridge_window_lag_seconds`** — gap from now to the last emitted bucket; rising ⇒ poller falling behind.

```promql
# genuine quiet period: no requests 15m AND poller healthy
sum_over_time(portkey_api_requests[15m]) == 0 and on() genai-otel-bridge:scrape_healthy == 1
# poller stale
genai-otel-bridge:scrape_healthy == 0
# leader gone (use absent, not just staleness — F46)
absent_over_time(genai_otel_bridge_last_success_timestamp_seconds[15m])
```

## Grafana-staff actions required at deploy time

Pre-requisites genai-otel-bridge cannot configure itself (Grafana-staff stack-side config):

- **GS2 — raise Mimir `out_of_order_time_window` + `reject_old_samples_max_age`** to ≥ your tolerated
  downtime, else long-outage backfill is rejected (counted loudly as
  `samples_skipped_total{reason="backfill_unstorable"}`, but still lost).
- **GS3 — exempt the `genai_otel_bridge_*` namespace from Adaptive Metrics aggregation**, else the staleness/error
  signals that detect poller failure get rolled up and the health rules become unreliable.
- **GS1** (Loki stream-label promotion) is required for the logs phase; **GS4** (metric resource-attr
  promotion) is N/A for v1 (the gateway's default promotion set covers the data-point identity attrs).
