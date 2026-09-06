# cmd/genai-otel-bridge - the binary

Wiring only: parse flags, set the memory limit, load config, build real OTLP/k8s/selfobs
dependencies, run under the coordinator, handle SIGTERM. Logic belongs in `internal/app`.

## Flag defaults that are not guessable

`-config` `/etc/genai-otel-bridge/config.yaml`, `-health-addr` `:8080`, `-namespace`
`$POD_NAMESPACE`, `-identity` `$POD_NAME`, `-checkpoint-file`
`/var/lib/genai-otel-bridge/checkpoints.yaml` (used only by `ha.checkpoint=file`; override for local
runs), `-container-mem-bytes` (numerator for `GOMEMLIMIT`).

On ECS, `-identity` falls back to the Task ARN read from `$ECS_CONTAINER_METADATA_URI_V4/task`
(`ecs.go`).

## Four flags are alternate entry points, not modes of a normal run

Each branches **before** any config or wiring, so none of them needs credentials or a reachable
backend. Keep that ordering when adding to `main`.

- `-healthcheck` - ECS container health check; distroless has no shell for curl. GETs `/healthz`
  over 127.0.0.1, exits 0/1. `localHealthURL` rewrites a `0.0.0.0` / `[::]` bind, and accepts a bare
  port, into a dialable `127.0.0.1:<port>`. Task def: `["CMD","/genai-otel-bridge","-healthcheck"]`.
- `-validate-config` - loads and schema/semantic-checks `-config` via `app.ValidateConfigFile`,
  which placeholders unset `${ENV}` refs (endpoints get an https placeholder) so no secrets are
  needed. Prints `validate-config: OK/FAIL`. For pre-deploy or external-overlay validation.
- `-version` - prints the ldflags-stamped `version.String()`.
- `-cleanup` (with `-cleanup-retain-checkpoint`) - the chart's `post-delete` uninstall hook. Deletes
  the app-created Lease and, unless retained, the checkpoint ConfigMap via `internal/cleanup.Run`.
  Needs only `-namespace`. Idempotent (NotFound counts as success).

## Shared HA-object names

`leaseName` (`genai-otel-bridge-leader`) and `checkpointCMName`
(`genai-otel-bridge-checkpoints`) are package consts shared by `buildHA` (creates them) and
`runCleanup` (deletes them). **They must match the chart RBAC `resourceNames`** or the pod loses
access to its own lease or checkpoint. Fixed names; the chart is single-instance.

## Lifecycle

- **`buildHA` is the only HA-backend-aware code.** An in-cluster k8s client is built only for
  `coordinator=lease` or `checkpoint=configmap`; a DynamoDB client (SDK default credential chain, so
  the ECS task role; `ha.dynamodb.endpoint` overrides `BaseEndpoint` for dynamodb-local or a VPC
  endpoint) only for `coordinator=dynamodb` or `checkpoint=dynamodb`. checkpoint: `configmap` to the
  ConfigMap, `file` to `-checkpoint-file`, `dynamodb` to table `ha.dynamodb.table` with pk prefix
  `<key_prefix>ckpt#`. coordinator: `lease` to the Lease (15s/10s/2s), `dynamodb` to lock item
  `<key_prefix>lock#<lock_name>`, `none` to `coordinate.Noop`. Construction is lazy (no DynamoDB call
  here), which is what makes it safe to run before `cfg.Validate` inside `app.Build`.
- **A leadership lapse re-campaigns in-process, it does not exit.** Both coordinators rebuild and
  re-enter their acquire loop when leadership is lost while the root ctx is still alive, so a
  transient kube-apiserver or DynamoDB flap does not kill the process mid-pod-life; a standby may
  take over for the gap. `app.Run` therefore returns only on root-ctx cancellation (SIGTERM or
  rollout, clean exit 0) or a genuine construction error, which is fatal plus `os.Exit(1)`.
- `selfobs.SetMemoryLimit(0.9, *memLimit)` runs **before** config load. No-op when the limit is <= 0.
- **Self-observability identity** falls back to the telemetry endpoint when `cfg.Emit.Self` is nil,
  appends `-meta` to the service namespace, and uses POD_NAME as the instance so leader overlap is
  diagnosable per replica.
- **Self-profiling is opt-in and default-off.** `selfobs.StartProfiling` is wired after the
  self-metrics provider and **before** the coordinator, so it runs on the standby too. A start
  failure is fatal: never run silently un-profiled.
- **Liveness threshold** is derived, not a literal:
  `max(schedule.DegradedBackoff, slowest enabled cadence) + emitRetryBudget(2m) + livenessMargin(4m)`.
  A leader in retry or backpressure survives; a wedged scheduler does not. `bucket_settle` drives the
  `window_lag` alert, not this beat.
- **Replica double-emit guard.** The chart refuses to render `ha.coordinator=none` with
  `replicas>1`; the binary re-checks the same thing via `GENAI_OTEL_BRIDGE_REPLICAS` as
  defence-in-depth.
- **`main.go` must keep importing `internal/version`.** Without a real import the linker drops the
  package and the `-X .../version.Version=` ldflag silently leaves the version at `dev`. The stamp is
  observable three ways: the `-version` flag, the `version=` field on the `config loaded` startup log
  line, and the `service.version` resource attribute on both self-obs planes.
