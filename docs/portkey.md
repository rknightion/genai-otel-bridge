---
title: Portkey
description: How genai-otel-bridge collects and emits telemetry from the Portkey LLM gateway.
---

# Portkey

genai-otel-bridge collects operational telemetry from [Portkey](https://portkey.ai) via three
independent loops: **`analytics`** (bucketed workspace-aggregate metrics), **`groups`** (per-dimension
snapshot metrics), and **`logs_export`** (content-free per-request log records). Each loop is
independently enabled via its own `loops.<name>` config block.

!!! info "Content-free by design"
    None of the three loops ever request or emit prompt text, completion text, or message bodies.
    The `prompt` label that the groups loop can emit is a **saved-prompt ID** — an opaque
    identifier, not text. See [Content Governance](./governance.md).

---

## Analytics loop

The analytics loop polls Portkey's graph API and emits workspace-aggregate gauges for each
configured graph. Metrics are **per-bucket gauges** — each data point represents the total for
one 1-minute bucket.

### Emitted metrics

With the default `metric_prefix: portkey_api`, the analytics loop emits:

| Metric | Unit | Description |
|--------|------|-------------|
| `portkey_api_requests` | — | Request count per 1-minute bucket |
| `portkey_api_cost_usd` | USD | Request cost per bucket |
| `portkey_api_tokens` | — | Token units per bucket (split by `token_type`: total/input/output) |
| `portkey_api_latency_seconds` | s | Latency per bucket (per `quantile`: avg/p50/p90/p99) |
| `portkey_api_errors` | — | Error count per bucket |
| `portkey_api_users` | — | Distinct user count per bucket |

Which metrics are collected is controlled by the `graphs` setting. The default set is five —
`requests`, `cost`, `tokens`, `latency`, `errors`; `users` is opt-in (add it explicitly).

!!! warning "Use `sum_over_time`, not `rate` or `increase`"
    These are gauges, not counters. To aggregate over a time range use
    `sum_over_time(portkey_api_requests[5m])`. Using `rate()` or `increase()` on gauges
    produces meaningless or negative values. The bundled recording rules encode the correct
    patterns — see [Dashboards](./dashboards.md).

### Label dimensions

The `quantile` label on latency metrics and the `token_type` label on token metrics are
**only visible if they are allow-listed** in the governance config. The composition root
allow-lists them by default. See [Content Governance](./governance.md) for the
default-deny label model.

Use-case segmentation is available via `api_key_use_cases`: when configured, each pass
stamps an `api_key_use_case` label on its metrics so different uses of the gateway are
distinguishable.

### Bucket settle and watermark

The analytics loop is time-bucketed. A bucket is only emitted once
`bucket_end ≤ now − bucket_settle` to avoid emitting incomplete (still-updating) buckets.
The `bucket_settle` setting (default 3 minutes) should be tuned to the measured late-arrival
lag for your workspace. The loop detects post-settle revisions and records
`genai_otel_bridge_bucket_revised_after_settle_total` so you can tune `bucket_settle` to
the p95 of observed revision ages rather than guessing.

### Example analytics config

```yaml
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 55m
        bucket_settle: 3m
        max_backfill: 90m
        metric_prefix: portkey_api
        graphs:            # default: requests, cost, tokens, latency, errors
          - requests
          - cost
          - tokens
          - latency
          - errors
          - users          # opt-in: distinct-user count (not in the default set)
```

---

## Groups loop

The groups loop polls Portkey's dimension-aggregate endpoints and emits per-dimension snapshot
gauges. Unlike the analytics loop it is **not time-bucketed** — each poll takes a trailing-window
snapshot of totals. Timestamps are truncated to the minute so two polls in the same wall-clock
minute share a timestamp (1 data point per series per minute regardless of cadence).

### Emitted metrics

The groups loop emits metrics whose names follow the pattern `{metric_prefix}_{metric}_by_{dimension}`:

| Metric | Dimension labels | Gated by |
|--------|-----------------|----------|
| `portkey_api_requests_by_ai_model` | `ai_model` | groups enabled |
| `portkey_api_cost_usd_by_ai_model` | `ai_model` | `settings.emit_cost: true` (default) |
| `portkey_api_requests_by_metadata_value` | `metadata_key`, `metadata_value` | `settings.metadata_keys` configured |
| `portkey_api_requests_by_prompt` | `prompt` | `settings.emit_prompts: true` (default) |

The `prompt` label value is a **saved-prompt ID** (an opaque slug like `pp-my-prompt-abc123`),
not prompt text. It is content-free.

!!! note
    Metadata dimension values become label values. The per-series cardinality budget in
    `governance.per_metric_cardinality_budget` is the primary guard. Leave `metadata_keys`
    empty (the default) unless you have low-cardinality metadata keys to segment on.

### Workspace scope guardrail

Set `settings.expected_workspace` to your workspace slug to prevent a too-broad API key
from silently emitting cross-workspace aggregates. When set, the loop asserts on its first
collection that the API key is scoped to exactly that workspace; a mismatch causes the loop
to refuse to emit until corrected.

### Example groups config

```yaml
loops:
  groups:
    enabled: true
    cadence: 60s
    settings:
      window_span: 1h
      settle: 10m
      emit_cost: true
      emit_prompts: true
      metadata_keys: ""
      expected_workspace: ""
```

---

## Logs export loop

The logs export loop creates and downloads Portkey export jobs, then emits one content-free
OTLP log record per request. Log records carry operational fields (status, latency, tokens,
cost) as structured metadata; they never contain prompt or response text.

### What each log record contains

Each log record includes (as structured metadata, not stream labels):

- `ai_org`, `ai_model`, `response_status_code` — indexed attributes (queryable as Loki stream labels once [GS1 is complete](#grafana-staff-prerequisites))
- `api_key_use_case` — if `api_key_use_cases` is configured
- Request-level operational fields: status, latency, token counts, cost

The record `Body` is never set. The `source` record attribute is set to `"portkey"` so
Portkey and LangSmith log records are distinguishable in Loki.

### Trace correlation

Set `settings.metadata_trace_id_field` (or `settings.trace_id_field`) to the field name
that carries a UUID trace ID in your export. When set, the UUID is promoted to the OTLP
`trace_id` field, enabling logs-to-traces correlation. The two settings are mutually
exclusive.

### Required settings

`workspace_id` and `signed_url_allow_hosts` are required. The signed URL host must match
Portkey's export download host (check your Portkey workspace for the correct value).

### Example logs export config

```yaml
loops:
  logs_export:
    enabled: true
    cadence: 60s
    settings:
      workspace_id: ${PORTKEY_WORKSPACE_ID}
      signed_url_allow_hosts: "your-export-host.example.com"
      window: 1h
      settle: 10m
      max_backfill: 24h
      page_size: 50000
      max_pages_per_window: 50
```

---

## Content governance

All three loops share the same content governance stack: a default-deny field allow-list
(`logs_strip.go`) plus the `source.Guard` defence-in-depth backstop. For the full model,
see [Content Governance](./governance.md).

Opt-in record fields (safe operational fields that are not emitted by default) can be added
via `settings.extra_record_fields`. Hard-denied content fields (`inputs`, `outputs`,
`messages`, `metadata`, `portkeyHeaders`, `gen_ai.*`) cannot be opted in and are rejected
at config load time.

---

## Grafana-staff prerequisites

The indexed attributes on `logs_export` records (`ai_org`, `ai_model`, `response_status_code`)
land as Loki structured metadata until **GS1** (Loki stream-label promotion) is completed on
the target stack. Until then they are filterable via `| ai_model="gpt-4o"` but are not
queryable as `{ai_model="gpt-4o"}`.

---

## See also

- [Telemetry reference](./telemetry.md) — full signal catalogue with config dependencies
- [Content Governance](./governance.md) — field allow/deny model
- [Dashboards](./dashboards.md) — bundled recording rules and dashboard
- [Alerts & Runbooks](./alerts.md) — self-obs alert rules
- [Troubleshooting](./troubleshooting.md) — common failure modes
