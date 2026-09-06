# internal/checkpoint

Durable watermark store plus the monotonic + epoch write fence. `file/` is dev, `configmap/` is the
k8s prod default, `dynamodb/` is the ECS backend.

## The write fence is the load-bearing single-emit mechanism

The Lease only reduces overlap; it is not a write fence. Double-emit is prevented by three things
together: `CheckMonotonic`, the scheduler re-checking `leaderCtx` immediately before Emit/Save, and
leader-ctx cancellation aborting in-flight work. A demoted leader can then neither move the frontier
backward nor double-advance it. Do not weaken one expecting the other two to compensate.

`CheckMonotonic` accepts an unchanged `Time` only when the `Cursor` changed. That relaxation exists
so the logs-export job state machine can persist in-flight progress across ticks at a non-advancing
`Time`; removing it wedges that loop. `Time` itself never regresses and the epoch fence still wins.

`CheckEncodable` rejects a `Time` outside year [1,9999] at the top of every backend `Save`. Both
durable encodings (ConfigMap JSON, DynamoDB RFC3339Nano) drop or garble such a value, which would
become permanent poison that later Load/Save refuse forever. A zero `Watermark` is year 1 and is
legitimate.

## Backends

- `file/` is per-pod, not shared, so config validation rejects `checkpoint=file` with
  `coordinator=lease`. `New(path, ignoreInvalid)`: `false` refuses to start on corrupt YAML, `true`
  logs loudly and bootstraps empty. An absent key is a zero `Watermark{}`, not an error.
- `configmap/` keeps one JSON watermark per data key in a single ConfigMap. RMW under optimistic
  concurrency: a `resourceVersion` 409 re-reads and retries (5 retries), and a concurrent writer's
  newer watermark then trips `CheckMonotonic`. A corrupt value makes `Load` error and `Save` refuse
  to overwrite, never clobber. Data keys are sanitised to `[-._a-zA-Z0-9]+` plus a 12-char SHA256
  suffix of the full logical key, so sanitisation cannot collide and the key is stable across
  restarts. Payload is bounded at 900 KiB for headroom under the 1 MiB API cap.
- `dynamodb/` stores one item per key at `pk = <keyPrefix>ckpt#<CheckpointKey>` and mirrors the
  ConfigMap RMW exactly, with a conditional `PutItem` gated on a numeric `version` attribute in place
  of the 409 retry. A present-but-non-numeric `version` is item corruption and errors. A missing
  `version` (hand-seeded or migrated item) defaults to 0, and the write upgrades it via
  `attribute_not_exists(version)` rather than spinning forever on `version = 0`.

Permissions: the configmap backend needs `configmaps` create plus get/update scoped to the
checkpoint ConfigMap (`deploy/helm/templates/rbac.yaml`). The dynamodb backend needs
`dynamodb:GetItem`/`PutItem` on the lock+checkpoint table via IAM, not k8s RBAC; read
`deploy/ecs/terraform/README.md` before changing that policy.
