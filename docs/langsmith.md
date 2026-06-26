---
title: LangSmith
description: How genai-otel-bridge collects and emits telemetry from the LangSmith eval platform.
---

# LangSmith

genai-otel-bridge collects operational telemetry from LangSmith via two independent loops:
**`sessions`** (per-session aggregate metrics) and **`runs`** (content-free per-run log records).
Each loop is independently enabled via its own `loops.<name>` config block.

!!! info "Content-free by design"
    Neither loop requests or emits prompt inputs, output completions, or message bodies.
    The `runs` loop drops `inputs`, `outputs`, and `messages` by its default-deny allow-list;
    they cannot be opted in. See [Content Governance](./governance.md).

---

## Sessions loop

The sessions loop polls `GET /sessions?include_stats=true` (relative to `base_url`) and emits per-session
aggregate gauges. Unlike the Portkey analytics loop this is **not time-bucketed**: stats are
a rolling snapshot over the configured `stats_window`, and every sample is stamped at
`now.Truncate(1m)` so two polls in the same wall-clock minute share a timestamp (1 data
point per series per minute).

### Emitted metrics

With the default `metric_prefix: langsmith`, the sessions loop emits one series per session
per metric (labelled by the `session` dimension):

| Metric | Unit | Description |
|--------|------|-------------|
| `langsmith_runs` | — | Per-session run count (aggregate-now snapshot) |
| `langsmith_latency_seconds` | s | Per-session latency; one series per `quantile` (p50/p99) |
| `langsmith_first_token_seconds` | s | Per-session time-to-first-token per `quantile`; absent when not streaming |
| `langsmith_tokens` | — | Per-session total token count |
| `langsmith_prompt_tokens` | — | Per-session prompt (input) token count |
| `langsmith_completion_tokens` | — | Per-session completion (output) token count |
| `langsmith_cost_usd` | USD | Per-session total cost |
| `langsmith_prompt_cost_usd` | USD | Per-session prompt cost |
| `langsmith_completion_cost_usd` | USD | Per-session completion cost |
| `langsmith_error_rate` | — | Per-session error rate (ratio) |
| `langsmith_streaming_rate` | — | Per-session streaming rate (ratio) |
| `langsmith_feedback_score` | — | Per-session numeric feedback per `feedback_key` (requires `emit_feedback: true`) |
| `langsmith_feedback_count` | — | Per-session feedback sample count per `feedback_key` |

The `session` label key is fixed (not configurable). The `quantile` and `feedback_key`
labels are allow-listed by the composition root; they are subject to the default-deny
label governance model — see [Content Governance](./governance.md).

### Cardinality note

LangSmith session names can be ephemeral (per-experiment hashes). The `session_label_value`
setting controls whether the `session` label carries the session name or its ID (default: `id`).
Use `session_filter` in production to bound the number of sessions in scope and prevent
unbounded cardinality growth.

### Example sessions config

```yaml
sources:
  - type: langsmith
    enabled: true
    base_url: https://api.smith.langchain.com
    source_instance: langsmith-${ENV}
    auth: { header: x-api-key, value: ${LANGSMITH_API_KEY} }
    loops:
      sessions:
        enabled: true
        cadence: 60s
        window: 1h
        metric_prefix: langsmith
        settings:
          session_filter: "my-app"
          session_label_value: id
          emit_feedback: false
```

---

## Runs loop

The runs loop queries `POST /runs/query` (relative to `base_url`) and emits one content-free OTLP log record
per run. Records carry operational fields (run type, status, latency, token counts, cost,
IDs) as structured metadata; inputs, outputs, and messages are never included.

### What each log record contains

By default each record includes:

- `run_type`, `status` — indexed attributes (queryable as Loki stream labels once GS1 is complete)
- Operational record attributes: `id`, `trace_id`, `session_id`, `parent_run_id`,
  `start_time`, `end_time`, `first_token_time`, token counts, cost fields, `dotted_order`

The record `Body` is never set. The `source` record attribute is set to `"langsmith"` so
Portkey and LangSmith log records are distinguishable in Loki.

### Scope is required

The runs API requires a scope: either a static `session_ids` CSV, or a `session_filter` for
auto-discovery (up to `max_sessions` sessions are discovered and cached with a `session_refresh`
TTL). The loop fails fast at construction if neither is set, to prevent fetching from all
projects at once.

### Trace correlation

Run records include `trace_id` by default in the record attributes tier. This enables
correlation with application traces if the application writes the same trace ID to LangSmith.

### Optional record fields

Safe operational fields not emitted by default can be added via
`settings.extra_record_fields` (for example `app_path`, `tags`). Hard-denied content
fields (`inputs`, `outputs`, `messages`, and the raw-blob pointers `inputs_s3_urls` /
`outputs_s3_urls`) cannot be opted in and are rejected at config load time.

### Example runs config

```yaml
loops:
  runs:
    enabled: true
    cadence: 60s
    settings:
      session_filter: "my-app"
      max_sessions: 100
      session_refresh: 5m
      window: 1h
      settle: 10m
      max_backfill: 24h
      page_size: 100
      max_pages_per_window: 50
      root_only: false
```

---

## Content governance

Both loops share the same content governance stack: a default-deny field strip plus the
`source.Guard` defence-in-depth backstop. For the full model see
[Content Governance](./governance.md).

The `runs` loop uses a scalar-array-as-CSV renderer for array fields when opted in (e.g.
`tags` becomes a comma-separated string). Nested objects and arrays that cannot be rendered
as a flat scalar are dropped.

---

## Grafana-staff prerequisites

The indexed attributes on `runs` records (`run_type`, `status`) land as Loki structured
metadata until **GS1** (Loki stream-label promotion) is completed on the target stack.
Until then they are filterable via `| run_type="chain"` but are not queryable as
`{run_type="chain"}`.

---

## See also

- [Telemetry reference](./telemetry.md) — full signal catalogue with config dependencies
- [Content Governance](./governance.md) — field allow/deny model
- [Dashboards](./dashboards.md) — bundled recording rules
- [Alerts & Runbooks](./alerts.md) — self-obs alert rules
- [Troubleshooting](./troubleshooting.md) — common failure modes
