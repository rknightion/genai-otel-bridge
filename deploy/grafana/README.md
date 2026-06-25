# decant Grafana resources (gcx resources-as-code)

Grafana-managed alert/recording rules (and dashboards, when added) live here as **gcx-native
resource manifests** — the same shape `gcx resources pull` produces — so they can be applied
directly with `gcx resources push`. (This replaces the old Prometheus-rule-group
`dashboards/recording-rules.yaml`.)

## Layout

Manifests are split by **role**, matching the telemetry split (`emit.telemetry` vs `emit.self` —
see `test/eks/README.md`), NOT by stack name. The manifests are **stack-agnostic** — they carry no
`namespace`/stack id; the destination stack is chosen entirely by `--context` at push time.

| Dir | Role | Holds |
|-----|------|-------|
| `self-obs/` | the integrator's own o11y (`decant_*`) | folder, the dropped-records **alert**, self recording rules |
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

`self-obs/dashboard-self-obs.yaml` — **decant — self-observability** (`dashboard.grafana.app/v2`
`Dashboard`, folder `decant`). A **tabbed, dynamic** dashboard (`TabsLayout` + responsive
`AutoGridLayout`) covering the integrator across all three of its own signals:

| Tab | Covers |
|-----|--------|
| Overview / SLO | at-a-glance badges: loops-healthy (self-relative), leader present, replicas, worst freshness ratio, max window lag, fatal emit errors; freshness-by-loop + throughput |
| Liveness & leadership | window lag, last-success age vs each loop's own baseline, replicas over time, per-loop freshness gauge (repeats per `$loop`) |
| Emit pipeline | emitted samples/logs, emit errors by kind, queue depth, samples skipped/capped, guard dropped, buckets revised after settle |
| Upstream source health | request rate/latency/error-ratio per target, auth errors, source-graph-unavailable |
| Cardinality & governance | new-label-value growth, guard drops, DPM capping |
| Logs | the poller's OWN stdout (`{namespace="grafana-poller", service_name="decant"}`) — log rate by level + a warn/error stream. NOT the high-volume republished product logs (`service_namespace=decant`) |
| Profiling | the poller's OWN Pyroscope profiles (`service_name=decant`): CPU, heap in-use, goroutines, CPU flame graph |

Three datasource variables — `${datasource}` (Prometheus, `grafanacloud-prom`), `${loki}`
(`grafanacloud-logs`), `${pyroscope}` (`grafanacloud-profiles`) — plus a multi `$loop`. Stack-agnostic;
pick the datasources per stack. Push: `gcx resources push -p deploy/grafana/self-obs --context
<self-obs-stack>` (push the `Folder` first, or the dashboard 404s on the missing folder).

**Self-relative staleness (the decoupled threshold model).** Loops have wildly different healthy
staleness — a snapshot loop (`sessions`) settles in ~2m, a windowed log-export loop (`runs`) sawtooths
to tens of minutes — so a flat threshold can't serve them. The freshness panels colour on
`decant:freshness_ratio` = each loop's current staleness ÷ its OWN trailing-6h p90 baseline: `<1.5`
green, `1.5–2` yellow, `>2` red, uniform across every loop with **no hardcoded per-loop or
per-deployment constants**. The freshness/upstream-ratio panels need the recording rules pushed
(deploy the whole dir). The basic age/lag/liveness panels are computed inline and need no rules.

Note: `emitted`/`emitted_logs`/`last_success_timestamp`/`window_lag` are recorded only on a
**successful** emit (the watermark must leave zero), so those panels read "No data" until a loop has
committed at least once — empty there is a real signal that emit is failing, not a broken panel.

The manifest is generated from `self-obs/gen_dashboard.py` (a tracked Python generator) — edit the
generator, then `make gen-dashboard` and commit the emitted YAML. gcx 0.3.x requires the **v2** schema
(it rejects `v1beta1`).

## Self-obs alerts

The `self-obs/` dir ships **eleven** alerts (push the whole dir with `gcx resources push -p deploy/grafana/self-obs --context <self-obs-stack>`). Each queries `decant_*` metrics directly (self-contained — no dependency on the recording rules being deployed), and uses `noDataState: Ok` so the healthy case (query returns nothing) never fires:

