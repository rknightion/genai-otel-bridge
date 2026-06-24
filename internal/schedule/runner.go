// SPDX-License-Identifier: AGPL-3.0-only

package schedule

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/grafana-ps/aip-oi/internal/checkpoint"
	"github.com/grafana-ps/aip-oi/internal/coordinate"
	"github.com/grafana-ps/aip-oi/internal/emit"
	"github.com/grafana-ps/aip-oi/internal/logging"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// [CP-C9] consecutive checkpoint-save failures before the loop enters degraded mode.
const checkpointFailThreshold = 5

// LoopRunner owns one loop's bounded queue and its single emit worker (single-flight EMIT) plus a
// single-flight gate over COLLECTION ([CP-C1]) so the scheduler can never re-collect a window
// whose batch has not yet been emitted+saved (which would re-read a stale checkpoint and enqueue a
// duplicate). The last SAVED frontier is held in memory and is the source of truth for the next
// `since`, so on an emit/save failure the next collect re-pulls from the true saved position.
type LoopRunner struct {
	loop   source.Loop
	em     emit.Emitter
	cp     checkpoint.Checkpointer
	guard  *source.Guard
	q      chan model.Batch
	m      Metrics
	name   string
	maxDPM int // hard ≤ N points per (series, minute); from governance.max_dpm (default 1)
	// lim throttles the runner's warn lines (retryable-exhausted emit, checkpoint-save fail, skip-with-gap)
	// to ≤1/min per key so a stuck loop doesn't spam stdout; the metric counters carry the true rate. Keys
	// are "retry:<loop>" / "save:<loop>" / "skip:<loop>:<reason>" — disjoint from the scheduler's keyspace.
	lim *logging.Limiter

	mu        sync.Mutex
	busy      bool            // a batch is collected-but-not-yet-saved
	frontier  model.Watermark // last SAVED frontier (in-memory source of truth)
	hasFront  bool
	saveFails int  // consecutive checkpoint-save failures
	degraded  bool // terminal/degraded → scheduler backs off + alerts
	firstOK   bool // a first successful watermark commit has been logged this leadership (info, once)
}

func NewLoopRunner(loop source.Loop, em emit.Emitter, cp checkpoint.Checkpointer, guard *source.Guard, queueDepth int, maxDPM int, m Metrics) *LoopRunner {
	if queueDepth < 1 {
		queueDepth = 1
	}
	if maxDPM < 1 {
		maxDPM = 1
	}
	return &LoopRunner{loop: loop, em: em, cp: cp, guard: guard, q: make(chan model.Batch, queueDepth), m: m, name: loop.Key().Loop, maxDPM: maxDPM, lim: logging.NewLimiter(time.Minute)}
}

// Busy reports a collected-but-unsaved batch is in flight — the scheduler skips the tick. [CP-C1]
func (r *LoopRunner) Busy() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.busy }

// Degraded reports a terminal/degraded loop — the scheduler backs off (does not hammer). [CP-C8]
func (r *LoopRunner) Degraded() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.degraded }

// Since returns the lower bound for the next Collect: the in-memory saved frontier, or (on first
// poll / after a new leader) the persisted checkpoint. [CP-C1] — never a stale value behind an
// in-flight batch, because Busy() gates re-collection.
func (r *LoopRunner) Since(ctx context.Context) (model.Watermark, error) {
	r.mu.Lock()
	if r.hasFront {
		w := r.frontier
		r.mu.Unlock()
		return w, nil
	}
	r.mu.Unlock()
	return r.cp.Load(ctx, r.loop.Key())
}

// Reset clears per-leadership state on (re-)election [CP-R3]. Without it, a batch collected under a
// PRIOR leadership could linger in the queue and then be emitted under the NEW live leaderCtx —
// stale data, after an intervening leader already advanced the checkpoint. Reset (a) DRAINS the
// queue so no pre-election batch is processed, and (b) clears the in-memory frontier so the next
// Since() re-Loads the DURABLE checkpoint (which reflects the intervening leader's advance) rather
// than trusting a stale in-memory value. Called by Scheduler.Run BEFORE starting this leadership's
// goroutines, after the prior leadership's goroutines have exited (Scheduler.Run wg.Wait).
func (r *LoopRunner) Reset() {
	for drained := false; !drained; {
		select {
		case <-r.q:
		default:
			drained = true
		}
	}
	r.mu.Lock()
	r.busy, r.hasFront, r.frontier = false, false, model.Watermark{}
	r.saveFails, r.degraded = 0, false
	r.firstOK = false // a new leadership re-announces its first successful commit (liveness signal)
	r.mu.Unlock()
}

