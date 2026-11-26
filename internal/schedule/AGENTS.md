# internal/schedule

The tick, collect, enqueue, emit driver plus the watermark state machine. `scheduler.go` orchestrates
per-loop ticks; `LoopRunner` in `runner.go` owns the queue, the single-flight emit worker, the
in-memory frontier and the epoch-fenced commit. `metrics.go` is the `Metrics` seam that `selfobs`
implements.

## Invariants, each backed by a test

- Single-flight collection. `Busy()` blocks re-collection while a batch is in flight and the
  scheduler skips the tick. `Since()` returns the in-memory saved frontier, never a re-read of the
  checkpoint, so two windows cannot overlap.
- Watermark advance. `ProcessBatch` splits a batch into ascending per-bucket sub-batches and decides
  per bucket: advance past with a counted skip, terminal halt (degrade, no advance, loud), or
  retryable stop (commit only the interior buckets that advanced and re-pull next tick). Quiet and
  empty windows still advance past `until` so `window_lag_seconds` does not inflate. `Cursor` is set
  only on the full-completion path; interior commits carry an empty cursor.
- Epoch-fenced commit. `commit()` re-checks `ctx.Err()` immediately before `Save()`, so nothing
  advances after leadership loss even if the checkpointer ignores ctx. On `ErrStaleWrite` where the
  write would have made progress (durable time behind the attempt, or a same-time step to a new
  non-empty cursor under a higher durable epoch: a genuine fence, not a benign already-advanced write)
  it fires `checkpoint_fenced` and degrades the loop with reason `checkpoint fenced`, so the
  scheduler backs off instead of re-emitting the same window at cadence. Either way it resyncs the
  in-memory frontier to durable, so `Since()` can never run ahead of a rejected write. With self tracing enabled, one `loop.commit` span covers the common
  `Checkpointer.Save` path and records the fixed `outcome` set `committed`, `fenced`, `stale`, or
  `error`; the span never changes the live leadership context or commit semantics.
- Leadership-loss race. A `select` can dequeue a batch in the same iteration `leaderCtx` is
  cancelled, so both `Run()` and `ProcessBatch()` re-check `ctx.Err()` before emit and drop it.
- Cancellation is not failure. An emit, save, load or collect that fails after the leader context is
  cancelled (shutdown, lease loss) is not counted in `emit_errors_total`, not logged as a failure, does
  not step `saveFails` and does not degrade; a fence seen after cancellation is the hand-over to the
  next leader. The same errors under a live context still count.

## Behaviour

- Consecutive `source.ErrQuotaExceeded` ticks stretch the wait: cadence doubled per quota tick,
  jittered, capped at `DegradedBackoff` and never below cadence. One successful `Collect` resets it;
  a non-quota `Collect` error holds the streak. Auth (401/403) failures have no sentinel and still
  retry at cadence. Degraded keeps precedence, and the quota tick still counts
  `samples_skipped_total{reason="quota_exceeded"}` and advances nothing.
- Tick interval is cadence with 10% jitter, to de-align loops and replicas. On a terminal or degraded
  halt the scheduler backs off to 10 minutes, cleared on the next successful save. Five consecutive
  checkpoint-save failures mark the loop degraded, and so does one epoch-fenced write. When leadership
  ends, `Scheduler.Run` zeroes the `loop_degraded` gauge of each degraded loop after its goroutines
  join; `Reset` at the next election still clears the state.
- Snapshot loops (`LoopSpec.Window == 0`, for example langsmith sessions and portkey groups) gate out
  both catch-up acceleration and the `backfill_unstorable` count. A snapshot loop's watermark is a
  liveness heartbeat at `now`, not a replay frontier. Without the backfill gate, `MaxBackfill == 0`
  makes the floor `now` and produces a spurious skip count every tick.
- The queue is bounded (cap at least 1) and `Enqueue` blocks when full: backpressure, not drop.
- `SetBeat` wires a per-tick attempt heartbeat, progress rather than success, into `/healthz`. The
  liveness threshold is deliberately the worst legitimate gap between beats, so a leader blocked in
  an intended retry or a degraded loop on its slow backoff is not killed.
