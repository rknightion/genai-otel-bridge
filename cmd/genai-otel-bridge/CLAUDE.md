# cmd/genai-otel-bridge — the binary

`main.go`: parse flags → set memory limit → load config → wire real OTLP/k8s/selfobs → run under
coordinator → handle SIGTERM. No logic beyond wiring (that's `internal/app`).

## Flags

- `-config` (default `/etc/genai-otel-bridge/config.yaml`), `-health-addr` (default `:8080`),
  `-namespace` (`$POD_NAMESPACE`), `-identity` (`$POD_NAME`, lease/lock identity — on ECS, falls back to
  the Task ARN read from `$ECS_CONTAINER_METADATA_URI_V4/task` when unset; see `ecs.go`),
  `-container-mem-bytes` (numerator for `GOMEMLIMIT`),
  `-checkpoint-file` (default `/var/lib/genai-otel-bridge/checkpoints.yaml`; path for the `ha.checkpoint=file`
  store — override for local runs, e.g. `./checkpoints.local.yaml`).
- `-healthcheck`: **probe path, not a normal run** (ECS container health check — distroless has no shell
  for curl). GETs `/healthz` on the `-health-addr` port via 127.0.0.1 (`localHealthURL` rewrites a
  `0.0.0.0`/`[::]` bind — and accepts a bare port — to a dialable `127.0.0.1:<port>`) and exits 0/1.
  Branches with the other early exits, before any config/wiring. ECS task def:
  `["CMD","/genai-otel-bridge","-healthcheck"]`.
- `-validate-config`: **validation path, not a normal run.** Loads + schema/semantic-checks the
  `-config` file via `app.ValidateConfigFile` (placeholders unset `${ENV}` refs so no secrets are
  needed — endpoints/URLs get an https placeholder), prints `validate-config: OK/FAIL`, exits 0/1.
  Branches before any wiring. For pre-deploy / CI overlay validation (e.g. an external overlay repo
  validating against the published image).
- `-version`: **print path, not a normal run.** Prints the ldflags-stamped `version.String()` and
  exits 0. Branches before any wiring.
- `-cleanup` (+ `-cleanup-retain-checkpoint`): **uninstall path, not a normal run.** Builds an
  in-cluster client, deletes the app-created `genai-otel-bridge-leader` Lease and (unless retain) the
  `genai-otel-bridge-checkpoints` ConfigMap via `internal/cleanup.Run`, then exits. The chart's `post-delete`
  hook invokes it so `helm uninstall` leaves no orphans. Branches **before** any config/emit/selfobs
  wiring — it needs only `-namespace`. Idempotent (NotFound = success).

## Shared HA-object names

`leaseName` (`genai-otel-bridge-leader`) and `checkpointCMName` (`genai-otel-bridge-checkpoints`) are package consts — the
single source of truth shared by `buildHA` (creates them) and `runCleanup` (deletes them), and they
must match the chart RBAC `resourceNames`. Names are fixed (single-instance chart).

## Lifecycle gotchas

- `signal.NotifyContext(SIGTERM, SIGINT)` cancels the root ctx → `app.Run` returns → graceful shutdown.
  If `app.Run` returns a **non-nil** error while ctx is **not** cancelled → fatal log + `os.Exit(1)`.
  **Leadership-loss handling ([#110]):** BOTH coordinators now re-campaign in-process on a genuine
  renewal lapse (leadership lost while the root ctx is still alive) rather than returning — the `lease`
  coordinator builds a fresh elector and re-enters its election loop (symmetric with the `dynamodb`
  coordinator's acquire loop). So `app.Run` returns only when the root ctx is cancelled (SIGTERM/rollout
  → clean exit **0**) or on a genuine non-nil error such as elector/backend construction failure (→ the
  guard above fires, `os.Exit(1)`). A transient K8s-API/DynamoDB flap no longer exits the process
  mid-pod-life; it just re-campaigns and a standby may take over for the gap.
- `selfobs.SetMemoryLimit(0.9, *memLimit)` runs **before** config load (90% of the container limit;
  no-op if ≤ 0).
- **Self-observability identity:** falls back to the telemetry endpoint if `cfg.Emit.Self` is nil;
  appends `-meta` to the service namespace; instance = POD_NAME (per-replica, for leader-overlap diag).
- **Self-profiling (opt-in/default-off):** `selfobs.StartProfiling` is wired right after the
  self-metrics provider and **before** the coordinator — so it runs on leader AND standby (process-
  level). `defer profStop`. Start failure is fatal (never run silently un-profiled). Same `-meta`
  identity as self-metrics.
- **Liveness threshold (CP-C5):** `max(schedule.DegradedBackoff[=10m], slowest enabled cadence) +
  emitRetryBudget[2m] + livenessMargin[4m]` — derived from the real constants (DegradedBackoff is
  exported), not a coincidental literal; same numeric result as before. So a leader in retry/backpressure
  isn't killed, but a wedged scheduler is. (bucket_settle drives the window_lag alert, not this beat.)
- **Replica double-emit guard:** the chart fails to render on `ha.coordinator=none` + `replicas>1`
  (all-leader double-emit); the binary re-checks via the `GENAI_OTEL_BRIDGE_REPLICAS` env (defence-in-depth).
- **HA wiring (`buildHA`):** the ONLY HA-backend-aware code. An in-cluster k8s client is built only if
  `coordinator=lease` **or** `checkpoint=configmap`; a DynamoDB client (SDK default credential chain →
  ECS task role; `ha.dynamodb.endpoint` overrides `BaseEndpoint` for dynamodb-local/VPC endpoints) only
  if `coordinator=dynamodb` **or** `checkpoint=dynamodb`. checkpoint: `configmap` → ConfigMap
  `genai-otel-bridge-checkpoints`; `file` → `/var/lib/genai-otel-bridge/checkpoints.yaml`; `dynamodb` →
  table `ha.dynamodb.table`, pk prefix `<key_prefix>ckpt#`. coordinator: `lease` → Lease
  `genai-otel-bridge-leader` (15s/10s/2s); `dynamodb` → lock item `<key_prefix>lock#<lock_name>`
  (durations from `ha.dynamodb.*`, default 15s/10s/2s); `none` → `coordinate.Noop`. Construction is lazy
  (no DynamoDB call here) so it's safe before `cfg.Validate` runs inside `app.Build`.

Version is stamped into `internal/version.Version` via `make build` ldflags
(`git describe --tags --always --dirty`, default `dev`). [#91] `main.go` **imports** `internal/version`
(the `-version` flag + the self-obs resource wiring), which is what makes the `-X .../version.Version=…`
stamp actually LINK into the binary — without a real import the linker drops it and it stays `dev`. The
stamped version is observable three ways: the `-version` flag, the `version=` field on the startup
`config loaded` log line, and the `service.version` resource attribute on both self-obs planes
(metrics + traces).