// Enqueue marks the loop busy and hands the batch to the single worker (block-on-full backpressure).
func (r *LoopRunner) Enqueue(ctx context.Context, b model.Batch) error {
	r.mu.Lock()
	r.busy = true
	r.mu.Unlock()
	select {
	case r.q <- b:
		r.m.QueueDepth(r.name, len(r.q))
		return nil
	case <-ctx.Done():
		r.mu.Lock()
		r.busy = false
		r.mu.Unlock()
		return ctx.Err()
	}
}

// Run drains the queue with a single worker until leaderCtx is cancelled.
func (r *LoopRunner) Run(leaderCtx context.Context) {
	for {
		select {
		case <-leaderCtx.Done():
			return
		case b := <-r.q:
			// [CP-R3b] Go `select` picks RANDOMLY among ready cases, so a queued batch can be
			// dequeued in the SAME iteration leaderCtx is already done. Re-check before processing
			// so a stale batch is never emitted under a lost leadership.
			if leaderCtx.Err() != nil {
				return
			}
			r.m.QueueDepth(r.name, len(r.q))
			r.ProcessBatch(leaderCtx, b)
		}
	}
}

// ProcessBatch splits the ORIGINAL batch into per-bucket sub-batches (ascending) and applies the
// advance/skip decision per bucket. The watermark only moves forward and stays a contiguous prefix.
//   - [AR-C3] split pre-guard so a fully guard-dropped bucket still advances (counted), no stall.
//   - [CP-C2] on FULL completion the whole polled window up to `until` (= b.Watermark.Time) is
//     handled — including confirmed-empty/omitted-zero buckets — so the frontier advances to `until`
//     even when zero samples were emitted (a quiet window must NOT inflate window_lag forever).
//   - [AR-C2] commit ONCE per batch, not per bucket.
//   - [CP-C7/C8] a terminal-halt reject (bad-encoding OR unknown request-level 400) degrades the loop
//     (no advance, alert, scheduler backs off) instead of advancing-past (silent loss) or hammering.
func (r *LoopRunner) ProcessBatch(leaderCtx context.Context, b model.Batch) {
	defer func() { r.mu.Lock(); r.busy = false; r.mu.Unlock() }()
	// [CP-R3b] Never emit under a cancelled leadership — belt-and-suspenders for the Run select-race
	// above and for any direct caller. (Emit takes leaderCtx and would fail anyway, but we must not
	// even begin processing a stale batch.)
	if leaderCtx.Err() != nil {
		return
	}
	// [logs] Emit any log records (the logs-export loop's per-tick chunk) BEFORE the metric path. A
	// terminal/retryable reject aborts WITHOUT advancing (degrade / re-pull) — so the cursor isn't
	// committed and the loop re-does the step idempotently. On success/skip-with-gap, fall through to the
	// shared commit below, which persists Watermark.Time + Cursor (cursor advances even mid-window). The
	// metric path is a no-op for a logs-only batch (no Samples ⇒ no buckets). CoalesceDPM is samples-only
	// (logs go to Loki, not the 1DPM-capped metric plane), so it is not applied to Logs.
	if len(b.Logs) > 0 && !r.processLogs(leaderCtx, b) {
		return
	}
	// [followup §0] Hard DPM cap: coalesce to ≤ maxDPM points per (series, 60s minute) BEFORE the
	// per-bucket split, so a sub-minute/grouped source can't fan multiple same-minute points into
	// separate accepted emits (>1DPM). No-op on the 1-min Portkey shape. Suppressions are counted.
	var capped int
	b.Samples, capped = emit.CoalesceDPM(b.Samples, r.maxDPM)
	if capped > 0 {
		r.m.SamplesCapped(r.name, capped)
	}
	epoch := coordinate.EpochFromContext(leaderCtx)
	var frontier time.Time
	advanced := false
	for _, bucket := range splitByBucket(b) {
		bucketTime := bucket.Samples[0].Timestamp
		sub, dropped := r.guard.Sanitize(bucket)
		if dropped > 0 {
			r.m.GuardDropped(r.name, dropped)
		}
		if len(sub.Samples) == 0 {
			frontier, advanced = bucketTime, true // [AR-C3] whole bucket guard-dropped → advance past (counted)
			continue
		}
		err := r.em.Emit(leaderCtx, sub)
		switch {
		case err == nil:
			frontier, advanced = bucketTime, true
			r.m.EmittedSamples(r.name, len(sub.Samples))
			r.m.LastSuccess(r.name, bucketTime)
		case isAdvancePast(err): // duplicate-timestamp / too-old / 413: known sample-reject → skip-with-gap
			r.m.SamplesSkipped(r.name, rejectReason(err), len(sub.Samples))
			if r.lim.Allow("skip:" + r.name + ":" + rejectReason(err)) {
				slog.Warn("samples skipped (advancing past, counted gap)", "loop", r.name, "reason", rejectReason(err), "count", len(sub.Samples))
			}
			frontier, advanced = bucketTime, true
		case isTerminalHalt(err): // [CP-C7/C8] bad-encoding OR unknown request-level 400 → degrade, no advance
			r.m.EmitError(r.name, rejectReason(err))
			r.enterDegraded("terminal emit reject")
			if advanced {
				// [round3-#2] partial advance to an INTERIOR bucket (`frontier`), NOT the window end —
				// so it must carry an EMPTY cursor, not b.Watermark.Cursor (the window-END resume token).
				// Inert for the cursorless analytics loop; correctness for any future cursor-based source.
				r.commit(leaderCtx, b.Key, frontier, "", epoch)
			}
			return
		default: // RetryableError (5xx/429/exhausted): stop, no advance; re-pull next tick.
			r.m.EmitError(r.name, "retryable_exhausted")
			if r.lim.Allow("retry:" + r.name) {
				slog.Warn("emit failed after retries; will re-pull next tick", "loop", r.name)
			}
			if advanced {
				r.commit(leaderCtx, b.Key, frontier, "", epoch) // [round3-#2] interior bucket → empty cursor
			}
			return
		}
	}
	// [CP-C2] full completion ⇒ advance to the polled settled cutoff `until`, covering empty tails.
	// [logs] The Cursor!="" arm persists a cursor-ONLY step (no Time advance) at the zero Time — the
	// logs-export FIRST window runs at Time==zero (nothing completed yet), so stepIdle's "job created,
	// not yet started" cursor would otherwise never persist and the machine would loop on window 1
	// forever. The same-Time/cursor-change relaxation in commit()+CheckMonotonic makes the write stick.
	if target := b.Watermark.Time; advanced || !target.IsZero() || b.Watermark.Cursor != "" {
		r.commit(leaderCtx, b.Key, target, b.Watermark.Cursor, epoch)
	}
}

