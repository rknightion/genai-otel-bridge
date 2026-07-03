// SPDX-License-Identifier: AGPL-3.0-only

package schedule

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// tracerName scopes the self-APM spans this package emits. Spans go through the OTel GLOBAL tracer, so
// when self-tracing is disabled (the default) the global is a no-op and a tick allocates a no-op span —
// negligible at cadence ≥ 10s. main installs a real TracerProvider only when selfobs.tracing.enabled.
const tracerName = "genai-otel-bridge/schedule"

// DegradedBackoff is the slow interval a degraded/halted loop is retried on instead of every cadence
// (no hammering). Exported so the composition root derives the /healthz liveness threshold from the
// real value (CP-C5) rather than a coincidental duplicate.
const DegradedBackoff = 10 * time.Minute

// catchupInterval is the short tick interval used while a loop is draining a backlog (more than one
// window behind). It only shortens the WAIT between ticks — every collect still goes through the
// single-flight Busy()-gated path, and the source's own rate limiter throttles the actual API calls —
// so acceleration changes timing only, never watermark contiguity. (DESIGN §7a Cdx-C13/F44.)
const catchupInterval = 2 * time.Second

type LoopSpec struct {
	Runner      *LoopRunner
	Loop        source.Loop
	Cadence     time.Duration
	MaxBackfill time.Duration
	// Window is the loop's query window (0 for an aggregate-now/snapshot loop); used to detect a
	// catch-up backlog (more than one window behind). MaxCatchupPerTick bounds the catch-up burst.
	Window            time.Duration
	MaxCatchupPerTick int
}

// Scheduler drives the per-loop tick→collect→enqueue cycle. The runner owns the checkpoint
// (Since/commit), so the scheduler holds no Checkpointer. [CP-C1]
type Scheduler struct {
	specs []LoopSpec
	m     Metrics
	beat  func() // [AR-H-beat] heartbeat hook, fed each tick attempt; wired to selfobs.Health.Beat
	// lim throttles the per-tick warn lines (collect/checkpoint failures) to ≤1/min per loop so a
	// flapping upstream doesn't spam stdout; the metric counters carry the true rate. Keys are
	// "collect:<loop>" / "checkpoint_load:<loop>" — disjoint from the runner's keyspace.
	lim *logging.Limiter
}

func NewScheduler(specs []LoopSpec, m Metrics) *Scheduler {
	return &Scheduler{specs: specs, m: m, lim: logging.NewLimiter(time.Minute)}
}

// SetBeat wires the health heartbeat (called by app.Run before coord.Run). [AR-H-beat]
func (s *Scheduler) SetBeat(beat func()) { s.beat = beat }

// RunOnceForTest performs one tick with an explicit clock. TEST-ONLY seam (lets acceptance tests in
// another package drive deterministic windows incl. the max_backfill abandonment path); prod calls
// runOnce internally with time.Now.
func (s *Scheduler) RunOnceForTest(ctx context.Context, sp LoopSpec, now time.Time) {
	s.runOnce(ctx, sp, now)
}

// Run starts a worker + a ticker goroutine per loop; all stop when leaderCtx is cancelled.
// [CP-R3] On each (re-)election it Resets every runner FIRST (drop stale pre-election queued work +
// re-sync the frontier from the durable checkpoint), and wg.Waits its goroutines out before
// returning, so a leadership flap cannot leak a stale batch into the next leadership's queue.
func (s *Scheduler) Run(leaderCtx context.Context) {
	var wg sync.WaitGroup
	for _, sp := range s.specs {
		sp := sp
		sp.Runner.Reset()
		wg.Add(2)
		go func() { defer wg.Done(); sp.Runner.Run(leaderCtx) }() // single emit worker per loop
		go func() { defer wg.Done(); s.tickLoop(leaderCtx, sp) }()
	}
	<-leaderCtx.Done()
	wg.Wait()
}

