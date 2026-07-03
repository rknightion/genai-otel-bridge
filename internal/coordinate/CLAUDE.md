# internal/coordinate — leader election (single active replica)

`coordinate.go` defines the seam + `Noop`; `lease/` implements it on k8s client-go leader-election;
`dynamodb/` implements it on a single DynamoDB lock item (ECS deployment target).

```go
type Coordinator interface {
    Run(ctx context.Context, onElected func(leaderCtx context.Context)) error
}
func WithEpoch(ctx, epoch) context.Context   // threads lease epoch into leaderCtx
func EpochFromContext(ctx) int64
```

`onElected` runs only while leader; `leaderCtx` is cancelled on loss. The **lease epoch is passed via
`leaderCtx`** (so the checkpoint write fence reads it without changing the frozen `onElected`
signature). `Noop` leads with epoch `max(1, Epoch)` (single-replica/dev; the zero value ⇒ 1).

## Migration trap: coordinator → `none` over a surviving checkpoint (#45)

Switching `ha.coordinator` to `none` while keeping a **durable, shared** checkpoint store
(`configmap`/`dynamodb`) that a prior HA (`lease`/`dynamodb`) deployment advanced to **epoch ≥ 2**
permanently fences every watermark write: `CheckMonotonic` rejects `incoming.Epoch < stored.Epoch`, the
fenced `Save` is benign (`ErrStaleWrite`, exempt from the degrade counter), so the loop never backs off —
it re-collects and re-emits the same window at full cadence forever. It is **alertable, not silent**
(`checkpoint_fenced` fires and `window_lag` climbs) but does **not self-heal**: recovery is manual —
delete the checkpoint objects. This is the single-replica analogue of the dynamodb-outage fence hazard
in `ARCHITECTURE.md` ledger #17.
- **Mechanism:** `coordinate.NoopWithEpoch(e)` stamps `max(1, e)`, so constructing the Noop with a
  baseline ≥ the surviving stored epoch lets writes succeed. Single-replica has exactly one writer, so a
  higher epoch carries no cross-writer risk. (`buildHA` cannot read the stored epoch — checkpoint keys
  aren't known until the source graph is built — so it currently WARNs loudly at startup instead of
  auto-adopting the stored epoch; full auto-heal would wire the max-stored-epoch read through `app`.)
- Do **not** stamp a maximal sentinel epoch to dodge the fence: it re-creates the trap in the
  `none → lease` direction (a fresh lease's low epoch would then be fenced forever).

## lease/ gotchas

- **The Lease reduces overlap; it is not the write fence.** Single-emit safety comes from the
  checkpoint fence + leaderCtx cancellation (see `internal/checkpoint`). Don't assume the Lease alone
  prevents double-emit.
- **Async-`OnStartedLeading` barrier (round3 HIGH-1, `ext-review-3`):** client-go runs `OnStartedLeading`
  in a goroutine it doesn't join, and `LeaderElector.Run()` can return before the callback's work
  finishes. The fix tracks `elected.Store(true)` inside the callback, closes `leadDone` when it
  finishes, and after `Run()` returns waits on `leadDone` (checking `elected.Load() || IsLeader()`) so
  re-election can't race the old leader's drain. Preserve this — `barrier_test.go` guards it.
- **`ReleaseOnCancel = false`:** on SIGTERM the lease is *not* released — it expires, so a standby
  doesn't acquire mid-drain. This does **not** mean the old leader keeps working during the grace
  window: `leaderCtx` is cancelled immediately, in-flight collect/emit is aborted via ctx, queued
  batches are dropped, and the checkpoint commit path refuses any post-cancel `Save` (see
  `internal/checkpoint` / `internal/schedule`). Nothing new persists after cancel — the grace window
  only bounds how long the SIGKILL takes to arrive, it does not let watermarks keep advancing.
- `epoch()` reads `Lease.Spec.LeaseTransitions` once at election as a coarse monotonic fence (returns 0
  + warns on GET error; never guesses). The real in-flight fence is leaderCtx cancellation.
- **Leadership loss re-campaigns in-process on BOTH backends (#110).** `lease.Coordinator.Run` wraps a
  single election term (`campaign`) in a `for` loop: on a genuine renewal lapse (leadership lost while
  the root ctx is still alive) it builds a **fresh** elector and re-campaigns, exactly like the
  `dynamodb/` coordinator's `acquire` loop — it only returns when the root ctx is cancelled
  (SIGTERM/rollout → `ctx.Err() != nil`, a clean exit) or when the elector cannot be constructed (a
  loud, non-nil error → `main` fatals). This removed the prior asymmetry where a lease lapse made
  `Run` return `ctx.Err() == nil`, fell through `main`'s `err != nil && ctx.Err() == nil` guard, and
  exited the process **0** mid-pod-life on every K8s-API flap. The drain barrier (`elected || IsLeader`
  → `<-leadDone`) still joins the prior term before the next campaign, so a re-election never races the
  old leadership's emit worker; each re-election reads a fresh epoch fence and re-enters `onElected`
  (`Scheduler.Run` resets via `Reset()`, the same re-entry the dynamodb coordinator already relies on).

## dynamodb/ gotchas

- **Single lock item, CAS acquire + monotonic `fence` epoch.** `acquire` takes an empty/expired item
  with a conditional `UpdateItem` and bumps `fence` (the `LeaseTransitions` analogue); `renew` extends
  `expiresAtMs` only while `holder`+`fence` still match. The item is never given a DynamoDB TTL — expiry
  is by the `expiresAtMs` comparison only, so a long full outage can't reset `fence` back below a
  surviving checkpoint's epoch.
- **In-process re-acquire loop (unlike `lease/`):** `Run` is a `for` loop — on leadership loss (or a
  failed acquire) it retries every `retry` period without returning, as long as the root ctx is alive.
  It only returns (with `ctx.Err()`) when the context is cancelled.
- **Clock-sensitive failover, NOT identical to the K8s Lease path.** `acquire` compares the CANDIDATE's
  wall clock against an `expiresAtMs` written from the LEADER's wall clock — absolute inter-node clock
  skew (not just drift) affects failover timing, bounded by roughly `lease_duration - renew cadence`.
  Depends on NTP-synced hosts. See `ARCHITECTURE.md` decision ledger #17.

RBAC: `coordination.k8s.io/leases` get/create/update in the tool namespace (lease backend). The
dynamodb backend instead needs `dynamodb:UpdateItem`/`GetItem` on the lock+checkpoint table (IAM, not
k8s RBAC).
