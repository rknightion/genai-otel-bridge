---
title: Troubleshooting
description: Common failure modes in genai-otel-bridge and how to diagnose them.
---

# Troubleshooting

This page covers the most common failure modes. For the full engineering-level failure
taxonomy (F1–F47), see [`docs/DESIGN.md §6`](https://github.com/rknightion/genai-otel-bridge/blob/main/docs/DESIGN.md).

---

## Auth errors (401/403)

**Symptom:** `GenaiOtelBridgeAuthErrors` fires. `genai_otel_bridge_auth_errors_total{source="portkey"}` or
`{source="langsmith"}` is non-zero.

**Cause:** the upstream source API returned a 401 (unauthenticated) or 403 (forbidden).
Common reasons:

- API key expired or rotated without updating the secret.
- The secret environment variable or Kubernetes Secret is not mounted correctly.
- The API key lacks the required permissions for the configured graphs or workspace.
- The API key is workspace-scoped but `settings.expected_workspace` references a different
  workspace slug (Portkey).

**How to diagnose:**

1. Check pod logs: `kubectl logs -l app=genai-otel-bridge | grep -i auth`
2. Confirm the secret is present: `kubectl get secret <name>` and verify the key name matches
   the `${ENV}` reference in your config.
3. For Portkey: try the API key against the Portkey dashboard or API to confirm it is valid.
4. For LangSmith: verify the key is a valid API key for the target instance.

---

## Stale watermark and window lag

**Symptom:** `GenaiOtelBridgePollerStale` fires. `genai_otel_bridge_window_lag_seconds` is rising
for one or more loops. `genai_otel_bridge_last_success_timestamp_seconds` is not advancing.

**Cause:** the loop is not successfully collecting and emitting. Possible causes:

- **OTLP emit failing:** the OTLP endpoint is unreachable or returning errors. Check
  `genai_otel_bridge_emit_errors_total` by `kind`. The `retryable_exhausted` reason means the
  retry budget was exhausted.
- **Source API slow or unavailable:** `genai_otel_bridge_upstream_request_duration_seconds` shows
  elevated latency or errors. A sustained outage fills the queue and blocks new collection.
- **Checkpoint save failing:** `genai_otel_bridge_emit_errors_total{kind="checkpoint_*"}` is
  non-zero. The watermark was emitted but not saved — the loop appears stuck even though data
  is flowing. Check ConfigMap RBAC.
- **Queue full:** `genai_otel_bridge_queue_depth_ratio > 0` sustained — see
  [Queue backpressure](#queue-backpressure).

**For the log-export loops** (`logs_export`, `runs`): these loops legitimately take longer
than snapshot loops because they process windowed data page by page. The
`GenaiOtelBridgePollerStale` rule is self-relative and will not fire on a healthy slow loop.

---

## Queue backpressure

**Symptom:** `GenaiOtelBridgeQueueBackpressure` fires. `genai_otel_bridge_queue_depth_ratio`
is non-zero for more than 15 minutes.

**Cause:** the emit pipeline cannot drain the per-loop queue as fast as collection fills it.
This is almost always caused by a slow or failing OTLP endpoint.

On recovery from a sustained outage, the queue clears as the backlog is emitted. The loop
resumes from its last checkpointed watermark — bounded by `max_backfill`. Samples older than
the Mimir `out_of_order_time_window` are skipped with a counted gap
(`samples_skipped_total{reason="backfill_unstorable"}`).

---

## Source-graph 404s

**Symptom:** `genai_otel_bridge_source_graph_unavailable_total` is non-zero for a specific
`graph` label.

**Cause:** a configured graph endpoint returned 404. For Portkey this typically means the
graph is not available for the API key's plan, the workspace has no data for that graph, or
the endpoint path changed.

A single-graph 404 is logged and skipped (the loop continues emitting other graphs). If
**all** configured graphs 404 in a single collect cycle, the loop errors loudly and does not
advance its watermark — this indicates a configuration or permission problem.

**How to diagnose:**

1. Check pod logs for `graph unavailable` messages.
2. Confirm the graph name is in the supported list: `cost`, `errors`, `latency`, `requests`,
   `tokens`, `users`.
3. Verify the API key has access to the graph on the Portkey dashboard.

---

## Leader absent / no emission

**Symptom:** `GenaiOtelBridgeLeaderAbsent` fires. `genai_otel_bridge_last_success_timestamp_seconds`
is absent for 15 minutes. No data in Mimir/Loki from the bridge.

**Possible causes:**

- **Pod not running:** the Deployment has 0 ready replicas.
- **OTLP egress blocked:** the pod can reach the source APIs but not the OTLP endpoint.
  Check NetworkPolicy egress rules — the product OTLP endpoint uses port 443 to the
  configured OTLP host.
- **Leader election failing:** the pod cannot reach the Kubernetes API to acquire the Lease.
  Check NetworkPolicy `apiServerCIDR` egress and RBAC.
- **First-run:** on a fresh deployment, the alert fires until the first successful emit
  clears it. Check pod logs for startup errors.

See [High Availability](./high-availability.md) for the leader election model.

---

## No standby replica

**Symptom:** `GenaiOtelBridgeNoStandby` fires.

This alert warns when fewer than 2 replicas are self-reporting — there is no failover
headroom. Expected on single-replica dev deployments.

In production, check for crash-looping pods or a Deployment with `replicas: 1`. Raise to 2
(or 3) and confirm the pod topology spread constraints are satisfiable on your cluster.

---

## Records dropped (window truncated)

**Symptom:** `GenaiOtelBridgeWindowTruncatedDroppingRecords` fires.

A windowed log loop (`runs` or `logs_export`) hit its `max_pages_per_window` limit before
draining all records in a window. Records beyond the page cap were dropped with a counted
gap. The dropped count is not known precisely (the loop stops at the cap).

**Fix:** raise `settings.max_pages_per_window` for the affected loop, or reduce
`settings.window` to process smaller windows per cycle.

---

## Samples skipped (too old)

**Symptom:** `GenaiOtelBridgeDataLoss` fires with `reason="too_old"`.

Samples from long-outage backfill are older than Mimir's `out_of_order_time_window` (default
2 hours on Grafana Cloud). The bridge cannot emit them and skips them with a counted gap.

**Fix:** request Grafana Support to raise `out_of_order_time_window` on your Mimir tenant
to match the intended max downtime SLA (GS2). Until then, data from outages longer than the
current window cannot be recovered.

---

## Buckets revised after settle

**Symptom:** `GenaiOtelBridgeBucketRevisedAfterSettle` fires. `genai_otel_bridge_bucket_revised_after_settle_total`
is growing.

Portkey analytics buckets are still changing value after the configured `bucket_settle`
window. Because Mimir cannot overwrite an already-emitted `(series, timestamp, value)`, the
revisions cannot be corrected — they are counted but not re-emitted.

**Fix:** check `genai_otel_bridge_bucket_revised_after_settle_age_seconds` histogram for the p95
of observed revision ages. Increase `bucket_settle` on the `analytics` loop to cover at least
the p95 age. This delays the reporting horizon slightly but eliminates under-counting.

---

## Content governance rejection at startup

**Symptom:** the bridge exits at startup with an error about a field being hard-denied.

A `settings.extra_record_fields` or `settings.extra_indexed_fields` entry on a loop contains
a field from the hard-denied content floor: `gen_ai.*`, `input.value`, `output.value`,
`request`, `response`, `inputs`, `outputs`, `messages`, `metadata`, or `portkeyHeaders`.

**Fix:** remove the field from the opt-in list. These fields cannot be emitted. For
LangSmith, also check that `inputs_s3_urls` and `outputs_s3_urls` are not in the list.

See [Content Governance](./governance.md) for the full model.

---

## Checkpoint corruption

**Symptom:** the bridge refuses to start, logging a corrupt checkpoint error.

**For the ConfigMap backend:** the bridge refuses to overwrite a corrupt watermark (it never
clobbers a real-but-unreadable value). Delete the affected data key from the
`genai-otel-bridge-checkpoints` ConfigMap to force a bootstrap from `now − bootstrap_lookback`.
Accept the data gap this creates.

**For the file backend:** start with `ignoreInvalid: true` to log and bootstrap over the
corrupt file. Do not use the file backend in production HA mode.

---

## Self-obs metrics absent

**Symptom:** `genai_otel_bridge_*` metrics are absent from the self-obs stack.

Confirm `emit.self` (optional — when unset it falls back to `emit.telemetry`) points to the
correct OTLP endpoint. The self-obs resource identity uses a distinct `service.namespace` from
the product telemetry, so they can be routed to different stacks. If only the self-obs endpoint
is missing data, check its OTLP endpoint, port, and credentials separately from the product
endpoint.

---

## See also

- [Alerts & Runbooks](./alerts.md) — alert rule runbooks
- [High Availability](./high-availability.md) — leader election and failover
- [Content Governance](./governance.md) — field governance model
- [Configuration](./configuration.md) — config reference
- [`docs/DESIGN.md §6`](https://github.com/rknightion/genai-otel-bridge/blob/main/docs/DESIGN.md) — full F1–F47 failure taxonomy
