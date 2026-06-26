# internal/schedule — tick→collect→enqueue→emit driver + watermark state machine

`scheduler.go` orchestrates per-loop ticks; `runner.go` (`LoopRunner`) owns the queue, the single-flight
emit worker, the in-memory frontier, and the epoch-fenced checkpoint commit. `metrics.go` is the
`Metrics` seam (implemented by `selfobs`).

## Critical invariants (each backed by a `*_test.go`)

- **Single-flight collection (CP-C1):** `Busy()` blocks re-collect while a batch is in flight; the
  scheduler skips the tick if busy. `Since()` returns the **in-memory saved frontier**, never a stale
  re-read of the checkpoint, so windows can't overlap.
- **Watermark advance (CP-C2/C7/C8):** `ProcessBatch` splits a batch into ascending per-bucket
  sub-batches and per bucket decides: advance-past (skip + count), terminal-halt (degrade, no advance,
  loud), or retryable-stop (commit only interior buckets that advanced, re-pull next tick). Quiet/empty
  windows still advance past `until` so `window_lag` doesn't inflate. `Cursor` is set only on the
  full-completion path; interior commits get empty cursor.
- **Epoch-fenced commit (round3-#3):** `commit()` re-checks `ctx.Err()` immediately **before** `Save()`
  — no advance after leadership loss even if the checkpointer ignores ctx. On `ErrStaleWrite` where
  durable time is *behind* the attempt (fenced), it fires `checkpoint_fenced` and **resyncs the
  in-memory frontier to durable** so `Since()` can't run ahead of a rejected write.
- **Leadership-loss race (CP-R3b):** a `select` can dequeue a batch in the same iteration `leaderCtx`
  is cancelled — both `Run()` and `ProcessBatch()` re-check `ctx.Err()` before emit and drop it.

## DPM cap

`ProcessBatch` calls `emit.CoalesceDPM` to coalesce to ≤`max_dpm` samples per series-minute before `splitByBucket`; suppressions increment `genai_otel_bridge_samples_capped_total{loop,reason="dpm"}`.

## Behaviour

- Tick interval = `Cadence ± 10%` jitter. On terminal/degraded halt the scheduler backs off to **10
  minutes** (no hammering); cleared on the next successful save.
- **Snapshot loops (`LoopSpec.Window == 0`, e.g. langsmith sessions, portkey groups):** the catch-up
  acceleration AND the `backfill_unstorable` count are both gated on `Window > 0` — a snapshot loop's
  watermark is a liveness heartbeat (`= now`), not a replay frontier, so neither applies (without the
  backfill gate, `MaxBackfill==0` ⇒ `floor==now` ⇒ a spurious skip count every tick).
- Queue is bounded (cap ≥ 1); `Enqueue` blocks on full (backpressure). 5 consecutive save failures →
  degraded.
- `SetBeat` wires a per-tick **attempt** heartbeat (progress, not success) into `/healthz`.

Tests of note: `runner_test.go` (single-flight order [M4], CP-R3b race), `fence_test.go` (epoch fencing),
`leadership_loss_test.go` (real file checkpointer + ctx-ignoring impl, asserts no durable advance).
