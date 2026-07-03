---
title: Configuration
description: Narrative walk-through of the genai-otel-bridge config model — emit endpoints, identity, HA, sources, loops, and governance.
---

# Configuration

`genai-otel-bridge` is configured via a single YAML file. Secrets are never stored inline — they are referenced as `${ENV_VAR}` or `file:/path/to/secret` and resolved at load time.

This page is a narrative walk-through of the most important config sections. For an exhaustive key/type/default table, see the [Configuration reference](./config-reference.md). For the generated source-of-truth with inline comments, see [`deploy/helm/values.yaml`](https://github.com/rknightion/genai-otel-bridge/blob/main/deploy/helm/values.yaml).

---

## Secret references

Two syntax forms are supported anywhere in the config:

- `${ENV_VAR}` — resolved from the process environment at startup. An unset variable is a fatal error.
- `file:/path/to/file` — the entire scalar is replaced with the trimmed contents of the file.

```yaml
auth:
  header: x-portkey-api-key
  value: ${PORTKEY_API_KEY}         # env var
emit:
  telemetry:
    otlp:
      token: file:/run/secrets/otlp-token  # file ref
```

Neither form is re-interpreted as YAML: a secret value containing special characters (e.g. `#`, `:`, `{`) is safe.

---

## emit

The `emit` block configures where telemetry is sent.

```yaml
emit:
  telemetry:          # product telemetry (Portkey/LangSmith signals)
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
  self:               # optional: self-observability (bridge's own metrics/traces)
    otlp:
      endpoint: ${META_OTLP_ENDPOINT}
      instance_id: ${META_INSTANCE_ID}
      token: ${META_OTLP_TOKEN}
```

`emit.self` is optional. When unset, self-observability signals are sent to the same endpoint as product telemetry, tagged so they are distinguishable.

Endpoints must be `https://` (or loopback for local dev). For an in-cluster Alloy receiver that accepts cleartext over the pod network, set `allow_insecure: true` — but only with empty credentials (the collector holds the Grafana Cloud credentials, not the bridge).

---

## identity

The `identity` block sets OTLP resource attributes that are applied to every emitted signal.

```yaml
identity:
  service_namespace: genai-otel-bridge
  deployment_environment: ${ENV}    # e.g. dev, staging, prod
```

These three resource attributes (`service.name`, `service.namespace`, `deployment.environment.name`) are in the Grafana Cloud Loki default label set, so they become stream labels. They consume 3 of the 15 default stream-label slots — leave headroom when adding indexed log attributes.

---

## ha

```yaml
ha:
  coordinator: lease      # lease | none | dynamodb
  checkpoint: configmap   # configmap | file | dynamodb
```

- `coordinator: lease` — Kubernetes Lease leader election. Required for multi-replica HA. The bridge uses `coordination.k8s.io/v1` via client-go; RBAC is created by the Helm chart.
- `coordinator: none` — single-replica / dev. The process always acts as leader.
- `coordinator: dynamodb` — DynamoDB lock (AWS ECS deployment target). Requires `checkpoint: dynamodb` (they share one table).
- `checkpoint: configmap` — watermarks stored in a Kubernetes ConfigMap (`genai-otel-bridge-checkpoints`). Required with `coordinator: lease` — the file checkpoint is per-pod and not shared across replicas.
- `checkpoint: file` — local file (dev / non-Kubernetes only). **Not safe with `coordinator: lease`.**
- `checkpoint: dynamodb` — watermarks stored as DynamoDB items (AWS ECS deployment target).

### ECS (`ha.coordinator`/`ha.checkpoint: dynamodb`)

```yaml
ha:
  coordinator: dynamodb
  checkpoint: dynamodb
  dynamodb:
    table: genai-otel-bridge-ha       # required; shared by the lock and the checkpoint
    region: eu-west-1                 # optional; default: AWS_REGION env (SDK-resolved)
    endpoint: ""                      # optional: dynamodb-local / VPC endpoint override
    lock_name: genai-otel-bridge-leader
    key_prefix: ""                    # optional: prepended to every item key (shared-table isolation)
    lease_duration: 15s
    renew_deadline: 10s
    retry_period: 2s
```

`ha.dynamodb.table` is required whenever `coordinator` or `checkpoint` is `dynamodb`; `lease_duration`
must be greater than `renew_deadline`. See [High availability](./high-availability.md) for the failover
model and the [ECS Terraform module](https://github.com/rknightion/genai-otel-bridge/blob/main/deploy/ecs/terraform/README.md)
for a full deployment example.

---

## queue

```yaml
queue:
  max_batches: 256     # per-loop queue depth (batches)
  max_batch_bytes: 1048576   # per-batch size cap (~1 MiB)
  emit_workers: 1      # must be 1; validated at startup
```

The queue is bounded and blocks on full (backpressure, not drop). At approximately one batch per minute this gives roughly four hours of backlog before the queue blocks the scheduler. `emit_workers` must be 1 to preserve per-loop ordering and monotonic watermark advance.

---

## sources

The `sources` list configures one or more vendor integrations.

```yaml
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth:
      header: x-portkey-api-key
      value: ${PORTKEY_API_KEY}
    rate_limit:
      rps: 1
      burst: 3
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
        bucket_settle: 10m
        max_backfill: 90m
        metric_prefix: portkey_api
        graphs: [requests, cost, tokens, latency, errors]
```

**`source_instance`** is part of the `CheckpointKey` — it namespaces the watermark. Use a stable, environment-scoped identifier (e.g. `portkey-prod-eu`). Changing it resets the watermark and triggers a re-bootstrap.

---

## Per-loop knobs

Each loop under a source has:

| Key | Default | Notes |
|---|---|---|
| `cadence` | `60s` | Poll interval (±10% jitter applied by the scheduler). |
| `window` | `50m` | Time range queried per collect. Must be ≤ 55 minutes (Portkey granularity clamp). |
| `bucket_settle` | `10m` | Age at which a bucket is considered final. Live-measured default for Portkey. |
| `bootstrap_lookback` | `50m` | How far back to look on first run or after a watermark reset. |
| `max_backfill` | `90m` | Maximum backfill depth. On Grafana Cloud, the Mimir out-of-order accept window is 2h; 90m leaves margin. |
| `metric_prefix` | `portkey_api` | Prefix for all metrics emitted by this loop. |
| `graphs` | `[requests, cost, tokens, latency, errors]` | Which Portkey analytics graphs to collect. |

The window must satisfy `window ≥ cadence × 1.2 + bucket_settle` to guarantee no time is left uncovered between jittered ticks. The config validator enforces this.

---

## governance

```yaml
governance:
  per_metric_cardinality_budget: 10000
  max_dpm: 1
  max_catchup_per_tick: 1
  max_stream_label_keys: 15
  allow_label_keys: []
```

- **`per_metric_cardinality_budget`** — caps the number of distinct label-sets per metric name. Over-budget series are dropped and counted as `genai_otel_bridge_guard_dropped_total`.
- **`max_dpm`** — hard cap on data points per minute per series, on both planes. Default 1 (the Grafana Cloud standard).
- **`max_stream_label_keys`** — caps how many OTLP resource attributes a single logs loop may contribute as Loki stream labels. The Grafana Cloud Loki default limit is 15 per series; the bridge fails fast at startup if a loop would exceed it.
- **`allow_label_keys`** — extra content-free attribute keys the operator opts into the label allow-list, on top of every registered source's declared keys (the base union is unconditional — not gated on which sources are enabled; every key is content-free by declaration, so a disabled vendor's keys only widen the default-deny surface, never leak). Default empty. See [Content governance](./governance.md).

---

## selfobs

```yaml
selfobs:
  profiling:
    enabled: false
    mode: pull          # pull | push
    pull:
      addr: :6060       # pprof listener address (pull mode)
  tracing:
    enabled: false      # opt-in self-APM tracing of the bridge's own pipeline
```

Both are disabled by default. When `tracing.enabled: true`, spans covering the bridge's own tick→collect→enqueue pipeline are exported over OTLP to the same endpoint as self-metrics.

---

## log

```yaml
log:
  format: logfmt   # logfmt | json
  level: info      # debug | info | warn | error
```

The bridge's own operational logs go to stdout in logfmt (default) or JSON format, scraped by k8s-monitoring and sent to Loki. They are not pushed via OTLP.

---

## What to read next

- [Configuration reference](./config-reference.md) — complete key/type/default table.
- [Portkey guide](./portkey.md) — Portkey-specific loop settings.
- [LangSmith guide](./langsmith.md) — LangSmith-specific loop settings.
- [Content governance](./governance.md) — how the label allow-list and outbound field deny-list work.
- [High availability](./high-availability.md) — ha.coordinator, checkpointing, and failover.
