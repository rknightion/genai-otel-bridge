# deploy — Helm chart + dashboards/alerting

## helm/ (chart v0.5.0)

2-replica (active/passive; raise to 3 for a second standby) Deployment, namespaced (least-privilege),
default-deny egress **and** ingress. A render guard fails on `ha.coordinator=none` + `replicas>1`.
`templates/_helpers.tpl` defines `genai-otel-bridge.labels` / `genai-otel-bridge.selectorLabels` — the common label block is
included everywhere (DRY). **`selectorLabels` is the immutable Deployment/PDB selector — kept minimal
(`app: genai-otel-bridge`); never fold version/instance into it or `helm upgrade` breaks.**

- `templates/deployment.yaml` — non-root (UID 65532), seccomp RuntimeDefault, pod topology spread by
  **host AND zone** (`kubernetes.io/hostname` + `topology.kubernetes.io/zone`, both `ScheduleAnyway` so a
  single-node/single-AZ dev cluster still schedules — on multi-AZ EKS the standby lands in a different AZ),
  optional **scheduling knobs** `nodeSelector`/`tolerations`/`affinity`/`priorityClassName` (all default
  no-op via `{{- with }}`, for Karpenter node pools / taints / priority on EKS),
  downward API exposes POD_NAME + container memory limit (→ `GOMEMLIMIT`, from `resources.limits.memory`),
  **`terminationGracePeriod 300s`** to cover the emit-retry budget. `resources` is a standard
  requests/limits map in `values.yaml` (not the old `memoryLimit` scalar). Bounded `RollingUpdate`
  (maxUnavailable/maxSurge 1) + `revisionHistoryLimit: 3`. A **`checksum/config` pod annotation**
  (gated by `rolloutOnConfigChange`, default true) hashes `templates/configmap.yaml` so a config change
  rolls the pods — it hashes ONLY the `genai-otel-bridge-config` CM, NOT the app-created `genai-otel-bridge-checkpoints` state
  CM. Shutdown ordering: the lease is never released — it expires (see `internal/coordinate`) — so a
  standby can't acquire mid-shutdown; `leaderCtx` cancels immediately on SIGTERM (hard-cancel, not a
  drain), so nothing new persists after cancel. The grace window bounds time-to-SIGKILL, not a
  drain-to-completion period.
- `templates/pdb.yaml` — PodDisruptionBudget (`policy/v1`), `maxUnavailable: 1` (gated by
  `podDisruptionBudget.enabled`). Serialises node drains / Karpenter consolidation at replicas:3; a
  no-op at replicas:1 (no dev drain deadlock). `maxUnavailable` not `minAvailable` for that reason.
- `templates/rbac.yaml` — **Role, not ClusterRole**, namespace-scoped AND **resource-name-scoped**:
  `leases` get/update only on `genai-otel-bridge-leader`, `configmaps` get/update only on `genai-otel-bridge-checkpoints`
  (so the pod can't touch its own `genai-otel-bridge-config`). `create` stays unscoped — RBAC `resourceNames`
  can't gate `create` (the name doesn't exist yet).
- `templates/networkpolicy.yaml` — **default-deny ingress** except the health port (8080, + pprof in
  pull mode) from any source: self-metrics are OTLP-pushed (nothing scrapes the pod), so the kubelet
  probe is the only legitimate inbound; allowing just that port from anywhere keeps probes working on
  every CNI (a default-deny-all-ingress rule is a CNI-dependent footgun — CP-H10c). Then default-deny
  egress, then: **DNS by label-selector** (kube-system +
  `k8s-app=kube-dns`, NOT a CIDR — a CIDR silently breaks DNS on k3s/EKS because kube-router/VPC-CNI
  evaluate egress on the post-DNAT CoreDNS *pod* IP, CP-H10b); **explicit kube-apiserver egress**
  (`apiServerCIDR`, REQUIRED for lease+checkpoint, decoupled from OTLP so the OTLP CIDR can be
  tightened independently); OTLP (443) + source API (443) via CIDRs; Cilium `toFQDNs` pattern for FQDN
  egress. **EKS:** enforcement needs the VPC-CNI network-policy feature (or Calico) enabled.
- `templates/cleanup-hook.yaml` — **`post-delete` uninstall cleanup.** The binary creates the
  `genai-otel-bridge-leader` Lease and `genai-otel-bridge-checkpoints` ConfigMap at runtime, so `helm uninstall` doesn't
  track them and would orphan them. This hook (gated by `cleanup.enabled`, default true) runs a Job
  `genai-otel-bridge -cleanup` to delete them. **post-delete, not pre-delete** — pre-delete runs while the
  Deployment is still up, so the live leader would re-create the lease/checkpoint right after deletion;
  post-delete runs after the workload is gone. Uses a **dedicated, ephemeral, delete-only,
  resourceName-scoped** SA/Role/RoleBinding (also annotated `post-delete`, hook-weights order RBAC
  before the Job) — NOT the app's Role: by post-delete the app RBAC is already deleted, and the running
  app must never carry `delete` (least-privilege). `cleanup.retainCheckpoint` (default false) keeps the
  checkpoint CM for resume-on-reinstall (the lease is always removed) and drops the configmap grant
  from the hook Role. A failed hook degrades to today's orphan behaviour, never blocks the uninstall.