func (s *Scheduler) tickLoop(ctx context.Context, sp LoopSpec) {
	more := s.runOnce(ctx, sp, time.Now().UTC()) // [AR-M-tick] first collect immediately on election (cuts failover first-data latency)
	burst := 0                                   // consecutive accelerated (catch-up) ticks in the current cadence period
	for {
		wait, newBurst, jit := tickPlan(sp.Runner.Degraded(), more, burst, sp.MaxCatchupPerTick, sp.Cadence)
		burst = newBurst
		if jit {
			wait = jitter(wait, jitterFrac)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			more = s.runOnce(ctx, sp, time.Now().UTC())
		}
	}
}

// tickPlan decides the next tick WAIT and the updated catch-up burst counter from the loop state. Pure
// and jitter-free so the acceleration state machine (catch-up burst cap, end-of-burst breather, and the
// degraded-over-catch-up precedence) is unit-testable; the caller applies cadence jitter when jit=true.
//   - degraded ⇒ DegradedBackoff (no hammering), burst reset (CP-C8).
//   - backlog (more) and under the per-period cap ⇒ catchupInterval, burst++ (drain up to maxCatchup
//     windows per cadence period — Cdx-C13/F44; maxCatchup=1 ⇒ burst<0 ⇒ never accelerates = v1 behaviour).
//   - else ⇒ cadence (jittered), burst reset (steady state or end-of-burst breather).
func tickPlan(degraded, more bool, burst, maxCatchup int, cadence time.Duration) (wait time.Duration, newBurst int, jit bool) {
	switch {
	case degraded:
		return DegradedBackoff, 0, false
	case more && burst < maxCatchup-1:
		return catchupInterval, burst + 1, false
	default:
		return cadence, 0, true
	}
}

const jitterFrac = 0.10

