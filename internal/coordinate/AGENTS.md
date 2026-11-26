# internal/coordinate

Leader election, so exactly one replica is active. `lease/` runs on client-go leader election,
`dynamodb/` on a single DynamoDB lock item (ECS). `Noop` always leads (single replica and dev).

The lease epoch rides in `leaderCtx` (`WithEpoch` / `EpochFromContext`) so the checkpoint write fence
can read it without changing the frozen `onElected` signature. `Noop` stamps `max(1, Epoch)`.

`RequireIdentity` must be fatal to the caller when it returns non-nil. An empty identity under
`coordinator=lease` crash-loops in client-go, and under `dynamodb` an empty holder collides
self-telemetry across replicas, destroying the leader-overlap diagnostics.

## Migration trap: coordinator to `none` over a surviving checkpoint

Switching `ha.coordinator` to `none` while keeping a durable shared checkpoint store
(`configmap`/`dynamodb`) that a prior HA deployment advanced to epoch >= 2 permanently fences every
watermark write. The fenced `Save` returns `ErrStaleWrite`, which the runner classes as fenced: it
counts `checkpoint_fenced` and degrades the loop (reason `checkpoint fenced`), so the loop re-collects
on the 10-minute degraded backoff rather than at cadence. It is alertable (`checkpoint_fenced`,
`loop_degraded`, a climbing `window_lag_seconds`) but does not self-heal on its own; manual recovery
is deleting the checkpoint objects.

- `NoopWithEpoch(e)` stamps `max(1, e)`, so a baseline at or above the surviving stored epoch lets
  writes succeed. Single-replica has exactly one writer, so a higher epoch carries no cross-writer
  risk.
- The heal is wired in `app.BuildFromPlan`, not in `buildHA`: checkpoint keys are not known until the source
  graph is built, so `buildHA` only emits the loud startup warning, and `app.BuildFromPlan` then `Load`s each
  loop's key, takes the max stored epoch and re-constructs a `Noop` through `NoopWithEpoch`. A read
  error is non-fatal (the warning already flags the hazard) and a fresh store stays at epoch 1.
- Do not stamp a maximal sentinel epoch to dodge the fence. It re-creates the same trap in the
  `none` to `lease` direction, where a fresh lease's low epoch would be fenced forever.

## lease/

- The async `OnStartedLeading` barrier: client-go runs the callback in a goroutine it does not join,
  and `LeaderElector.Run()` can return before that work finishes. The code sets `elected` inside the
  callback, closes `leadDone` when it returns, and after `Run()` waits on `leadDone` whenever
  `elected.Load() || IsLeader()`, so re-election cannot race the old leader's drain. `barrier_test.go`
  guards it; preserve it.
- `ReleaseOnCancel = false` is deliberate: on SIGTERM the lease expires rather than being released,
  so a standby cannot acquire mid-drain. It does not mean the old leader keeps working through the
  grace window. `leaderCtx` is cancelled at once, in-flight collect/emit aborts, queued batches are
  dropped and the commit path refuses any post-cancel `Save`. Nothing new persists after cancel; the
  grace window only bounds how long SIGKILL takes to arrive.
- `epoch()` reads `Lease.Spec.LeaseTransitions` once at election as a coarse fence and returns the
  count plus one (`leaseEpoch`; nil counts as 0), so the first leader on a fresh Lease runs at
  epoch 1 like `Noop` and the DynamoDB coordinator's first fence. A raw count would put that leader
  at epoch 0, fenced by every watermark a single-replica deployment wrote (#375). The shift is upward
  only, so stores written by older releases (epoch == count) stay writable, mixed-version rollouts
  included.
- A failed GET is retried every `epochReadBackoff` until it succeeds or the leader context is done,
  with a rate-limited warning; the loops do not start until then (#376, which reversed the earlier
  "fall back to epoch 0" choice: a leader at a guessed epoch is fenced for its whole term while it
  blocks every standby). On ctx done it returns 0 and `onElected` runs under the cancelled ctx and
  exits. Caveat: client-go's renew fast path is an `Update` with no `Get`, so a Lease that can be
  updated but not read keeps this replica leading with no loops; `GenaiOtelBridgeLeaderAbsent` and
  the warning are the signal. The real in-flight fence is leaderCtx cancellation.
- Leadership loss re-campaigns in process. `Run` wraps one election term in a `for` loop and on a
  genuine renewal lapse builds a fresh elector, matching the `dynamodb/` acquire loop. It returns
  only when the root ctx is cancelled or the elector cannot be constructed. The drain barrier joins
  the prior term before the next campaign, and each re-election reads a fresh epoch fence and re-enters `onElected`, which
  `Scheduler.Run` handles by calling `Runner.Reset()`.

## dynamodb/

- One lock item, CAS acquire and a monotonic `fence` epoch. `acquire` takes an empty or expired item
  with a conditional `UpdateItem` and bumps `fence`; `renew` extends `expiresAtMs` only while
  `holder` and `fence` still match. The item is deliberately given no DynamoDB TTL: auto-deletion
  would reset `fence` below a surviving checkpoint's epoch after a long outage. Expiry is by the
  `expiresAtMs` comparison alone.
- `Run` retries acquire every `retry` period without returning, as long as the root ctx is alive.
- The renew watchdog measures `renew_deadline` from the START of the last successful acquire or
  renew request (#377), read just before that request's own clock read, so step-down always comes
  before the `expiresAtMs` (start + lease) that request wrote. Measuring from the response let a
  renew slower than `lease_duration - renew_deadline` keep leaderCtx alive after a standby had
  acquired. client-go also starts its RenewDeadline before the request. The watchdog uses the real
  monotonic clock; `setNow` only feeds `expiresAtMs`.
- Failover here is clock-sensitive and not equivalent to the Lease path. `acquire` compares the
  candidate's wall clock against an `expiresAtMs` written from the leader's wall clock, so absolute
  inter-node skew (not just drift) shifts failover timing, bounded by roughly
  `lease_duration - renew cadence`. It depends on NTP-synced hosts. See `ARCHITECTURE.md` ledger #17.

Permissions: the lease backend needs `coordination.k8s.io/leases` create plus get/update on the
named lease; the dynamodb backend needs `dynamodb:UpdateItem`/`GetItem` on the lock+checkpoint table
via IAM, not k8s RBAC.
