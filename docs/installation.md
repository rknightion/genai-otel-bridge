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

The chart names each immutable ConfigMap by the SHA-256 of the mounted `config.yaml`. A config
edit changes the Deployment's volume reference, so a replacement Pod from an old ReplicaSet
still mounts the old file. Config changes always roll the Deployment. `rolloutOnConfigChange: false`
now removes only the optional `checksum/config` annotation. Secret values are not pinned by
config revisions. Do not put credentials in `config` or `configOverride`.

On the first **live Helm upgrade** from the old fixed-name map, the chart looks up the
release-owned `genai-otel-bridge-config`, preserves its data and marks it to keep. The Helm
caller must be able to get that ConfigMap. Do not use an offline render for this migration if
old workloads could lose their map. To restore a known good config, upgrade with the
revision-aware chart and last good values (`--reset-values`), pinning the last good image if
needed. Helm can reuse the kept map or recreate it from those values. Native Helm rollback
replays stored manifests; it does not run the prune hook. Some Helm clients cannot roll back
to a retained map absent from the current manifest. Do not delete a map to make rollback work.

**Argo CD:** each new revision opts out of both sync pruning (`Prune=false`) and Application
deletion (`Delete=false`). Helm's `keep` annotation alone does not stop Argo CD sync pruning.
Before the first sync from the fixed-name map, disable sync pruning temporarily or ensure the
old live map has both options and self-heal will not remove them. Preserve the old map until
no workload references it. Argo CD uses offline rendering and does not run this chart's
revision pruner. Keep the prune value off and arrange manual cleanup. Retained maps may appear
out of sync until removed.

#### Pruning old revisions

`configRevisionPruning.enabled` defaults to `false`. No revision delete permissions are added
by default, and unreferenced maps can accumulate. Opt in only for live `helm upgrade`:

```yaml
configRevisionPruning:
  enabled: true
  historyLimit: 3
```

When using `--reuse-values` with a release from before this option existed, set both fields
explicitly to opt in. An enabled hook requires a non-negative limit, not `null`.

On a successful post-upgrade run, the Job keeps the desired revision, those referenced by the
bridge Deployment, any Pod or ReplicaSet in the namespace (including scaled-down ReplicaSets),
and the newest `historyLimit` **unused** revisions. Set the limit to `0` for no unused history.
Referenced history is not capped by this setting. If Helm waits for a rollout and times out,
or hooks are disabled, no pruning occurs. No pruning runs at install, rollback or uninstall.
Reserve revision names for the bridge; do not refer to them from other workload templates or
change references outside Helm while pruning runs. The reference scan is not atomic with delete.

The Helm caller needs namespace ConfigMap list access to discover candidates. The Job gets and
deletes only the discovered, release-owned old names, not the desired map or checkpoint. It
also reads workload specs to find references. It has its own ServiceAccount, no Secret access,
and needs Kubernetes API reachability and a cluster compatible with the pinned utility image's
kubectl version. It rechecks ownership and uses ConfigMap UID and resource-version preconditions.

**Failed admission can leave an active grant.** Helm creates the ServiceAccount, Role and
RoleBinding before the Job. A quota or admission denial, or a failed or interrupted hook, can
leave the RoleBinding behind. Setting `enabled: false` does not revoke a leftover grant. After
the Helm operation has stopped, remove the RoleBinding first, then the remaining hook resources:

```bash
kubectl -n <namespace> delete rolebinding genai-otel-bridge-config-revisions --ignore-not-found
kubectl -n <namespace> delete job,role,serviceaccount genai-otel-bridge-config-revisions --ignore-not-found
```

For manual cleanup, list candidates by revision label, then check their owning release and
all Deployment, ReplicaSet and Pod references in the namespace:

```bash
kubectl -n <namespace> get configmaps -l app.kubernetes.io/component=config-revision
```

Keep referenced maps and any unused history you want. Delete only exact names you have checked,
including the old fixed-name map after migration or uninstall once its consumers are gone. Never
delete by label alone or remove the checkpoint ConfigMap as part of revision cleanup.

### Uninstall cleanup

By default the chart registers a `post-delete` hook that removes the leader Lease and checkpoint ConfigMap when you run `helm uninstall`. Set `cleanup.retainCheckpoint: true` to keep both (the Lease carries the leader epoch the checkpoint was written under) so a later reinstall resumes the watermark instead of bootstrapping from scratch.

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
