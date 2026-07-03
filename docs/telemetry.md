---
title: Telemetry reference
description: Every metric, log, and trace genai-otel-bridge can emit, and which config each depends on.
---
# Telemetry reference

genai-otel-bridge emits two planes of telemetry: **product** (operational signals republished from
the upstream AI platforms — Portkey, LangSmith) and **self-observability** (the bridge's own health).
It is content-free by design: no prompts, completions, inputs, or outputs ever leave the bridge.

Product metric/log **names are config-derived** — the `{loops.*.metric_prefix}` and label-key
placeholders below resolve from your config. With the README's default config, `{loops.analytics.metric_prefix}`
is `portkey_api`, so `{loops.analytics.metric_prefix}_requests` becomes `portkey_api_requests`.

The catalogue below is **generated from the code** (`make generate`) and gate-checked in CI, so it
cannot drift from what the binary actually emits.

<!-- >>> BEGIN generated telemetry catalogue — do not edit by hand; run `make generate` <<< -->

### Product telemetry

#### Metrics

| Name | Kind | Unit | Labels / attributes | Depends on | Description |
|------|------|------|---------------------|-----------|-------------|
| `{loops.analytics.metric_prefix}_cost_usd` | gauge | USD | — | loops.analytics.graphs includes 'cost' | request cost per bucket (US dollars) |
| `{loops.analytics.metric_prefix}_cost_usd_by_metadata` | gauge | USD | metadata_key, metadata_value | loops.groups settings.emit_cost=true with a metadata dimension | cost per metadata-dimension value (÷100 from Portkey cents) |
| `{loops.analytics.metric_prefix}_cost_usd_by_model` | gauge | USD | ai_model | loops.groups settings.emit_cost=true | cost per AI model (÷100 from Portkey cents) |
| `{loops.analytics.metric_prefix}_errors` | gauge | 1 | — | loops.analytics.graphs includes 'errors' | error count per bucket |
| `{loops.analytics.metric_prefix}_latency_seconds` | gauge | s | quantile | loops.analytics.graphs includes 'latency' | request latency statistic per bucket; one series per quantile (avg/p50/p90/p99) |
| `{loops.analytics.metric_prefix}_requests` | gauge | 1 | — | loops.analytics.graphs includes 'requests' | request count per bucket |
| `{loops.analytics.metric_prefix}_requests_by_metadata` | gauge | 1 | metadata_key, metadata_value | loops.groups enabled with a metadata dimension | request count per metadata-dimension value |
| `{loops.analytics.metric_prefix}_requests_by_model` | gauge | 1 | ai_model | loops.groups.enabled=true | request count per AI model (groups ai-models dimension) |
| `{loops.analytics.metric_prefix}_requests_by_prompt` | gauge | 1 | prompt | loops.groups settings.emit_prompts=true | request count per saved-prompt id (content-free — the label is a prompt ID, not text) |
| `{loops.analytics.metric_prefix}_tokens` | gauge | 1 | token_type | loops.analytics.graphs includes 'tokens' | token units per bucket; split by token_type (total/input/output) — do NOT bare-sum across token_type |
| `{loops.analytics.metric_prefix}_users` | gauge | 1 | — | loops.analytics.graphs includes 'users' | distinct-user count per bucket |
| `{loops.sessions.metric_prefix}_completion_cost_usd` | gauge | USD | session | loops.sessions.enabled=true | per-session completion cost (US dollars) |
| `{loops.sessions.metric_prefix}_completion_tokens` | gauge | 1 | session | loops.sessions.enabled=true | per-session completion (output) token count |
| `{loops.sessions.metric_prefix}_cost_usd` | gauge | USD | session | loops.sessions.enabled=true | per-session total cost (US dollars) |
| `{loops.sessions.metric_prefix}_error_rate` | gauge | 1 | session | loops.sessions.enabled=true | per-session error rate (ratio) |
| `{loops.sessions.metric_prefix}_feedback_count` | gauge | 1 | session, feedback_key | loops.sessions settings.emit_feedback=true | per-session numeric feedback sample count; one series per feedback_key |
| `{loops.sessions.metric_prefix}_feedback_score` | gauge | 1 | session, feedback_key | loops.sessions settings.emit_feedback=true | per-session numeric feedback aggregate; one series per feedback_key |
| `{loops.sessions.metric_prefix}_first_token_seconds` | gauge | s | session, quantile | loops.sessions.enabled=true | per-session time-to-first-token; one series per quantile (p50/p99); absent when not streaming |
| `{loops.sessions.metric_prefix}_latency_seconds` | gauge | s | session, quantile | loops.sessions.enabled=true | per-session run latency; one series per quantile (p50/p99) |
| `{loops.sessions.metric_prefix}_prompt_cost_usd` | gauge | USD | session | loops.sessions.enabled=true | per-session prompt cost (US dollars) |
| `{loops.sessions.metric_prefix}_prompt_tokens` | gauge | 1 | session | loops.sessions.enabled=true | per-session prompt (input) token count |
| `{loops.sessions.metric_prefix}_runs` | gauge | 1 | session | loops.sessions.enabled=true | per-session run count (aggregate-now snapshot) |
| `{loops.sessions.metric_prefix}_streaming_rate` | gauge | 1 | session | loops.sessions.enabled=true | per-session streaming rate (ratio) |
| `{loops.sessions.metric_prefix}_tokens` | gauge | 1 | session | loops.sessions.enabled=true | per-session total token count |
| `{loops.usage.metric_prefix}_usage_spans` | gauge | 1 | session, retention_tier | loops.usage settings.emit_span_counts=true | PLATFORM cost driver: spans (all runs) ingested per project = the storage/volume driver; one series per retention_tier |
| `{loops.usage.metric_prefix}_usage_traces` | gauge | 1 | session, retention_tier | loops.usage.enabled=true | PLATFORM cost driver: traces (root runs) ingested per project = the LangSmith billing unit; one series per retention_tier |

#### Logs

| Name | Kind | Unit | Labels / attributes | Depends on | Description |
|------|------|------|---------------------|-----------|-------------|
| `langsmith runs record` | — | — | run_type, status | loops.runs.enabled=true | one content-free OTLP log per run: type/status/latency/tokens/cost as structured metadata; no inputs/outputs |
| `portkey logs_export record` | — | — | ai_org, ai_model, response_status_code, api_key_use_case | loops.logs_export.enabled=true | one content-free OTLP log per request: status/latency/tokens/cost as structured metadata; no prompt/response bodies |

### Self-observability

#### Metrics

| Name | Kind | Unit | Labels / attributes | Depends on | Description |
|------|------|------|---------------------|-----------|-------------|
| `genai_otel_bridge_auth_errors_total` | counter | 1 | loop, source | — | upstream source API responded 401/403 — a credential failure |
| `genai_otel_bridge_bucket_revised_after_settle_age_seconds` | histogram | s | loop | — | age (now − bucketEnd) of a settled bucket observed to change after bucket_settle |
| `genai_otel_bridge_bucket_revised_after_settle_total` | counter | 1 | loop | — | settled buckets observed to change value after settle (late arrival beyond bucket_settle) |
| `genai_otel_bridge_emit_errors_total` | counter | 1 | loop, kind | — | emit errors by kind |
| `genai_otel_bridge_emit_partial_success_rejected_total` | counter | 1 | plane | — | data points or log records the gateway rejected via an OTLP 200 partial_success response (rejected_data_points/rejected_log_records) |
| `genai_otel_bridge_emit_request_duration_seconds` | histogram | s | plane, status_class | — | outbound OTLP emit request latency (per POST attempt to /v1/metrics or /v1/logs) |
| `genai_otel_bridge_emitted_logs_total` | counter | 1 | loop | — | log records emitted (logs-export loop) |
| `genai_otel_bridge_emitted_total` | counter | 1 | loop | — | samples emitted |
| `genai_otel_bridge_guard_dropped_total` | counter | 1 | loop | — | data points or log records dropped by the governance guard |
| `genai_otel_bridge_last_success_timestamp_seconds` | gauge | s | loop | — | unix time of last successful emit |
| `genai_otel_bridge_loop_degraded` | gauge | 1 | loop, reason | — | 1 while a loop is degraded (reason attribute), 0 after the clearing commit |
| `genai_otel_bridge_new_label_values_total` | counter | 1 | series | — | new label-value combinations seen per series |
| `genai_otel_bridge_queue_depth` | gauge | 1 | loop | — | per-loop queue depth |
| `genai_otel_bridge_samples_capped_total` | counter | 1 | loop, reason | — | samples suppressed by the DPM cap (coalesced last-write-wins per series-minute) |
| `genai_otel_bridge_samples_skipped_total` | counter | 1 | loop, reason | — | data points or log records skipped with a counted gap |
| `genai_otel_bridge_source_graph_unavailable_total` | counter | 1 | loop, graph | — | configured source graph skipped on a poll due to a 404 (capability/permission/absence) |
| `genai_otel_bridge_upstream_request_duration_seconds` | histogram | s | target, method, status_class | — | outbound request latency to upstream source APIs (time to response headers) |
| `genai_otel_bridge_window_lag_seconds` | gauge | s | loop | — | now minus the watermark frontier |

#### Traces

| Name | Kind | Unit | Labels / attributes | Depends on | Description |
|------|------|------|---------------------|-----------|-------------|
| `genai-otel-bridge/selfobs (tracer scope)` | — | — | — | self-observability tracing enabled in config | the bridge's own internal spans, exported via the self-observability OTLP TracerProvider (internal/selfobs/tracing.go) |

<!-- >>> END generated telemetry catalogue <<< -->