// runOnce performs one loop tick. [CP-C1] It is single-flight over collection: if a batch is still
// in flight (collected-but-unsaved) it skips, so it never re-reads a stale checkpoint and enqueues a
// duplicate window. The `since` is the runner's in-memory saved frontier (or the persisted
// checkpoint on first poll / new leader), never a value behind the in-flight batch.
// runOnce performs one loop tick and returns `more` = the loop is still draining a backlog (more than
// one window behind), so the tick loop should accelerate. It is always single-flight (Busy-gated), so
// returning `more` only shortens the WAIT — never overlaps windows or changes watermark contiguity.
func (s *Scheduler) runOnce(ctx context.Context, sp LoopSpec, now time.Time) (more bool) {
	if s.beat != nil {
		s.beat() // [AR-H-beat] heartbeat each tick ATTEMPT (progress, not success) so /healthz tracks loop liveness
	}
	name := sp.Loop.Key().Loop
	// [followup §4] One span per tick covering the SYNCHRONOUS work (collect → enqueue). The async emit
	// runs in the runner's worker (decoupled via the queue), so it is NOT in this span — it is covered by
	// emit_errors_total + the upstream-request histogram; cross-queue span propagation is a future path.
	ctx, span := otel.Tracer(tracerName).Start(ctx, "loop.tick", trace.WithAttributes(attribute.String("loop", name)))
	defer span.End()
	if sp.Runner.Busy() {
		span.SetAttributes(attribute.String("outcome", "skipped_busy"))
		// [CP-C1] prior batch in flight — skip collect. Signal catch-up (fast retry) only for a WINDOWED
		// loop that is genuinely draining a backlog; a snapshot loop (Window==0) must NOT accelerate to
		// 2s ticks just because a slow emit outlasted its cadence ([#92]) — that mirrors the end-of-tick
		// backlog gate below (scheduler.go: `return sp.Window > 0 && ...`) and DESIGN §7's "snapshot loops
		// never get catch-up acceleration". A busy snapshot loop simply retries at its jittered cadence.
		return sp.Window > 0
	}
	wm, err := sp.Runner.Since(ctx) // [CP-C1] in-memory saved frontier, or persisted checkpoint
	if err != nil {
		s.m.EmitError(name, "checkpoint_load")
		if s.lim.Allow("checkpoint_load:" + name) {
			slog.Warn("checkpoint load failed; skipping tick", "loop", name, "err", err)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "checkpoint_load")
		return false
	}
	if !wm.Time.IsZero() {
		// [AR-C6] window_lag floors at ~bucket_settle+cadence (we never emit unsettled buckets), so
		// staleness alerts must be thresholded ABOVE that baseline, not at zero (deploy README).
		s.m.WindowLag(name, now.Sub(wm.Time))
	}
	// [AR-C6] Count an abandoned-as-unstorable span (older than the backend accept window) ONCE — the
	// source clamps its start to the same floor and the runner's commit(until) advances past it, so
	// we only COUNT here (no direct Save; Busy() ensures this fires ~once per catch-up). (F25/F47)
	// Gated on a WINDOWED loop: backfill is a time-bucket concept. A snapshot loop's watermark is a
	// liveness heartbeat (= the last poll's `now`, so always slightly in the past) and carries no
	// MaxBackfill — without this guard floor==now and every tick would falsely count backfill_unstorable.
	if floor := now.Add(-sp.MaxBackfill); sp.Window > 0 && !wm.Time.IsZero() && wm.Time.Before(floor) {
		// [#94] Count only the NEWLY-unstorable minutes. If Collect/emit keeps failing the frontier stays
		// pinned while `now` (and thus `floor`) advances each tick — counting `floor - wm` unconditionally
		// would re-count the entire abandoned span every tick and inflate samples_skipped_total by orders
		// of magnitude. The runner records the last-counted floor per frontier so the initial gap is
		// counted once and only ~cadence is added per subsequent stuck tick.
		if mins := sp.Runner.BackfillUnstorableMinutes(wm.Time, floor); mins > 0 {
			s.m.SamplesSkipped(name, "backfill_unstorable", mins)
		}
	}
	batch, err := sp.Loop.Collect(ctx, wm)
	switch {
	case errors.Is(err, source.ErrQuotaExceeded):
		s.m.SamplesSkipped(name, "quota_exceeded", 1) // discard, no advance, re-pull next tick (F34)
		span.SetAttributes(attribute.String("outcome", "quota_exceeded"))
		return
	case errors.Is(err, source.ErrGranularityUnexpected):
		// [Cdx-M5] A granularity flip is alertable + no-advance (F27). Degrade the loop so the scheduler
		// backs off to degradedBackoff (10m) instead of re-pulling every cadence and hammering the source
		// with the same loud error; a later good collect+save clears the degrade (commit resets it).
		s.m.EmitError(name, "granularity_unexpected")
		sp.Runner.Degrade("granularity_unexpected")
		span.RecordError(err)
		span.SetStatus(codes.Error, "granularity_unexpected")
		return
	case err != nil:
		s.m.EmitError(name, "collect")
		if s.lim.Allow("collect:" + name) {
			slog.Warn("collect failed", "loop", name, "err", err)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "collect")
		return
	}
	// [CP-C2] Enqueue even an EMPTY batch: Collect sets Watermark.Time = the polled settled cutoff
	// (`until`), and ProcessBatch's full-completion path commits it, advancing the frontier over a
	// confirmed-empty/quiet window so window_lag does not grow as if the poller were broken.
	if err := sp.Runner.Enqueue(ctx, batch); err != nil {
		return false // ctx cancelled (leadership loss / shutdown)
	}
	span.SetAttributes(
		attribute.String("outcome", "enqueued"),
		attribute.Int("samples", len(batch.Samples)),
		attribute.Int("logs", len(batch.Logs)),
	)
	// Per-tick detail for bring-up (Debug — silent at the default info level). Not rate-limited: it only
	// fires when debug is explicitly enabled, and a steady cadence makes it self-bounding.
	slog.Debug("tick collected", "loop", name, "since", wm.Time, "samples", len(batch.Samples), "logs", len(batch.Logs))
	// [Cdx-C13/F44] Backlog detection: a windowed loop whose just-collected `until` (batch.Watermark.Time)
	// is still more than one window behind `now` has further windows to drain → signal catch-up. A backlog
	// only exists when max_backfill > window (e.g. raised per GS2); at steady state the lag is ~settle <
	// window, so this is false. A snapshot loop (Window==0) is always current → never accelerates.
	return sp.Window > 0 && !batch.Watermark.Time.IsZero() && now.Sub(batch.Watermark.Time) > sp.Window
}

// jitter returns d ± frac (uniform), used on every tick to de-align loops/replicas.
func jitter(d time.Duration, frac float64) time.Duration {
	return d + time.Duration(float64(d)*frac*(rand.Float64()*2-1))
}