// processLogs sanitises + emits a logs chunk (the logs-export loop emits Logs, never Samples). Returns
// true to PROCEED to the shared commit (a clean emit, an all-dropped chunk, or a known skip-with-gap
// reject), false to ABORT without advancing (a terminal-halt degrades the loop; a retryable failure
// re-pulls) — so the cursor is not persisted and the loop re-does the step idempotently next tick.
func (r *LoopRunner) processLogs(leaderCtx context.Context, b model.Batch) bool {
	kept, dropped := r.guard.SanitizeLogs(r.name, b.Logs)
	if dropped > 0 {
		r.m.GuardDropped(r.name, dropped)
	}
	if len(kept) == 0 {
		return true // nothing to emit (all guard-dropped / empty) — still commit the cursor/time
	}
	err := r.em.Emit(leaderCtx, model.Batch{Key: b.Key, Logs: kept})
	switch {
	case err == nil:
		r.m.EmittedLogs(r.name, len(kept))
		// [self-obs] Stamp last_success at the committed window-end (the same Time window_lag is measured
		// against), so the logs loops (logs_export, runs) feed scrape_healthy and the poller-stale/
		// leader-absent alerts — the metrics path does the equivalent at its per-bucket Time. Guarded on
		// non-zero Time (mirrors scheduler.go WindowLag): the first cursor-only window has no completed
		// Time and must not stamp an epoch-0 timestamp that would read as permanently stale.
		if !b.Watermark.Time.IsZero() {
			r.m.LastSuccess(r.name, b.Watermark.Time)
		}
		return true
	case isAdvancePast(err): // known per-record reject → skip-with-gap, advance
		r.m.SamplesSkipped(r.name, rejectReason(err), len(kept))
		if r.lim.Allow("skip:" + r.name + ":" + rejectReason(err)) {
			slog.Warn("log records skipped (advancing past, counted gap)", "loop", r.name, "reason", rejectReason(err), "count", len(kept))
		}
		return true
	case isTerminalHalt(err): // [CP-C7/C8] bad-encoding / unknown 4xx → degrade, no advance
		r.m.EmitError(r.name, rejectReason(err))
		r.enterDegraded("terminal emit reject (logs)")
		return false
	default: // retryable exhausted → no advance, re-pull (idempotent: cursor not committed)
		r.m.EmitError(r.name, "retryable_exhausted")
		if r.lim.Allow("retry:" + r.name) {
			slog.Warn("emit failed after retries; will re-pull next tick (logs)", "loop", r.name)
		}
		return false
	}
}

