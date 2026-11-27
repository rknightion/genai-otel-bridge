---
title: Installation
description: Install genai-otel-bridge from source or deploy it to Kubernetes with the bundled Helm chart.
---

# Installation

`genai-otel-bridge` ships as a statically-linked Go binary. You can run it directly or deploy it to Kubernetes with the bundled Helm chart.

---

## Prerequisites

- Go 1.27 or later (for building from source).
- A Grafana Cloud account (or any OTLP/HTTP endpoint).
- For Kubernetes deployment: Helm 3 or 4 and a cluster supported by the chart's `kubeVersion`.
  For native Helm rollback, see [Config revisions](#config-revisions).
- For HA / ConfigMap checkpointing: the chart creates the required RBAC automatically.

---

## Option A — binary (quickstart)

### 1. Build

```bash
git clone https://github.com/rknightion/genai-otel-bridge.git
cd genai-otel-bridge
just build
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
ha:
  coordinator: none                       # one local process; the Helm chart uses lease + configmap
  checkpoint: file
queue:
  emit_workers: 1
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
        bucket_settle: 10m
        bootstrap_lookback: 50m
        max_backfill: 90m
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
  bin/genai-otel-bridge --config config.yaml --checkpoint-file ./checkpoints.yaml
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

To load further env from Secrets or ConfigMaps managed by other tools, list them in `extraEnvFrom` as standard `envFrom` entries (`secretRef` or `configMapRef`); they are appended after `genai-otel-bridge-secrets`.

### 2. Deploy

```bash
helm install genai-otel-bridge ./deploy/helm -f my-values.yaml
```

Override `image:` to point at your registry and tag:

```bash
helm install genai-otel-bridge ./deploy/helm \
  --set image=ghcr.io/rknightion/genai-otel-bridge:1.0.0 \
  -f my-values.yaml
```

### 3. Verify

```bash
kubectl get pods -l app=genai-otel-bridge
kubectl logs -l app=genai-otel-bridge --tail=50
```

The leader pod logs `leader elected; starting loops` and begins polling. Standby replicas log nothing at the default level and idle. `kubectl get lease genai-otel-bridge-leader -o jsonpath='{.spec.holderIdentity}'` names the current leader.

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
  grafanaConfigSecret: "my-grafana-config"              # properties: otlp-url, otlp-username
  grafanaSelfTokenSecret: "my-grafana-self-token"       # property: token (self-observability, always required)
  grafanaProductTokenSecret: "my-grafana-product-token" # property: token (omit when product telemetry goes to an in-cluster collector)
  portkeySecret: "my-portkey-secret"                    # property: api_key
runtimeEnv:
  env: "prod"                                           # fills the ${ENV} refs in the default config
```

The chart refuses to render without `grafanaSelfTokenSecret`. The default config also reads the product `GC_OTLP_*` values, which are provisioned only when `grafanaProductTokenSecret` is set, and `${ENV}`, which comes from `runtimeEnv.env` because the ExternalSecret does not carry it.

### Config revisions

The chart hashes the exact bytes of `config.yaml` and mounts the immutable ConfigMap named for
that hash. A config change rolls the Deployment by changing its mount, even with
`rolloutOnConfigChange: false`; that setting now controls only the optional checksum annotation.
Secret updates are separate and do not change the ConfigMap revision. Never put credentials in
`config` or `configOverride`.

On the first live `helm upgrade` from the fixed-name ConfigMap, Helm preserves that map's bytes
for older Pods. Use a live Helm upgrade for this migration, not an offline render with automatic
pruning. The Helm caller must be allowed to list ConfigMaps in the namespace for discovery.

After a live install or upgrade, a hook keeps revisions referenced by the bridge Deployment,
Pods and ReplicaSets (including scaled-down ReplicaSets), plus the `configRevisionHistoryLimit`
newest unused revisions. Set the limit to 0 to keep no unused revisions. If hooks are disabled or
`--wait` times out before the hook runs, pruning waits for the next successful live upgrade.
Do not change workload mount references outside Helm while pruning is running.

A native `helm rollback` can reuse a retained ConfigMap or recreate a pruned one from Helm release
history. Use Helm 3.22+ or 4.2+ for this. Older clients can fail if the rollback target's ConfigMap
still exists but is not in the current manifest. On an older client, use `helm upgrade --reset-values`
with a revision-aware chart, the last good values and, if needed, the previous image pinned in those
values. Do not delete the target ConfigMap to make rollback work. Rollback does not prune: it replays
stored manifests rather than discovering names again. The next live upgrade does the pruning.

The pruning Job has a separate ServiceAccount and no Secret access. Allow the pinned k3s utility
image to be pulled and the Job to reach the Kubernetes API. Its Role allows ConfigMap reads and
deletes only by exact name: the newly rendered name and release-owned revisions discovered at
render time. The Job rechecks ownership and workload references, and supplies the ConfigMap UID
and resource version when deleting. RBAC is name-based, so reserve these names for the chart.
The bridge's own Role is unchanged.

Offline `helm template` cannot discover old names. For GitOps or direct application of manifests,
omit Helm hooks (`helm template --no-hooks` for a standalone render), disable automatic pruning of
these ConfigMaps, and arrange separate cleanup once nothing references a revision.

### Uninstall cleanup

Use `helm uninstall --cascade=foreground --wait` so Kubernetes removes Pods and ReplicaSets before
the config hook runs. Uninstall uses the names saved with the last applied release. It leaves any
still-referenced map or one outside that saved set, for example after rollback. A live upgrade
refreshes discovery; otherwise remove leftovers only once no workload references them.

By default a separate `post-delete` hook removes the leader Lease and checkpoint ConfigMap. Set
`cleanup.retainCheckpoint: true` to keep both (the Lease carries the leader epoch the checkpoint
was written under) so a later reinstall resumes the watermark instead of bootstrapping from scratch.

---

## Tenants (Helm)

Set `config.tenants` and `config.defaults` (or a whole `configOverride`) and the chart renders the
config without `sources` and `emit.telemetry`. It mounts one optional, read-only Secret per tenant,
named `genai-otel-bridge-tenant-<tenant id>`, and refuses to render unless
`rollingUpdate.maxUnavailable` is 0. A Secret change does not restart the pods. The worked values are
`deploy/helm/examples/tenants-values.yaml`; the procedure, the restart command and the rollback
are in [Multi-tenancy](./multi-tenancy.md). Converting an existing install is two steps, not one upgrade: see
[Converting a legacy install](./multi-tenancy.md#converting-a-legacy-install). The NetworkPolicy's OTLP egress
range must cover every tenant's destination
([A narrowed NetworkPolicy](./multi-tenancy.md#a-narrowed-networkpolicy)).

---

## What to read next

- [Configuration](./configuration.md) — detailed walk-through of the config model.
- [High availability](./high-availability.md) — leader election, checkpointing, and failover behaviour.
- [Security](./security.md) — secret handling, SSRF guard, and the content-free guarantee.
