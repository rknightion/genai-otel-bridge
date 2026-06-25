# cmd/decant — the binary

`main.go`: parse flags → set memory limit → load config → wire real OTLP/k8s/selfobs → run under
coordinator → handle SIGTERM. No logic beyond wiring (that's `internal/app`).

## Flags

- `-config` (default `/etc/decant/config.yaml`), `-health-addr` (default `:8080`),
  `-namespace` (`$POD_NAMESPACE`), `-identity` (`$POD_NAME`, lease identity),
  `-container-mem-bytes` (numerator for `GOMEMLIMIT`),
  `-checkpoint-file` (default `/var/lib/decant/checkpoints.yaml`; path for the `ha.checkpoint=file`
  store — override for local runs, e.g. `./checkpoints.local.yaml`).
- `-validate-config`: **validation path, not a normal run.** Loads + schema/semantic-checks the
  `-config` file via `app.ValidateConfigFile` (placeholders unset `${ENV}` refs so no secrets are
  needed — endpoints/URLs get an https placeholder), prints `validate-config: OK/FAIL`, exits 0/1.
  Branches before any wiring. For pre-deploy / CI overlay validation (e.g. an external overlay repo
  validating against the published image).
- `-cleanup` (+ `-cleanup-retain-checkpoint`): **uninstall path, not a normal run.** Builds an
  in-cluster client, deletes the app-created `decant-leader` Lease and (unless retain) the
  `decant-checkpoints` ConfigMap via `internal/cleanup.Run`, then exits. The chart's `post-delete`
  hook invokes it so `helm uninstall` leaves no orphans. Branches **before** any config/emit/selfobs
  wiring — it needs only `-namespace`. Idempotent (NotFound = success).

## Shared HA-object names

`leaseName` (`decant-leader`) and `checkpointCMName` (`decant-checkpoints`) are package consts — the
single source of truth shared by `buildHA` (creates them) and `runCleanup` (deletes them), and they
must match the chart RBAC `resourceNames`. Names are fixed (single-instance chart).

## Lifecycle gotchas

- `signal.NotifyContext(SIGTERM, SIGINT)` cancels the root ctx → `app.Run` returns → graceful shutdown.
  If `app.Run` returns while ctx is **not** cancelled → fatal log + `os.Exit(1)`.
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
  (all-leader double-emit); the binary re-checks via the `DECANT_REPLICAS` env (defence-in-depth).
- **HA wiring (`buildHA`):** an in-cluster k8s client is created only if `coordinator=lease` **or**
  `checkpoint=configmap`. checkpoint: `configmap` → ConfigMap `decant-checkpoints`; `file` →
  `/var/lib/decant/checkpoints.yaml`. coordinator: `lease` → Lease `decant-leader` (15s/10s/2s);
  `none` → `coordinate.Noop`.

Version is stamped into `internal/version.Version` via `make build` ldflags
(`git describe --tags --always --dirty`, default `dev`).