// commit persists the watermark once (monotonic + epoch-fenced) and updates the in-memory frontier
// (the next `since`). [CP-C9] repeated save failures degrade the loop; a success clears it. [CP-C1]
func (r *LoopRunner) commit(ctx context.Context, key model.CheckpointKey, t time.Time, cursor string, epoch int64) {
	// [ext-review-2] Re-check leadership immediately before the Save: the watermark must never
	// advance (durably OR in-memory) once leadership is lost. The OTLP emitter and the prod ConfigMap
	// checkpointer both honour ctx cancellation, so in the valid lease+configmap config a lost-mid-Emit
	// window already cannot advance the durable checkpoint; but a ctx-IGNORING checkpointer (the dev
	// file impl, or any future backend) would otherwise advance here on a Save that ran after the
	// leaderCtx was cancelled. The invariant "no emit/advance under lost leadership" must hold
	// independent of a backend's ctx handling. (Emitting a settled bucket under a just-lost leadership
	// is itself harmless — the next leader re-emits it deterministically — so the gate lives at the
	// advance, not the emit.)
	if ctx.Err() != nil {
		return
	}
	w := model.Watermark{Time: t, Cursor: cursor, Epoch: epoch}
	err := r.cp.Save(ctx, key, w)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if !errors.Is(err, checkpoint.ErrStaleWrite) {
			r.saveFails++
			r.m.EmitError(r.name, "checkpoint_save")
			if r.saveFails >= checkpointFailThreshold && !r.degraded {
				r.degraded = true
				slog.Error("loop degraded: repeated checkpoint-save failures", "loop", r.name, "fails", r.saveFails)
			} else if r.lim.Allow("save:" + r.name) {
				slog.Warn("checkpoint save failed", "loop", r.name, "fails", r.saveFails)
			}
			return
		}
		// [round3-#3] ErrStaleWrite has two cases: benign already-advanced (durable Time ≥ our Time —
		// an intervening/concurrent leader is ahead) vs a forward-write FENCED by epoch (durable Time
		// BEHIND our Time yet rejected → a stale-epoch under-read, e.g. lease GET failed → epoch 0, or a
		// concurrent higher-epoch leader). The fenced case must NOT be silent: it freezes durable
		// progress while the loop looks healthy. Surface it loudly + counted, and re-sync the in-memory
		// frontier to the durable truth either way so Since() never runs ahead of a rejected write.
		if stored, lerr := r.cp.Load(ctx, key); lerr == nil {
			if stored.Time.Before(w.Time) {
				r.m.EmitError(r.name, "checkpoint_fenced")
				slog.Warn("checkpoint forward-write fenced (stale-epoch or concurrent leader); not advancing",
					"loop", r.name, "attempted", w.Time, "durable", stored.Time, "epoch", epoch)
			}
			r.frontier, r.hasFront = stored, true
		}
		return
	}
	r.saveFails = 0
	r.degraded = false // a successful save clears a transient (e.g. ConfigMap-outage) degrade
	if !r.firstOK {
		// First durable advance this leadership — a positive "leader is alive and emitting" signal on
		// stdout (commit already logs under r.mu, so this is consistent). Uniform across samples + logs
		// loops since both reach commit. Re-armed by Reset on (re-)election.
		r.firstOK = true
		slog.Info("loop committed first watermark advance (leader healthy)", "loop", r.name, "watermark", w.Time)
	}
	// Mirror the durable advance in memory so Since() never trails it. Update on a forward Time OR a
	// same-Time CURSOR advance to a NON-EMPTY cursor (a logs-export job step within a window) — matching
	// the relaxed fence — but never backward. The non-empty guard means an interior-bucket sample commit
	// (which writes an empty cursor, round3-#2) at an equal Time can never clobber a loop's live cursor:
	// a loop is either cursor-bearing (logs) or bucketed (samples), never both, so this is belt-and-braces.
	if !r.hasFront || w.Time.After(r.frontier.Time) || (w.Time.Equal(r.frontier.Time) && w.Cursor != "" && w.Cursor != r.frontier.Cursor) {
		r.frontier, r.hasFront = w, true
	}
}

