---
title: Alerts & Runbooks
description: Bundled self-observability alert rules for genai-otel-bridge, with runbooks for each.
---

# Alerts & Runbooks

genai-otel-bridge ships eleven self-observability alert rules under
`deploy/grafana/self-obs/alertrule-*.yaml`. All rules query `genai_otel_bridge_*` metrics
directly (no recording-rule dependency) and use `noDataState: Ok` so the healthy case (no
series at all) never fires spuriously.

Push the entire `self-obs/` directory to apply them:

```bash
gcx resources push -p deploy/grafana/self-obs --context self-obs-stack
```

---

## Alert summary

| Alert | Severity | When it fires |
|-------|----------|---------------|
| [GenaiOtelBridgeLeaderAbsent](#genaiotelbridgeleaderabsent) | critical | No successful emit from any replica in 15m |
| [GenaiOtelBridgePollerStale](#genaiotelbridgepollerstale) | warning | A loop is staler than 2× its own 6h baseline AND > 300s |
| [GenaiOtelBridgeEmitFailing](#genaiotelbridgeemitfailing) | critical | Fatal emit errors in the last 10m |
| [GenaiOtelBridgeAuthErrors](#genaiotelbridgeautherrors) | critical | Upstream returned 401/403 (credential failure) |
| [GenaiOtelBridgeUpstreamErrorBudget](#genaiotelbridgeupstreamerrorbudget) | warning | > 20% of requests to an upstream target are errors |
| [GenaiOtelBridgeWindowTruncatedDroppingRecords](#genaiotelbridgewindowtruncateddroppingrecords) | warning | A log loop truncated a window; records were dropped |
| [GenaiOtelBridgeDataLoss](#genaiotelbridgedataloss) | warning | Samples skipped for a real-loss reason (too-old / duplicate) |
| [GenaiOtelBridgeBucketRevisedAfterSettle](#genaiotelbridgebucketrevisedaftersettle) | warning | > 30 settled buckets/h still changing after `bucket_settle` |
| [GenaiOtelBridgeQueueBackpressure](#genaiotelbridgequeuebackpressure) | warning | Emit cannot keep up with collect (queue depth > 0 for 15m) |
| [GenaiOtelBridgeCardinalitySpike](#genaiotelbridgecardinalityspike) | warning | Sustained creation of new label-value combinations on a series |
| [GenaiOtelBridgeNoStandby](#genaiotelbridgenostandby) | warning | Fewer than 2 replicas self-reporting |

`GenaiOtelBridgeLeaderAbsent` and `GenaiOtelBridgePollerStale` are complementary:
*absent* means no series at all (leader gone); *stale* means the series is present but
ageing past its own normal baseline (leader wedged or loop stuck).

---

## Runbooks

### GenaiOtelBridgeLeaderAbsent

**Severity:** critical

**Fires when:** `absent_over_time(genai_otel_bridge_last_success_timestamp_seconds[15m])` is true
for 10 minutes — no replica has successfully emitted in 15 minutes.

**What to check:**

1. Check that the Deployment is running: `kubectl get pods -l app=genai-otel-bridge`
2. Check pod logs for startup errors: `kubectl logs -l app=genai-otel-bridge`
3. Check that the Kubernetes Lease exists: `kubectl get lease genai-otel-bridge-leader`
4. Check OTLP egress — the pod must reach the configured OTLP endpoint on port 443. If
   `genai_otel_bridge_emit_errors_total` is non-zero, the source APIs may be reachable but
   the OTLP endpoint is blocked.
5. Check RBAC — the pod's ServiceAccount must be able to `get`/`update` the Lease and
   the `genai-otel-bridge-checkpoints` ConfigMap.

**Note:** this alert fires as soon as the metric goes absent; it does not require the loop to have
been running previously. On a fresh deployment it will fire until the first successful emit.

See also: [Troubleshooting](./troubleshooting.md), [High Availability](./high-availability.md).

---

### GenaiOtelBridgePollerStale

**Severity:** warning

**Fires when:** a loop's last-success age exceeds **2× its own trailing-6h p90 baseline AND
> 300 seconds** for 15 minutes.

This rule is **self-relative** — each loop's threshold is derived from its own recent behaviour.
Log-export loops (`logs_export`, `runs`) legitimately take tens of minutes per cycle; the
self-relative threshold avoids false positives on those loops while remaining sensitive to
a genuinely stuck snapshot loop (`sessions`).

**What to check:**

1. Which loop is stale? Check `genai_otel_bridge_window_lag_seconds` labelled by `loop`.
2. Check `genai_otel_bridge_emit_errors_total` for that loop — failed emits prevent watermark advancement.
3. Check `genai_otel_bridge_upstream_request_duration_seconds` — slow source API responses increase
   collect time, which can cause the window lag to grow.
4. Check `genai_otel_bridge_queue_depth` — a full queue blocks collection.

See also: [Troubleshooting — stale watermark](./troubleshooting.md#stale-watermark-and-window-lag).

---

### GenaiOtelBridgeEmitFailing

**Severity:** critical

**Fires when:** fatal emit errors (`retryable_exhausted`, `checkpoint_*`, `bad_encoding`) appear
in the last 10 minutes. Benign upstream-collect retries are excluded.

**What to check:**

1. Check pod logs for OTLP error messages.
2. Check that the OTLP endpoint is reachable and accepting data.
3. If `bad_encoding` errors appear, this indicates a bug in the OTLP encoder — file an issue.
4. If `checkpoint_*` errors appear, check ConfigMap RBAC and whether the ConfigMap is corrupt.

See also: [Troubleshooting](./troubleshooting.md).

---

### GenaiOtelBridgeAuthErrors

**Severity:** critical

**Fires when:** `increase(genai_otel_bridge_auth_errors_total[10m]) > 0` — the upstream source API
returned a 401 or 403 response.

**What to check:**

1. Check which `source` label is on the metric — it identifies which source's credential is failing.
2. Verify the API key / secret for that source is correct and not expired.
3. Check that the Kubernetes Secret or environment variable containing the credential is mounted
   correctly in the pod.
4. If the error started recently after a deployment, check for a config change that may have
   altered the credential reference.

See also: [Troubleshooting — auth errors](./troubleshooting.md#auth-errors-401403).

---

### GenaiOtelBridgeUpstreamErrorBudget

**Severity:** warning

**Fires when:** more than 20% of requests to an upstream `target` are 4xx/5xx or errors
(including timeouts) over 10 minutes.

**What to check:**

1. Check `genai_otel_bridge_upstream_request_duration_seconds` labelled by `target` and
   `status_class` for the error distribution.
2. Distinguish between 401/403 (credential failure → `GenaiOtelBridgeAuthErrors` fires too),
   429 (quota exceeded — the loop backs off automatically), and 5xx (upstream outage).
3. Check the upstream platform status page for incidents.
4. If the errors are sustained, check that the configured `base_url` is correct.

---

### GenaiOtelBridgeWindowTruncatedDroppingRecords

**Severity:** warning

**Fires when:** a windowed log loop (`runs`, `logs_export`) truncated a window — it advanced
past undrained records with a counted gap. Some log records were dropped.

**Query:** `sum by (loop) (increase(genai_otel_bridge_source_graph_unavailable_total{graph="window_truncated"}[10m])) > 0`

The truncated count is unknowable by construction (the loop stops at the page cap).

**What to check:**

1. Which loop truncated? The `loop` label identifies it.
2. If `logs_export` is truncating: increase `settings.max_pages_per_window` or decrease
   `settings.window` to reduce the volume per window.
3. If `runs` is truncating: increase `settings.max_pages_per_window` or narrow the scope
   via `settings.session_filter`.

---

### GenaiOtelBridgeDataLoss

**Severity:** warning

**Fires when:** samples are being skipped for a real-loss reason: `too_old` (sample outside
Mimir's accept window), `payload_too_large` (413 on a minimal chunk), or
`duplicate_timestamp`. Benign reasons are excluded.

**What to check:**

1. **`too_old`:** the sample is outside Mimir's `out_of_order_time_window`. Either the
   loop's `max_backfill` exceeds the Mimir window, or the stack's OOO window is too small
   for the intended max downtime. Request Grafana Support to raise it (GS2).
2. **`payload_too_large`:** the minimum emit chunk exceeds the gateway's payload limit.
   Reduce `max_records_per_chunk` on the relevant loop.
3. **`duplicate_timestamp`:** two sources are writing to the same `(series, timestamp)`.
   Check for overlapping series names across sources — this is caught at startup but can
   appear after config changes.

---

### GenaiOtelBridgeBucketRevisedAfterSettle

**Severity:** warning

**Fires when:** more than 30 settled buckets per hour are still changing value after
`bucket_settle` — the late-arrival lag for the analytics loop exceeds the current setting.

**What to check:**

1. Check `genai_otel_bridge_bucket_revised_after_settle_age_seconds` histogram for the p95
   of observed revision ages. Set `bucket_settle` to at least the p95 value.
2. Widening `bucket_settle` delays the reporting horizon slightly but eliminates
   under-counting. Metrics cannot be re-emitted once settled.

See also: [Portkey — bucket settle](./portkey.md#bucket-settle-and-watermark).

---

### GenaiOtelBridgeQueueBackpressure

**Severity:** warning

**Fires when:** `genai_otel_bridge_queue_depth_ratio > 0` is sustained for 15 minutes — the
emit pipeline cannot drain the queue as fast as collection enqueues batches.

**What to check:**

1. Check `genai_otel_bridge_emit_errors_total` — repeated emit errors (OTLP 5xx / timeout)
   fill the queue while workers are busy retrying.
2. Check the OTLP endpoint latency — slow responses reduce emit throughput.
3. Under sustained outage, `window_lag` rises and `GenaiOtelBridgePollerStale` will eventually
   fire. On recovery, the loop resumes from its watermark — bounded by `max_backfill`.

---

### GenaiOtelBridgeCardinalitySpike

**Severity:** warning

**Fires when:** new label-value combinations are being created at a sustained rate on a
series — a cardinality early warning.

**What to check:**

1. Check `genai_otel_bridge_new_label_values_total` labelled by `series` to identify which
   metric is growing.
2. Check whether a high-cardinality field was recently added to `governance.allow_label_keys`
   or `settings.extra_indexed_fields`.
3. For the LangSmith `session` label: check that `session_filter` is set to bound the number
   of sessions in scope.
4. Review `governance.per_metric_cardinality_budget` — the guard drops over-budget series
   (counted via `genai_otel_bridge_guard_dropped_total`).

See also: [Content Governance](./governance.md#cardinality-governance).

---

### GenaiOtelBridgeNoStandby

**Severity:** warning

**Fires when:** fewer than 2 replicas are self-reporting (i.e. only 1 replica visible, or none).

This alert is expected to fire on intentionally single-replica dev stacks. In production,
fewer than 2 replicas means there is no failover headroom.

**What to check:**

1. Check the Deployment replica count: `kubectl get deploy genai-otel-bridge`
2. Check for pending or crash-looping pods: `kubectl get pods -l app=genai-otel-bridge`
3. On multi-AZ deployments, check that the pod topology spread constraints are satisfiable
   (the chart uses `ScheduleAnyway`, so this should not block scheduling, but it is worth
   verifying).

---

## See also

- [Dashboards](./dashboards.md) — the self-obs dashboard and recording rules
- [Telemetry reference](./telemetry.md) — `genai_otel_bridge_*` metric definitions
- [Troubleshooting](./troubleshooting.md) — detailed failure mode guidance
- [High Availability](./high-availability.md) — leader election and failover