- `templates/configmap.yaml` — runtime config (emit endpoints, identity, HA backends, queue bounds,
  per-loop config). Renders `{{- if .Values.configOverride }}` (verbatim string, e.g. the e2e's
  fully-resolved config) `{{- else }} {{ .Values.config | toYaml | indent 4 }}`. `values.yaml`
  carries replica/image/resources/grace + rollout/PDB knobs + NetworkPolicy CIDRs + the GENERATED
  `config:` map.

### Generated default config (do not hand-edit)

The `config:` map in `values.yaml` (between the `# >>> BEGIN/END generated config <<<` markers) is
**generated from the Go config schema** by `make generate` — **never hand-edit it.** The `helm:"..."` tag
semantics, the regen workflow, and the `TestHelmGeneratedConfigUpToDate` drift gate are documented in
`internal/config/CLAUDE.md` (the generator's owner — don't duplicate that detail here). Chart-side
specifics: `toYaml` strips comments and sorts keys (fine — the rendered configmap is just data; `${ENV}`
refs are scalar strings and survive it). The verbatim-override path is `configOverride` (string) — a
`config:` string would be a map type-mismatch, so e2e values use `configOverride:`.

## grafana/ — Grafana resources as code (gcx-native)

Alert/recording rules **and dashboards** live as **gcx-native resource manifests**
(`rules.alerting.grafana.app/v0alpha1` `AlertRule`/`RecordingRule`, `folder.grafana.app` `Folder`,
`dashboard.grafana.app/v2` `Dashboard`) — applied with `gcx resources push -p deploy/grafana/<role> --context <stack>`. **This replaced the old Prometheus-rule-group `dashboards/recording-rules.yaml`.** Manifests are **stack-agnostic** (no `namespace`/stack id — gcx derives it from `--context`), split by ROLE not stack name:
- `grafana/self-obs/` (the integrator's own o11y, `genai_otel_bridge_*`) — **deploy-by-default, push the whole dir:**
  - **Dashboard** `genai-otel-bridge-self-obs`: v2 `TabsLayout` + responsive `AutoGridLayout`, 7 tabs (Overview/SLO,
    Liveness, Emit pipeline, Upstream, Cardinality, Logs, Profiling), 3 datasource vars (Prometheus/Loki/
    Pyroscope) + `$loop`. Generated from `gen_dashboard.py` (tracked Python; `make gen-dashboard` → commit YAML).
  - **11 alerts**: LeaderAbsent, PollerStale (**self-relative**: 2× the loop's own 6h-p90 staleness baseline,
    not a flat threshold — flat false-positived ~100×/day on the log-export loops), EmitFailing, AuthErrors,
    UpstreamErrorBudget, WindowTruncatedDroppingRecords, DataLoss, BucketRevisedAfterSettle, QueueBackpressure,
    CardinalitySpike, NoStandby. Self-contained (no rule dependency); `noDataState: Ok` (NB the enum is `Ok`/
    `NoData`, **not** `OK` — gcx 403s on `OK`).
  - **recording rules**: `genai-otel-bridge:last_success_age:seconds`, `:baseline6h`, `genai-otel-bridge:freshness_ratio`,
    `genai-otel-bridge:upstream_error_ratio:5m` (the dynamic-threshold seam the dashboard colours on) + the originals
    `genai-otel-bridge:window_truncated:rate5m`, `genai-otel-bridge:scrape_healthy`, `genai-otel-bridge:scrape_present`.
- `grafana/product/` (product telemetry): `portkey:requests:sum_5m`, `portkey:error_ratio:5m` (over `portkey_api_*`),
  and `langsmith:runs:sum_5m`/`:cost_usd:sum_5m`/`:tokens:sum_5m` (over `langsmith_*`, summed across the
  high-card `session` label to per-env fleet totals). NB `portkey_api_*` are per-bucket gauges emitted only at
  the analytics/groups loop cadence (~11m), so they're STALE to an instant query between emits — use
  `last_over_time(...[20m])` to see them (this is why a quick instant lookup can wrongly read "metric absent").
- Dev/test context mapping: self-obs→`self-obs-stack`, product→`product-stack` (the telemetry split; different stacks).
- **Per-bucket gauge semantics (non-obvious):** `portkey_api_*` are gauges — aggregate with `sum_over_time`,
  **never `rate()`/`increase()`** (those are for counters; `genai_otel_bridge_source_graph_unavailable_total` and the
  `genai_otel_bridge_upstream_request_duration_seconds` histogram ARE counters, so `rate()`/`increase()` are correct there).
  Note the queue metric is `genai_otel_bridge_queue_depth_ratio` (the OTLP unit-1 `_ratio` suffix), not `_depth`. Full
  guide + GS2/GS3 staff actions: `grafana/README.md`.

## Grafana-staff-only prerequisites (stack-side)

These can't be set from this repo and are prod release prerequisites:
- **GS2** — raise Mimir `out_of_order_time_window` + `reject_old_samples_max_age` to match the downtime
  SLA (otherwise long-outage backfill is rejected).
- **GS3** — exempt the `genai_otel_bridge_*` namespace from Adaptive Metrics aggregation (else self-metrics get
  aggregated away).
