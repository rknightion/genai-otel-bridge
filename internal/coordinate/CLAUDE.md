# internal/coordinate — leader election (single active replica)

`coordinate.go` defines the seam + `Noop`; `lease/` implements it on k8s client-go leader-election.

```go
type Coordinator interface {
    Run(ctx context.Context, onElected func(leaderCtx context.Context)) error
}
func WithEpoch(ctx, epoch) context.Context   // threads lease epoch into leaderCtx
func EpochFromContext(ctx) int64
```

`onElected` runs only while leader; `leaderCtx` is cancelled on loss. The **lease epoch is passed via
`leaderCtx`** (so the checkpoint write fence reads it without changing the frozen `onElected`
signature). `Noop` always leads with epoch 1 (single-replica/dev).

## lease/ gotchas

- **The Lease reduces overlap; it is not the write fence.** Single-emit safety comes from the
  checkpoint fence + leaderCtx cancellation (see `internal/checkpoint`). Don't assume the Lease alone
  prevents double-emit.
- **Async-`OnStartedLeading` barrier (round3 HIGH-1, `ext-review-3`):** client-go runs `OnStartedLeading`
  in a goroutine it doesn't join, and `LeaderElector.Run()` can return before the callback's work
  finishes. The fix tracks `elected.Store(true)` inside the callback, closes `leadDone` when it
  finishes, and after `Run()` returns waits on `leadDone` (checking `elected.Load() || IsLeader()`) so
  re-election can't race the old leader's drain. Preserve this — `barrier_test.go` guards it.
- **`ReleaseOnCancel = false`:** on SIGTERM the lease is *not* released — it expires, letting the leader
  finish persisting watermarks and the standby take over only after a clean drain.
- `epoch()` reads `Lease.Spec.LeaseTransitions` once at election as a coarse monotonic fence (returns 0
  + warns on GET error; never guesses). The real in-flight fence is leaderCtx cancellation.

RBAC: `coordination.k8s.io/leases` get/create/update in the tool namespace.
