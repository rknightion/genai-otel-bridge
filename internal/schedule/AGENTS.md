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
  durable time is behind the attempt (a genuine fence, not a benign already-advanced write) it fires
  `checkpoint_fenced` and resyncs the in-memory frontier to durable, so `Since()` can never run ahead
  of a rejected write.
- Leadership-loss race. A `select` can dequeue a batch in the same iteration `leaderCtx` is
  cancelled, so both `Run()` and `ProcessBatch()` re-check `ctx.Err()` before emit and drop it.

## Behaviour

- Tick interval is cadence with 10% jitter, to de-align loops and replicas. On a terminal or degraded
  halt the scheduler backs off to 10 minutes, cleared on the next successful save. Five consecutive
  checkpoint-save failures mark the loop degraded.
- Snapshot loops (`LoopSpec.Window == 0`, for example langsmith sessions and portkey groups) gate out
  both catch-up acceleration and the `backfill_unstorable` count. A snapshot loop's watermark is a
  liveness heartbeat at `now`, not a replay frontier. Without the backfill gate, `MaxBackfill == 0`
  makes the floor `now` and produces a spurious skip count every tick.
- The queue is bounded (cap at least 1) and `Enqueue` blocks when full: backpressure, not drop.
- `ProcessBatch` calls `emit.CoalesceDPM` before `splitByBucket`, so the DPM cap applies to what is
  emitted, and suppressions increment
  `genai_otel_bridge_samples_capped_total{loop,reason="dpm"}`.
- `SetBeat` wires a per-tick attempt heartbeat, progress rather than success, into `/healthz`. The
  liveness threshold is deliberately the worst legitimate gap between beats, so a leader blocked in
  an intended retry or a degraded loop on its slow backoff is not killed.