// Degrade lets the SCHEDULER mark a loop degraded for a collect-side terminal condition it detects
// (e.g. a persistent granularity flip), reusing the same degraded/backoff path as a terminal emit
// reject — the scheduler then backs off to degradedBackoff instead of re-pulling every tick. A later
// successful collect+save clears it (commit resets degraded). [Cdx-M5]
func (r *LoopRunner) Degrade(reason string) { r.enterDegraded(reason) }

func (r *LoopRunner) enterDegraded(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.degraded {
		r.degraded = true
		slog.Error("loop degraded (loud halt, scheduler will back off)", "loop", r.name, "reason", reason)
	}
}

// splitByBucket groups samples by timestamp, ascending — one sub-batch per bucket (per-bucket
// emit granularity for partial-accept correctness, F10).
func splitByBucket(b model.Batch) []model.Batch {
	groups := map[int64][]model.Sample{}
	for _, s := range b.Samples {
		ns := s.Timestamp.UnixNano()
		groups[ns] = append(groups[ns], s)
	}
	keys := make([]int64, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]model.Batch, 0, len(keys))
	for _, k := range keys {
		out = append(out, model.Batch{Key: b.Key, Samples: groups[k]})
	}
	return out
}

func isAdvancePast(err error) bool {
	var re *emit.RejectError
	return errors.As(err, &re) && re.AdvancesPast()
}

// isTerminalHalt: a non-retryable reject that must NOT advance the watermark — bad-encoding (our
// bug) OR an unknown request-level 400 ([CP-C7]: advancing past it would be silent data loss on a
// misconfiguration). Both degrade the loop (loud, scheduler backs off) rather than hammer/lose.
func isTerminalHalt(err error) bool {
	var re *emit.RejectError
	return errors.As(err, &re) && (re.Reason == emit.ReasonBadEncoding || re.Reason == emit.ReasonUnknown)
}

func rejectReason(err error) string {
	var re *emit.RejectError
	if errors.As(err, &re) {
		switch re.Reason {
		case emit.ReasonDuplicateTimestamp:
			return "duplicate_timestamp"
		case emit.ReasonTooOld:
			return "too_old"
		case emit.ReasonPayloadTooLarge:
			return "payload_too_large"
		case emit.ReasonBadEncoding:
			return "bad_encoding"
		case emit.ReasonUnknown:
			return "unknown_4xx"
		}
	}
	return "unknown"
}
