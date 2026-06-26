# Example: deploying genai-otel-bridge to EKS

A worked example of running genai-otel-bridge on an Amazon EKS cluster, emitting product + self-observability
telemetry directly to Grafana Cloud over the cluster's NAT egress. Adapt the values to your environment.

## Files

| File | Purpose |
|------|---------|
| `values-eks.yaml` | Helm overrides for an EKS deploy (image, pull secret, NetworkPolicy, full `configOverride`). Every secret/identity value is a `${ENV}` ref resolved at runtime from the `genai-otel-bridge-secrets` Secret. |
| `values_sync_test.go` | Gate that keeps `values-eks.yaml`'s config KEYS/layout in lockstep with the schema (values may differ). |
| `make-genai-otel-bridge-secret.sh` | Builds the `genai-otel-bridge-secrets` Secret from your environment (or a dotenv via `GENAI_OTEL_BRIDGE_SECRET_ENV`). |

**Keeping config in step with the schema.** `values-eks.yaml` carries a verbatim `configOverride`, so
`make generate` (which only regenerates the chart's `config:` block) does NOT touch it. Two gate tests in
`values_sync_test.go` guard it: a **key-parity** check (every global config key in the generated
production block must exist here — catches a newly-added key) and a **Load** check (the override must
still parse against the current schema — catches a renamed/removed/retyped key). When `make gate` fails
on a missing key, add it here (the VALUE can be deploy-specific; only the key/layout must match).

## Deploy

```bash
# 1. Create the genai-otel-bridge-secrets Secret in the target namespace (reads creds from your environment).
GENAI_OTEL_BRIDGE_SECRET_ENV=./my.env ./test/eks/make-genai-otel-bridge-secret.sh default

# 2. Ensure an image-pull secret named `regcred` exists in the namespace if the image is private
#    (a kubernetes.io/dockerconfigjson Secret for your registry), then install the chart.
helm upgrade --install genai-otel-bridge ./deploy/helm -n default -f test/eks/values-eks.yaml \
  --atomic --timeout 360s
```

## Verify

```bash
kubectl -n default get pods -l app=genai-otel-bridge -o wide          # 2 pods Running
kubectl -n default get lease genai-otel-bridge-leader                 # leader elected
kubectl -n default get configmap genai-otel-bridge-checkpoints        # watermark store created
kubectl -n default logs -l app=genai-otel-bridge --tail=50 --prefix   # no fatal config/emit errors
```

Then query Grafana Cloud (≈1–2 min after a successful emit): product metrics are `portkey_api_*` /
`langsmith_*` with `deployment_environment="eks-test"`; self-observability is `genai_otel_bridge_*` with
`service_namespace="genai-otel-bridge-meta"` (metrics + traces + push-mode profiles).

## Teardown

```bash
helm uninstall genai-otel-bridge -n default
kubectl -n default delete configmap genai-otel-bridge-checkpoints   # app-created state, not chart-managed
kubectl -n default delete secret genai-otel-bridge-secrets
```

## Notes

- **Private EKS API endpoint:** set `networkPolicy.apiServerCIDR` to your VPC CIDR — kube-apiserver
  traffic is evaluated on the VPC CIDR (not the service IP), and lease leader-election + the ConfigMap
  checkpoint need egress to it.
- **NetworkPolicy enforcement** requires the VPC-CNI network-policy feature (or Calico); otherwise the
  rules are a no-op.
- **Cleartext-emit guard ([CP-M7]):** emit endpoints must be https — routing through an in-cluster
  plain-http OTLP receiver is rejected.
