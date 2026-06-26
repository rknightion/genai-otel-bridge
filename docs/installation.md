---
title: Installation
description: Install genai-otel-bridge from source or deploy it to Kubernetes with the bundled Helm chart.
---

# Installation

`genai-otel-bridge` ships as a statically-linked Go binary. You can run it directly or deploy it to Kubernetes with the bundled Helm chart.

---

## Prerequisites

- Go 1.26 or later (for building from source).
- A Grafana Cloud account (or any OTLP/HTTP endpoint).
- For Kubernetes deployment: Helm 3.x and a cluster with `coordination.k8s.io/v1` available (standard since Kubernetes 1.18).
- For HA / ConfigMap checkpointing: the chart creates the required RBAC automatically.

---

## Option A — binary (quickstart)

### 1. Build

```bash
git clone https://github.com/rknightion/genai-otel-bridge.git
cd genai-otel-bridge
make build
# -> bin/genai-otel-bridge (version stamped via git describe)
```

### 2. Write a config file

Secrets are resolved from `${ENV_VAR}` or `file:/path/to/secret` references at load time. No credentials need to live in the file itself.

```yaml
emit:
  telemetry:
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  service_namespace: genai-otel-bridge
  deployment_environment: ${ENV}
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
        window: 50m
        bucket_settle: 3m
        max_backfill: 55m
        metric_prefix: portkey_api
        graphs: [requests, cost, tokens, latency, errors]
```

See [Configuration](./configuration.md) for a full walk-through of every key, and [Configuration reference](./config-reference.md) for the complete key/type/default table.

### 3. Run

```bash
GC_OTLP_ENDPOINT=https://otlp-gateway-prod-eu-west-0.grafana.net/otlp \
GC_INSTANCE_ID=123456 \
GC_OTLP_TOKEN=... \
ENV=dev \
PORTKEY_API_KEY=... \
  bin/genai-otel-bridge --config config.yaml
```

The binary validates the config at startup and exits immediately on missing required fields or invalid values.

---

## Option B — Kubernetes (Helm)

The bundled chart at `deploy/helm/` deploys a leader-elected, multi-replica Deployment with a ConfigMap checkpoint store, RBAC, NetworkPolicy, and optional ExternalSecret wiring.

### 1. Create the secret

The chart expects a Kubernetes Secret named `genai-otel-bridge-secrets` with the environment variables your config references. The simplest path is to create it manually:

```bash
kubectl create secret generic genai-otel-bridge-secrets \
  --from-literal=GC_OTLP_ENDPOINT=https://otlp-gateway-prod-eu-west-0.grafana.net/otlp \
  --from-literal=GC_INSTANCE_ID=123456 \
  --from-literal=GC_OTLP_TOKEN=... \
  --from-literal=ENV=prod \
  --from-literal=PORTKEY_API_KEY=...
```

Alternatively, use the chart's built-in [ExternalSecret](#externalsecrets) support to provision secrets from a cloud secrets manager.

### 2. Deploy

```bash
helm install genai-otel-bridge ./deploy/helm -f my-values.yaml
```

Override `image:` to point at your registry and tag:

```bash
helm install genai-otel-bridge ./deploy/helm \
  --set image=ghcr.io/rknightion/genai-otel-bridge:v1.0.0 \
  -f my-values.yaml
```

### 3. Verify

```bash
kubectl get pods -l app=genai-otel-bridge
kubectl logs -l app=genai-otel-bridge --tail=50
```

The leader pod logs a `leader=true` line and begins polling. Standby replicas log `leader=false` and idle.

---

## Helm chart reference

The chart at [`deploy/helm/values.yaml`](https://github.com/rknightion/genai-otel-bridge/blob/main/deploy/helm/values.yaml) is the authoritative reference for all chart values with inline comments. The sections below cover the most common customisations.

### Replicas and HA

```yaml
replicas: 2  # active/passive; 3 for an extra standby
```

Two replicas is the lean HA default: one leader polls and emits, one standby takes over within the lease duration (~15 seconds) on failure. Raise to 3 for a second standby during rolling upgrades or node consolidation.

!!! warning
    `ha.coordinator=lease` requires `ha.checkpoint=configmap` (the file checkpoint is per-pod and is not shared across replicas). The config validator rejects the combination `coordinator=lease` + `checkpoint=file`.

### Resources and GOMEMLIMIT

```yaml
resources:
  requests:
    cpu: "100m"
    memory: "256Mi"
  limits:
    cpu: "1"
    memory: "512Mi"
```

The chart wires `limits.memory` to `GOMEMLIMIT` via the downward API, so raising the memory limit also raises the Go GC ceiling.

### NetworkPolicy

The chart renders a default-deny NetworkPolicy. Egress is allowed for:

- DNS (by label selector, not CIDR — portable across CNIs)
- Kubernetes API server (for Lease leader election and ConfigMap checkpoint)
- OTLP endpoint (product telemetry)
- Source API (e.g. `api.portkey.ai`)

The CIDRs default to `0.0.0.0/0` (permissive). Tighten them per environment:

```yaml
networkPolicy:
  apiServerCIDR: "10.0.0.0/16"   # e.g. VPC CIDR on EKS
  otlpEgressCIDR: "10.0.0.0/16"  # PrivateLink endpoint CIDR
  sourceEgressCIDR: "0.0.0.0/0"  # public API (no PrivateLink)
```

On EKS, NetworkPolicy enforcement requires the VPC CNI network-policy feature (or Calico) to be enabled. Without it, the NetworkPolicy objects are created but have no effect.

### RBAC

The chart creates a Role (namespace-scoped, not ClusterRole) with the minimum permissions required:

- `coordination.k8s.io/leases` — get, create, update on the `genai-otel-bridge-leader` resource name.
- `configmaps` — get, create, update on the `genai-otel-bridge-checkpoints` resource name.

The pod cannot read or modify its own config ConfigMap or any other resource.

### ExternalSecrets

When `externalSecrets.enabled: true`, the chart renders a SecretStore and ExternalSecret that populate `genai-otel-bridge-secrets` from a cloud secrets manager (AWS Secrets Manager via ESO). Requires the External Secrets Operator to be installed in the cluster and a separate IRSA-annotated ServiceAccount (provisioned by your infrastructure IaC):

```yaml
aws:
  region: "eu-west-1"
serviceAccount:
  esoName: "genai-otel-bridge-eso"
externalSecrets:
  enabled: true
  grafanaConfigSecret: "my-grafana-config"
  portkeySecret: "my-portkey-secret"
```

### Uninstall cleanup

By default the chart registers a `post-delete` hook that removes the leader Lease and checkpoint ConfigMap when you run `helm uninstall`. Set `cleanup.retainCheckpoint: true` to keep the checkpoint so a later reinstall resumes the watermark instead of bootstrapping from scratch.

---

## What to read next

- [Configuration](./configuration.md) — detailed walk-through of the config model.
- [High availability](./high-availability.md) — leader election, checkpointing, and failover behaviour.
- [Security](./security.md) — secret handling, SSRF guard, and the content-free guarantee.