| Alert | Severity | Fires when |
|-------|----------|------------|
| `DecantLeaderAbsent` | critical | `absent_over_time(decant_last_success_timestamp_seconds[15m])` — no replica has emitted in 15m (poller down / never reached a first commit). |
| `DecantPollerStale` | warning | a loop's last-success age exceeds **2× its own trailing-6h p90 baseline AND 300s** — staler than normal *for that loop*. Self-relative, so it does not false-positive on the slow log-export loops (see below). |
| `DecantEmitFailing` | critical | fatal emit errors (`retryable_exhausted`/`checkpoint_*`/`bad_encoding`; the benign `collect` upstream-fetch retries are excluded) in 10m. |
| `DecantAuthErrors` | critical | `increase(decant_auth_errors_total[10m]) > 0` — upstream returned 401/403 (credential failure). |
| `DecantUpstreamErrorBudget` | warning | >20% of requests to an upstream `target` are 4xx/5xx/error (incl. timeouts) over 10m. |
| `DecantWindowTruncatedDroppingRecords` | warning | a windowed log loop truncated a window — records dropped (see below). |
| `DecantDataLoss` | warning | samples skipped for a real-loss reason (`too_old`/`payload_too_large`/`duplicate_timestamp`); benign reasons excluded. |
| `DecantBucketRevisedAfterSettle` | warning | >30 settled buckets/h still changing after `bucket_settle` — late arrivals; widen the loop's `bucket_settle`. |
| `DecantQueueBackpressure` | warning | `decant_queue_depth_ratio > 0` sustained 15m — emit can't keep up with collect. |
| `DecantCardinalitySpike` | warning | sustained creation of new label-value combos on a series — a cardinality early-warning. |
| `DecantNoStandby` | warning | fewer than 2 replicas self-reporting — no failover headroom (expected on intentionally single-replica dev stacks). |

`DecantLeaderAbsent` and `DecantPollerStale` are complementary: *absent* = no series at all (gone), *stale* = series present but ageing past its own baseline (wedged).

**Why `DecantPollerStale` is self-relative, not flat.** Backtested over 24h of live data, the old flat
`>900s` rule fired on ~100 healthy points/day (logs_export 55, runs 48) — the log-export loops
legitimately sawtooth to tens of minutes. The self-relative rule (`> 2× own-6h-p90 AND > 300s`, `for:
15m`) dropped that to a handful of isolated transient points that the `for` suppresses, while keeping
full sensitivity for snapshot loops (`sessions`, normally ~2m). Tune the `2×` multiplier per appetite.

## The dropped-records alert

`DecantWindowTruncatedDroppingRecords` fires (`severity: warning`) when a windowed log loop
(`runs`, `logs_export`) truncates a window — it advanced past undrained records with a counted gap,
so records were dropped. Query: `sum by (loop) (increase(decant_source_graph_unavailable_total{graph="window_truncated"}[10m])) > 0`.
The truncated COUNT is unknowable by construction (we stop at the page cap), so it's an event signal.
Remedy: raise the loop's `settings.max_pages_per_window` or shrink `settings.window`.

## Per-bucket gauge semantics — the most important thing to know

`portkey_api_*` metrics are **per-bucket gauges**, not counters. Each point is the total for one
1-minute bucket, emitted as an OTLP Gauge.

- **Use `sum_over_time(portkey_api_requests[5m])`** to sum across a window (adds per-bucket gauge values).
- **Never use `rate()`/`increase()`** on these — they assume a monotonic counter and produce
  meaningless/negative values on gauges. `portkey:requests:sum_5m` / `portkey:error_ratio:5m` encode the
  correct patterns. (NOTE: `decant_source_graph_unavailable_total` IS a counter, so the alert's
  `increase()` is correct there.)

## "No traffic" is not the same as "poller down"

Zero `portkey_api_requests` could mean a genuine quiet period OR a downed poller. Pair traffic/error
alerts with the poller-health recording rules:

- **`decant:scrape_healthy`** — 1 when the leader's last successful collect+emit is recent.
- **`decant:scrape_present`** — 1 if `decant_last_success_timestamp_seconds` was exported in the last 15m
  (missing entirely ⇒ leader gone, distinct from stale).
- **`decant_window_lag_seconds`** — gap from now to the last emitted bucket; rising ⇒ poller falling behind.

```promql
# genuine quiet period: no requests 15m AND poller healthy
sum_over_time(portkey_api_requests[15m]) == 0 and on() decant:scrape_healthy == 1
# poller stale
decant:scrape_healthy == 0
# leader gone (use absent, not just staleness — F46)
absent_over_time(decant_last_success_timestamp_seconds[15m])
```

## Grafana-staff actions required at deploy time

Pre-requisites decant cannot configure itself (Grafana-staff stack-side config):

- **GS2 — raise Mimir `out_of_order_time_window` + `reject_old_samples_max_age`** to ≥ your tolerated
  downtime, else long-outage backfill is rejected (counted loudly as
  `samples_skipped_total{reason="backfill_unstorable"}`, but still lost).
- **GS3 — exempt the `decant_*` namespace from Adaptive Metrics aggregation**, else the staleness/error
  signals that detect poller failure get rolled up and the health rules become unreliable.
- **GS1** (Loki stream-label promotion) is required for the logs phase; **GS4** (metric resource-attr
  promotion) is N/A for v1 (the gateway's default promotion set covers the data-point identity attrs).
