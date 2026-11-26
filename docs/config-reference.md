---
title: Configuration reference
description: Complete reference for every genai-otel-bridge config key, with types, defaults, and descriptions.
---

# Configuration reference

This page lists the configuration keys outside the vendor `settings` maps, with types, defaults, and a brief description. The per-loop `settings` keys are vendor-specific: see [Portkey](./portkey.md) and [LangSmith](./langsmith.md), and the complete generated example block in `deploy/helm/values.yaml`. The generated source of truth with inline comments is [`deploy/helm/values.yaml`](https://github.com/rknightion/genai-otel-bridge/blob/main/deploy/helm/values.yaml). The Go struct definitions and validation rules are in [`internal/config/config.go`](https://github.com/rknightion/genai-otel-bridge/blob/main/internal/config/config.go).

For a narrative walk-through, see [Configuration](./configuration.md).

---

## emit

| Key | Type | Default | Description |
|---|---|---|---|
| `emit.telemetry.otlp.endpoint` | string | _(required)_ | Grafana Cloud OTLP gateway base URL (no trailing `/v1/metrics`). Must be `https://` or loopback. |
| `emit.telemetry.otlp.instance_id` | string | `${GC_INSTANCE_ID}` | Grafana Cloud instance ID (Basic auth username). |
| `emit.telemetry.otlp.token` | string | `${GC_OTLP_TOKEN}` | Grafana Cloud access-policy token (Basic auth password). |
| `emit.telemetry.otlp.allow_insecure` | bool | `false` | Opt out of the https-only gate for an in-cluster cleartext OTLP receiver. Requires empty credentials and a private/DNS target. |
| `emit.self.otlp.*` | — | _(optional)_ | Same structure as `emit.telemetry.otlp`. When the whole `emit.self` block is absent, self-observability signals use the product endpoint. A present `emit.self` block with an empty `otlp.endpoint` is rejected. |
| `emit.self.metric_interval` | duration | `60s` | Self-obs PeriodicReader export period. Must be at least `60s / governance.max_dpm` (60s at the default `max_dpm` of 1); unset means 60s. |

Self-observability histogram instruments use a code-owned base2 exponential aggregation view with
`MaxSize: 160` and `MaxScale: 20`. The OTel-Go SDK also recognizes
`OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION=base2_exponential_bucket_histogram` for its
default histogram selector, but this service does not rely on deployment environment configuration:
the explicit view remains authoritative.

---

## identity

| Key | Type | Default | Description |
|---|---|---|---|
| `identity.service_namespace` | string | `genai-otel-bridge` | OTLP resource attribute `service.namespace`. Appears as a Loki stream label. |
| `identity.deployment_environment` | string | `${ENV}` | OTLP resource attribute `deployment.environment.name` (e.g. `dev`, `prod`). |

---

## ha

| Key | Type | Default | Description |
|---|---|---|---|
| `ha.coordinator` | string | `lease` | `lease` — Kubernetes Lease leader election. `none` — single-replica / dev. `dynamodb` — DynamoDB lock (ECS/AWS). |
| `ha.checkpoint` | string | `configmap` | `configmap` — watermarks in a Kubernetes ConfigMap (required with `coordinator=lease`). `file` — local file (dev only; unsafe with `coordinator=lease`). `dynamodb` — DynamoDB item (ECS/AWS). |
| `ha.dynamodb.table` | string | _(required when coordinator\|checkpoint is `dynamodb`)_ | DynamoDB table backing both the leader lock and the checkpoint (one table for both). |
| `ha.dynamodb.region` | string | AWS SDK default (`AWS_REGION` env) | AWS region override. |
| `ha.dynamodb.endpoint` | string | — | Optional endpoint override (e.g. `dynamodb-local` or a VPC endpoint). |
| `ha.dynamodb.lock_name` | string | `genai-otel-bridge-leader` | Leader-lock item key. |
| `ha.dynamodb.key_prefix` | string | — | Optional prefix prepended to every item key (shared-table isolation). |
| `ha.dynamodb.lease_duration` | duration | `15s` | Leader lease TTL. Must be greater than `renew_deadline`. |
| `ha.dynamodb.renew_deadline` | duration | `10s` | Deadline for the leader to renew its lease before giving it up. |
| `ha.dynamodb.retry_period` | duration | `2s` | Poll interval for lock acquisition/renewal retries, and the renew cadence. Must be > 0 and less than `renew_deadline`. |

---

## queue

| Key | Type | Default | Description |
|---|---|---|---|
| `queue.max_batches` | int | `256` | Per-loop queue capacity (batches). Accepted for compatibility: collection is single-flight with the emit, so at most one batch is ever queued or in flight and no backlog is buffered. |
| `queue.max_batch_bytes` | int | `1048576` | Per-batch size cap in bytes (~1 MiB). Over-cap batches are split proactively; a 413 from the gateway triggers a reactive split. |
| `queue.emit_workers` | int | `1` | Must be 1. Per-loop single-flight emit so the watermark advances monotonically. |

---

## governance

| Key | Type | Default | Description |
|---|---|---|---|
| `governance.per_metric_cardinality_budget` | int | `10000` | Max distinct label-sets per metric name active within the last hour. Over-budget series are dropped and counted as `genai_otel_bridge_guard_dropped_total`. |
| `governance.max_dpm` | int | `1` | Hard cap on data points per minute per series (both planes). Drives the product-plane LWW coalesce and clamps the self-obs PeriodicReader interval. |
| `governance.max_catchup_per_tick` | int | `1` | Max windows drained per cadence period when a loop is backlogged, and max pages or chunks per cadence for a logs loop mid-window (`runs`, `logs_export`). `1` = no catch-up acceleration (the default). |
| `governance.max_stream_label_keys` | int | `15` | Max OTLP resource attributes a single logs loop may contribute as Loki stream labels. The Grafana Cloud Loki default ceiling is 15; the bridge fails fast at startup if a loop would exceed it. |
| `governance.allow_label_keys` | []string | `[]` | Extra content-free attribute keys added to the label allow-list on top of each source's declared keys. Content-floor keys (message bodies, PII) are rejected at startup. |

---

## log

| Key | Type | Default | Description |
|---|---|---|---|
| `log.format` | string | `logfmt` | `logfmt` or `json`. Applies to the bridge's own operational stdout logs (not OTLP). |
| `log.level` | string | `info` | `debug`, `info`, `warn`, or `error`. |

---

## selfobs

| Key | Type | Default | Description |
|---|---|---|---|
| `selfobs.profiling.enabled` | bool | `false` | Enable continuous profiling of the bridge's own runtime. |
| `selfobs.profiling.mode` | string | `pull` | `pull` — expose `net/http/pprof` on a dedicated listener. `push` — push to Grafana Cloud Profiles via the pyroscope-go agent. |
| `selfobs.profiling.pull.addr` | string | `:6060` | pprof listener address (pull mode). |
| `selfobs.profiling.push.endpoint` | string | _(required when push)_ | Grafana Cloud Profiles ingest URL. Must be `https://`. |
| `selfobs.profiling.push.instance_id` | string | _(required when push)_ | Grafana Cloud instance ID for profiles. |
| `selfobs.profiling.push.token` | string | _(required when push)_ | Grafana Cloud access-policy token for profiles (requires `profiles:write` scope). |
| `selfobs.tracing.enabled` | bool | `false` | Enable opt-in self-APM tracing of the bridge's own poll/emit pipeline. Spans are exported to the same endpoint as self-metrics. |

---

## sources

Each entry in the `sources` list has:

| Key | Type | Default | Description |
|---|---|---|---|
| `type` | string | `portkey` | Source type. Currently: `portkey`, `langsmith`. |
| `enabled` | bool | `true` | Enable this source. |
| `base_url` | string | `https://api.portkey.ai/v1` | Source API base URL. Must be `https://` unless `http.allow_private=true`. |
| `source_instance` | string | `portkey-${ENV}` | Stable per-environment identifier. Part of the CheckpointKey; changing it resets the watermark. Must not contain `/`. |
| `auth.header` | string | `x-portkey-api-key` | HTTP request header name for the API key. |
| `auth.value` | string | `${PORTKEY_API_KEY}` | API key value (use a `${ENV_VAR}` or `file:` reference). |
| `rate_limit.rps` | float | `1` | Sustained outbound request rate (requests/second). |
| `rate_limit.burst` | int | `3` | Token bucket burst size. |
| `http.user_agent` | string | — | Override User-Agent (required for some endpoints, e.g. LangSmith behind a WAF). |
| `http.allow_hosts` | []string | — | Hostname allow-list for the egress/SSRF guard on this source's outbound client. Empty ⇒ any host that passes the IP guard; non-empty restricts requests (incl. redirects) to exactly these hosts, so `base_url`'s host must be included. |
| `http.allow_private` | bool | `false` | Allow non-loopback, non-`https` base URLs (for in-VPC sources). |
| `api_key_use_cases` | list | — | Maps human use-case labels to Portkey API key UUIDs. See the [Portkey guide](./portkey.md). |

### Per-loop config (`sources[].loops.<name>`)

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Enable this loop. |
| `cadence` | duration | `60s` | Poll interval (±10% jitter). Must be ≥ 10s. |
| `window` | duration | `50m` | Time range queried per collect. Must be ≤ 55m (Portkey granularity clamp). |
| `bucket_settle` | duration | `10m` | Age at which a bucket is considered final. |
| `bootstrap_lookback` | duration | `50m` | How far back to bootstrap on first run or watermark reset. Must be ≤ `max_backfill`. |
| `max_backfill` | duration | `90m` | Maximum backfill depth. On Grafana Cloud, the Mimir out-of-order accept window is 2h; 90m leaves margin. |
| `metric_prefix` | string | `portkey_api` | Prefix applied to all metrics emitted by this loop. |
| `graphs` | []string | `[requests, cost, tokens, latency, errors]` | Which Portkey analytics graphs to collect. |
| `settings` | map[string]string | — | Source-specific knobs. Each source package documents its own keys. See the [Portkey guide](./portkey.md) and [LangSmith guide](./langsmith.md). |

!!! note "Window constraint"
    The window must satisfy `window ≥ cadence × 1.2 + bucket_settle` to ensure no time is left uncovered between jittered ticks. The config validator enforces this and reports the constraint in the error message.

---

## Validation rules

The validator, `Config.Validate` in `internal/config/config.go`, is the authoritative list and runs at startup. These are the main rules, not every check:

- `emit.telemetry.otlp.endpoint` is required.
- `queue.emit_workers` must be exactly 1.
- `ha.checkpoint=file` with `ha.coordinator=lease` is rejected.
- `ha.coordinator=dynamodb` requires `ha.checkpoint=dynamodb` (they share one table).
- `ha.dynamodb.table` is required whenever `ha.coordinator` or `ha.checkpoint` is `dynamodb`.
- `ha.dynamodb.lease_duration` must be greater than `ha.dynamodb.renew_deadline`, `ha.dynamodb.retry_period` must be > 0 and less than `renew_deadline`, and none of the three may be negative (all checked when `ha.coordinator=dynamodb`).
- `emit.telemetry.metric_interval` is rejected (it is not honoured; use `emit.self.metric_interval`).
- A present `emit.self` block needs `otlp.endpoint`, and `emit.self.metric_interval` must be at least `60s / governance.max_dpm`.
- Each source needs a unique `source_instance`, `rate_limit.rps` > 0 and `rate_limit.burst` ≥ 1.
- Enabled self-profiling is cross-checked: `selfobs.profiling.push.*` only with `mode: push`, `pull.addr` only with `mode: pull`.
- `source_instance` must not contain `/`.
- `source.base_url` must be `https://` unless `http.allow_private=true`.
- `auth.header` and `auth.value` are required for every enabled source.
- `cadence` must be ≥ 10s.
- `window` must be ≤ 55m and satisfy the `cadence×1.2+bucket_settle` lower bound.
- `bootstrap_lookback` must be ≤ `max_backfill`.
- `governance.max_dpm`, `governance.per_metric_cardinality_budget`, `governance.max_catchup_per_tick`, `governance.max_stream_label_keys` must be ≥ 0.
- `log.format` must be `logfmt`, `json`, or empty (defaults to `logfmt`).
- `log.level` must be `debug`, `info`, `warn`, `error`, or empty (defaults to `info`).

---

## Tenant shape

A config whose `tenants` map has at least one entry has the tenant shape, from the version that
enables it. `sources` must then be empty and `emit.telemetry` must have no endpoint, instance id or
token; with no tenants the legacy shape applies and `defaults` must be absent. `governance`, `queue`, `ha`,
`log` and `selfobs` apply to the whole install. The guide is [Multi-tenancy](./multi-tenancy.md).

| Key | Meaning |
| --- | --- |
| `emit.self.otlp` | Required. Where self-telemetry goes; it never falls back to a tenant's destination. |
| `defaults.<type>.loops.<loop>` | The loop blocks every target of that source type uses. Each must be complete. A target has no `loops` block. |
| `tenants.<id>.identity` | Required, with `service_namespace` and `deployment_environment`. It does not fall back to the top-level `identity`. |
| `tenants.<id>.destinations.<name>.otlp` | `endpoint`, `instance_id` and `token_file`, all required. `token_file` is a bare file name in the tenant's secrets directory. |
| `tenants.<id>.targets.<name>.source` | `type`, `base_url`, `auth` (`header` and `value_file`), `rate_limit` and the optional HTTP settings. No `enabled`, `source_instance` or `loops`. |
| `tenants.<id>.targets.<name>.scope` | Required, with `name` and `id`: the one workspace the key reads. |
| `tenants.<id>.targets.<name>.destination` | The name of a destination of the same tenant. |
| `tenants.<id>.targets.<name>.identity` | Optional. Replaces the tenant's identity whole. |

Rules the loader and the config check enforce:

- Tenant, destination and target names match `[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?`.
- Nothing under `tenants` or `defaults` is an `${ENV}` or `file:` reference. Secrets are files named by
  `token_file` and `value_file`.
- Every URL and endpoint is `https://` (a destination has no plain-text setting).
- No two tenants share a stack, and no scope name or id appears in two tenants.
- Two targets of one tenant that share a stack must differ in identity.
- A target must not set `settings.expected_workspace`: the scope replaces it.
- `ha.coordinator` must be `lease` and `ha.checkpoint` must be `configmap`. A loop that emits log records is refused.

Two rules need the tenants' secrets and so are checked when the pod starts, not by the config
check: an unreadable secret file and a source API key shared by two targets. Each blocks the tenant
concerned only.

---

## What to read next

- [Portkey guide](./portkey.md) — Portkey `settings` keys.
- [LangSmith guide](./langsmith.md) — LangSmith `settings` keys.
- [Content governance](./governance.md) — `governance.allow_label_keys` and the outbound field deny-list.
- [High availability](./high-availability.md) — `ha.*` in depth.
