# internal/checkpoint — durable watermark store + write fence

`checkpoint.go` defines the seam and the fence; `file/` (dev), `configmap/` (k8s prod), and
`dynamodb/` (ECS deployment target) implement it.

```go
type Checkpointer interface {
    Load(ctx context.Context, key model.CheckpointKey) (model.Watermark, error)
    Save(ctx context.Context, key model.CheckpointKey, w model.Watermark) error
}
func CheckMonotonic(stored, incoming model.Watermark) error // accept iff epoch >= stored AND Time strictly advances
```

## The write fence is the load-bearing single-emit mechanism (Cdx-C14)

The Lease only *reduces* overlap — it is **not** a write fence. Double-emit is prevented by:
`CheckMonotonic` (rejects `incoming.Time ≤ stored.Time` or `incoming.Epoch < stored.Epoch`) +
scheduler re-checking `leaderCtx` before Emit/Save + leader-ctx cancellation aborting in-flight work.
A demoted leader cannot move the frontier backward or double-advance.

## Backends

- **`file/`** — local YAML, dev only. Atomic temp-then-rename write; all access mutex-guarded.
  `New(path, ignoreInvalid)`: `false` refuses to start on corrupt YAML, `true` logs + bootstraps empty
  (loud). Absent key → zero `Watermark{}` (not an error). **Discouraged with `coordinator=lease`** —
  it's per-pod, not shared (config validation forbids `file`+`lease`).
- **`configmap/`** — single k8s ConfigMap, one JSON-serialized watermark per data key (the prod
  default). RMW with optimistic concurrency: on a `resourceVersion` 409 it re-reads and retries (≤5);
  a concurrent writer's newer watermark then trips `CheckMonotonic`. Single-writer `mu` serializes the
  RMW loop (M1). Corrupt value → `Load` errors and `Save` refuses to overwrite (CP-C10, never clobber).
  Data keys are sanitized to `[-._a-zA-Z0-9]+` + a 12-char SHA256 suffix of the logical key (collision-
  free, stable across restarts). Total payload bounded < 900 KiB (headroom under the 1 MiB cap).
- **`dynamodb/`** — single DynamoDB item per checkpoint key (`pk = <keyPrefix>ckpt#<CheckpointKey>`),
  the ECS backend. `Save` mirrors the ConfigMap RMW exactly: `GetItem` → `CheckMonotonic` → conditional
  `PutItem` gated on a numeric `version` attribute (optimistic concurrency; retries ≤5 on a condition
  failure, same shape as the ConfigMap's `resourceVersion` 409 retry). `Watermark.Time` is stored as an
  RFC3339Nano string (round-trips a zero time). A present-but-non-numeric `version` is treated as item
  corruption (error); a missing `version` (hand-seeded/migrated item) defaults to 0 and the write
  upgrades it via `attribute_not_exists(version)` rather than spinning forever on `version = 0`.

RBAC: `configmaps` get/create/update in the tool namespace (for the configmap backend). The dynamodb
backend instead needs `dynamodb:GetItem`/`PutItem` on the lock+checkpoint table (IAM, not k8s RBAC) —
see `deploy/ecs/terraform/README.md`.

Tests: `wm(sec, epoch)` helper; fence cases (forward same-epoch, equal-time reject, backward reject,
higher-epoch accept, stale-epoch reject); persistence-across-reopen; corruption; injected-409 retry;
`datakey_test.go` validates `IsConfigMapKey` + no-collision + stability.
