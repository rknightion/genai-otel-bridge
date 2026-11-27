---
title: Dashboards
description: Bundled Grafana dashboards and recording rules for genai-otel-bridge self-observability and product telemetry.
---

# Dashboards

genai-otel-bridge ships Grafana resources as gcx-native resource manifests under `deploy/grafana/`.
Resources are split by **role** — self-observability (the bridge's own health) and product
telemetry (Portkey, LangSmith signals). Push each role to the appropriate stack.

---

## Layout

```text
deploy/grafana/
├── self-obs/     # genai_otel_bridge_* signals — push to your self-obs stack
│   ├── folder.yaml
│   ├── dashboard-self-obs.yaml
│   ├── alertrule-*.yaml          (20 alert rules)
│   └── recordingrule-*.yaml      (7 recording rules)
└── product/      # portkey_api_* and langsmith_* signals — push to your product stack
    ├── folder.yaml
    └── recordingrule-*.yaml      (5 recording rules)
```

---

## Applying resources

Resources are applied with the `gcx` CLI. The `--context` flag selects the target Grafana
stack. Substitute your own stack context names for the placeholders below:

```bash
# Self-obs rules, alerts, and dashboard → your self-obs stack
gcx resources push -p deploy/grafana/self-obs --context self-obs-stack

# Product recording rules → your product stack
gcx resources push -p deploy/grafana/product --context product-stack
```

Add `--dry-run` to preview changes without applying them.

!!! warning "Push the Folder before the Dashboard"
    The `folder.yaml` manifest creates the `genai-otel-bridge` folder. Push the whole
    `self-obs/` directory (not individual files) so the Folder is created before the
    Dashboard — otherwise the Dashboard creation fails with a 404 on the missing folder.

To reconcile drift after out-of-band changes:

```bash
gcx resources pull <selector> -p deploy/grafana/self-obs -o yaml
```

---

## Self-observability dashboard

`self-obs/dashboard-self-obs.yaml` — **genai-otel-bridge — self-observability**

A tabbed, dynamic dashboard (v2 `TabsLayout` + responsive `AutoGridLayout`) covering the
bridge's own health across all signals. The manifest is generated from `gen_dashboard.py`;
edit the generator and run `just gen-dashboard` to regenerate it.

### Dashboard tabs

| Tab | What it shows |
|-----|---------------|
| Overview / SLO | At-a-glance badges: loops-healthy, leader present, replicas, worst freshness ratio, max window lag, fatal emit errors; freshness-by-loop + throughput |
| Liveness & leadership | Window lag, last-success age vs each loop's own baseline, replicas over time, per-loop freshness gauge (repeats per `$loop`), scope state by pipeline (`genai_otel_bridge_scope_state`, tenant shape) |
| Emit pipeline | Emitted samples/logs, emit errors by kind, queue depth, samples skipped/capped, guard dropped, buckets revised after settle |
| Upstream source health | Request rate/latency/error-ratio per target, auth errors, capability outcomes, and data incompleteness |
| Cardinality & governance | New-label-value growth, guard drops, DPM capping |
| Logs | The poller's own stdout logs (not the high-volume product logs) |
| Profiling | The poller's own Pyroscope profiles: CPU, heap in-use, goroutines, CPU flame graph |

### Self-relative freshness

The freshness panels colour on a self-relative staleness ratio (`genai_otel_bridge:freshness_ratio`)
rather than a flat threshold. Each loop's current staleness is divided by its own trailing-6h
p90 baseline: `< 1.5` green, `1.5–2` yellow, `> 2` red. This prevents false positives on the
log-export loops, which legitimately sawtooth to tens of minutes, while keeping full sensitivity
for fast snapshot loops. The freshness and upstream-ratio panels depend on the recording rules
being deployed.

### "No data" on some panels

`emitted`, `emitted_logs`, `last_success_timestamp`, and `window_lag` are only recorded on
a **successful** emit (the watermark must leave zero). Panels for these metrics show "No data"
until the loop has committed at least once — this is a real signal that emit is failing, not
a broken panel.

### Dashboard variables

| Variable | Datasource type | Default UID |
|----------|----------------|-------------|
| `${datasource}` | Prometheus | `grafanacloud-prom` |
| `${loki}` | Loki | `grafanacloud-logs` |
| `${pyroscope}` | Pyroscope | `grafanacloud-profiles` |
| `${tenant}` | all | multi-value tenant filter; its all-value matches a series with no `tenant` label, so a legacy install shows everything |
| `${pipeline}` | all | multi-value pipeline filter (`<tenant>.<target>` in the tenant shape), with the same all-value |
| `${loop}` | — | multi-value loop filter |
| `${namespace}` | Loki (query) | multi-value namespace filter, used by the Logs panels |

The dashboard is stack-agnostic. Select the correct datasource UIDs for your stack when
provisioning.

---

## Self-obs recording rules

| Rule | What it computes |
|------|-----------------|
| `genai_otel_bridge:last_success_age:seconds` | `time() − max by (tenant, pipeline, source, loop)(last_success_timestamp_seconds)` — staleness in seconds per loop instance |
| `genai_otel_bridge:last_success_age:baseline6h` | Trailing 6h p90 of `last_success_age:seconds` per loop — the self-relative staleness baseline |
| `genai_otel_bridge:freshness_ratio` | `last_success_age:seconds / clamp_min(last_success_age:baseline6h, 120)` — the ratio the dashboard colours on; the 120s floor stops a fast, quiet loop dividing by about 0 |
| `genai_otel_bridge:upstream_error_ratio:5m` | Error ratio per upstream `target`, for the upstream-health panel. The `GenaiOtelBridgeUpstreamErrorBudget` alert inlines its own query and does not read this rule |
| `genai_otel_bridge:window_truncated:rate5m` | Rate of window-truncation events per loop, for dashboards. The `GenaiOtelBridgeWindowTruncatedDroppingRecords` alert inlines its own query and does not read this rule |
| `genai_otel_bridge:scrape_healthy` | 1 when the leader's last successful collect+emit is recent |
| `genai_otel_bridge:scrape_present` | 1 if `last_success_timestamp_seconds` was exported in the last 15m |

---

## Product recording rules

The `product/` directory ships recording rules over `portkey_api_*` and `langsmith_*` signals.
The Portkey rules encode the correct query patterns for per-bucket gauges (using `sum_over_time` not
`rate`/`increase`). The LangSmith gauges are different: each poll emits a total over the trailing
`stats_window`, so the LangSmith rules take the latest value (`last_over_time`) and never sum over time.

| Rule | What it computes |
|------|-----------------|
| `portkey:requests:sum_5m` | `sum_over_time(portkey_api_requests[5m])` — total requests over 5m windows |
| `portkey:error_ratio:5m` | Error ratio derived from `portkey_api_errors` and `portkey_api_requests` |
| `langsmith:runs:window_total` | `last_over_time(langsmith_runs[3m])` summed across sessions per environment: the run count over the trailing `settings.stats_window` (default 1h) |
| `langsmith:cost_usd:window_total` | The same for cost: spend over the trailing `stats_window` |
| `langsmith:tokens:window_total` | The same for tokens over the trailing `stats_window` |
| `langsmith:runs:sum_5m`, `langsmith:cost_usd:sum_5m`, `langsmith:tokens:sum_5m` | **Deprecated**, removed after one release. Mis-named: they apply `sum_over_time(...[5m])` to a trailing-window total, so they overstate the value (about 5x at a 1m cadence) and are not 5 minute sums. Use the `window_total` rules |

!!! note "Per-bucket gauge semantics"
    `langsmith_runs`, `langsmith_tokens` and `langsmith_cost_usd` are not per-bucket: each point is the
    total over the trailing `stats_window` (aggregate-now). Read them with `last_over_time`; never
    `sum_over_time`, which adds overlapping windows.

    `portkey_api_*` metrics are **per-bucket gauges**, not counters. The loops emit every
    cadence (60s by default), but the newest sample is about 11 minutes old because the query upper
    bound sits `bucket_settle` (10m) behind now. An instant query can therefore read as absent: use
    `last_over_time(...[20m])` to see the last known value.
    Always use `sum_over_time` to aggregate; never use `rate()` or `increase()` on these metrics.
    `genai_otel_bridge_source_capability_total`, `genai_otel_bridge_source_data_incomplete_total`, and
    `genai_otel_bridge_upstream_request_duration_seconds` are counters — `rate()` is correct there.

---

## Grafana-staff prerequisites

Before deploying to production:

- **GS2** — raise Mimir `out_of_order_time_window` and `reject_old_samples_max_age` to match
  your tolerated downtime SLA, or long-outage backfill will be rejected.
- **GS3** — exempt `genai_otel_bridge_*` from Adaptive Metrics aggregation, or the staleness
  and error signals that detect poller failure will be rolled up and the health rules will
  become unreliable.

---

## See also

- [Alerts & Runbooks](./alerts.md) — the twenty bundled alert rules with runbooks
- [Telemetry reference](./telemetry.md) — full signal catalogue
- [Troubleshooting](./troubleshooting.md) — common failure modes
