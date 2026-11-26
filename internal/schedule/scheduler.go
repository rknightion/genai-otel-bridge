// SPDX-License-Identifier: Apache-2.0

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

// Pipeline names the tenant and pipeline a loop belongs to [#521]. The zero value is the legacy
// shape, where a loop belongs to no tenant and its log lines and limiter keys are as before.
type Pipeline struct {
	Tenant, ID string
}

// LogAttrs returns the slog attributes "tenant", Tenant, "pipeline", ID when ID is set, else nil, so
// a legacy loop's log lines are unchanged.
func (p Pipeline) LogAttrs() []any {
	if p.ID == "" {
		return nil
	}
	return []any{"tenant", p.Tenant, "pipeline", p.ID}
}

// limitKey is a scheduler limiter key: "<kind>:<loop>", or "<kind>:<pipeline id>:<loop>" when the
// pipeline is set, so one tenant's failing loop cannot suppress another tenant's same-named loop's
// warning [#521].
func (p Pipeline) limitKey(kind, loop string) string {
	if p.ID == "" {
		return kind + ":" + loop
	}
	return kind + ":" + p.ID + ":" + loop
}

type LoopSpec struct {
	Runner      *LoopRunner
	Loop        source.Loop
	Cadence     time.Duration
	MaxBackfill time.Duration
	// Window is the loop's query window (0 for an aggregate-now/snapshot loop); used to detect a
	// catch-up backlog (more than one window behind). MaxCatchupPerTick bounds the catch-up burst.
	Window            time.Duration
	MaxCatchupPerTick int
	// Metrics records this loop's ticks; nil means the scheduler's own [#521]. In the tenant shape it
	// is the pipeline's labelled copy, so the scheduler's series carry tenant and pipeline.
	Metrics Metrics
	// Pipeline is the tenant and pipeline the loop belongs to; zero in the legacy shape [#521].
	Pipeline Pipeline
	// Gate is the pipeline's scope gate, shared by every loop of the pipeline and asked before each
	// collect (design §5.1); nil means no gate, and the tick is exactly as without one.
	Gate *source.ScopeGate
}

// Scheduler drives the per-loop tick→collect→enqueue cycle. The runner owns the checkpoint
// (Since/commit), so the scheduler holds no Checkpointer. [CP-C1]
type Scheduler struct {
	specs []LoopSpec
	m     Metrics
	beat  func() // [AR-H-beat] heartbeat hook, fed each tick attempt; wired to selfobs.Health.Beat
	// lim throttles the per-tick warn lines (collect/checkpoint failures) to ≤1/min per loop so a
	// flapping upstream doesn't spam stdout; the metric counters carry the true rate. Keys are
	// "collect:<loop>" / "checkpoint_load:<loop>" / "scope_assertion:<loop>", with the pipeline id before
	// the loop when a spec has one ("collect:<pipeline>:<loop>", #521) — disjoint from the runner's keyspace.
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
// returning, so a leadership flap cannot leak a stale batch into the next leadership's queue. After the
// join it zeroes each degraded loop's loop_degraded gauge [#369], so a demoted replica stops exporting 1.
//
// A spec's scope gate is told of the election (Elected: reset, then read the remembered match) before
// any loop starts, once per distinct gate, and reset after the join, so a demoted replica reports no
// result.
func (s *Scheduler) Run(leaderCtx context.Context) {
	gates := s.gates()
	for _, g := range gates {
		g.Elected(leaderCtx)
	}
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
	// [#369] Leadership ended and every goroutine has joined: zero any degraded gauge now, so a demoted
	// replica does not keep exporting loop_degraded == 1 until it is re-elected or restarted.
	for _, sp := range s.specs {
		sp.Runner.ClearDegradedGauge()
	}
	for _, g := range gates {
		g.Reset()
	}
}

// gates returns each distinct non-nil scope gate of the specs, in spec order.
func (s *Scheduler) gates() []*source.ScopeGate {
	var out []*source.ScopeGate
	seen := map[*source.ScopeGate]bool{}
	for _, sp := range s.specs {
		if sp.Gate != nil && !seen[sp.Gate] {
			seen[sp.Gate] = true
			out = append(out, sp.Gate)
		}
	}
	return out
}

func (s *Scheduler) tickLoop(ctx context.Context, sp LoopSpec) {
	out := s.tick(ctx, sp, time.Now().UTC()) // [AR-M-tick] first collect immediately on election (cuts failover first-data latency)
	var st tickState
	for {
		wait, jit := st.next(sp.Runner.Degraded(), out, sp.MaxCatchupPerTick, sp.Cadence)
		if jit {
			wait = jitter(wait, jitterFrac)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			out = s.tick(ctx, sp, time.Now().UTC())
		}
	}
}

// tickOutcome is what one tick reports to the tick loop's wait planning.
type tickOutcome struct {
	more      bool // the loop is still draining: a windowed backlog, or a cursor mid-window [#374] (catch-up)
	collected bool // Collect returned without error (the source answered)
	quota     bool // Collect returned source.ErrQuotaExceeded
	auth      bool // Collect, or the scope gate's call, failed with an error wrapping source.ErrAuth (401/403) [#537]
}

// maxQuotaStreak bounds the consecutive-quota counter. Any streak this long already pins the wait at
// DegradedBackoff, so the cap only keeps the counter from growing without bound.
const maxQuotaStreak = 32

// tickState is the per-loop state the tick loop carries between ticks: the catch-up burst counter and
// the consecutive-quota streak [#370]. next folds one tick's outcome in and plans the following wait, so
// the wiring in tickLoop is a single call that tests can drive with real tick outcomes.
type tickState struct {
	burst       int // consecutive accelerated (catch-up) ticks in the current cadence period
	quotaStreak int // consecutive ticks whose Collect returned ErrQuotaExceeded or an auth error [#537]; reset by a good Collect
}

func (st *tickState) next(degraded bool, out tickOutcome, maxCatchup int, cadence time.Duration) (wait time.Duration, jit bool) {
	switch {
	case out.quota || out.auth:
		if st.quotaStreak < maxQuotaStreak {
			st.quotaStreak++
		}
	case out.collected:
		st.quotaStreak = 0
	}
	wait, st.burst, jit = tickPlan(degraded, out.more, st.burst, maxCatchup, st.quotaStreak, cadence)
	return wait, jit
}

// tickPlan decides the next tick WAIT and the updated catch-up burst counter from the loop state. Pure
// and jitter-free so the acceleration state machine (catch-up burst cap, end-of-burst breather, and the
// degraded-over-catch-up precedence) is unit-testable; the caller applies cadence jitter when jit=true.
//   - degraded ⇒ DegradedBackoff (no hammering), burst reset (CP-C8).
//   - backlog (more) and under the per-period cap ⇒ catchupInterval, burst++ (drain up to maxCatchup
//     windows per cadence period — Cdx-C13/F44; maxCatchup=1 ⇒ burst<0 ⇒ never accelerates = v1 behaviour).
//   - quota or auth streak > 0 ⇒ quotaBackoff (cadence doubled per consecutive tick, capped at
//     DegradedBackoff, never below cadence; jittered), burst reset [#370]. A quota tick never reports a
//     backlog, and the source is throttling us, so the stretched wait wins over catch-up.
//   - else ⇒ cadence (jittered), burst reset (steady state or end-of-burst breather).
func tickPlan(degraded, more bool, burst, maxCatchup, quotaStreak int, cadence time.Duration) (wait time.Duration, newBurst int, jit bool) {
	switch {
	case degraded:
		return DegradedBackoff, 0, false
	case quotaStreak > 0:
		return quotaBackoff(cadence, quotaStreak), 0, true
	case more && burst < maxCatchup-1:
		return catchupInterval, burst + 1, false
	default:
		return cadence, 0, true
	}
}

// quotaBackoff is the wait after a streak of consecutive ErrQuotaExceeded ticks: cadence << streak, capped
// at DegradedBackoff. [#370] A loop whose cadence already exceeds DegradedBackoff keeps its cadence, so a
// quota failure can never make a slow loop poll MORE often. With jitter the capped wait stays within
// DegradedBackoff+10%, below the /healthz liveness threshold (DegradedBackoff plus slack).
func quotaBackoff(cadence time.Duration, streak int) time.Duration {
	if cadence >= DegradedBackoff {
		return cadence
	}
	w := cadence
	for i := 0; i < streak && w < DegradedBackoff; i++ {
		w *= 2
	}
	return min(w, DegradedBackoff)
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
	return s.tick(ctx, sp, now).more
}

// tick is runOnce reporting the full tickOutcome: the backlog signal plus whether Collect answered or hit
// quota, which the tick loop's quota backoff needs [#370].
func (s *Scheduler) tick(ctx context.Context, sp LoopSpec, now time.Time) tickOutcome {
	if s.beat != nil {
		s.beat() // [AR-H-beat] heartbeat each tick ATTEMPT (progress, not success) so /healthz tracks loop liveness
	}
	id := MetricsLoopID(sp.Loop) // [#387] {source, loop}: same-named loop instances keep distinct series
	name := id.Loop
	m := s.m
	if sp.Metrics != nil {
		m = sp.Metrics // [#521] the pipeline's own (labelled) metrics
	}
	pattrs := sp.Pipeline.LogAttrs() // [#521] nil in the legacy shape
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
		return tickOutcome{more: sp.Window > 0}
	}
	// Design §5.1: ask the pipeline's scope gate after the busy check and before Since. Wait and
	// mismatched end the tick with no lag and no unreadable time counted; a failed call is handled as a
	// failed collect below, after the lag accounting.
	var scopeErr error
	if sp.Gate != nil {
		decision, err := sp.Gate.Before(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			span.SetAttributes(attribute.String("outcome", "cancelled"))
			return tickOutcome{}
		case err != nil:
			scopeErr = err
		case decision == source.ScopeProceed:
		case decision == source.ScopeMismatched:
			// Degraded, so the loop is next ticked at DegradedBackoff; its next successful save clears it.
			sp.Runner.Degrade("scope_mismatch")
			span.SetAttributes(attribute.String("outcome", "scope_mismatch"))
			return tickOutcome{}
		default: // ScopeWait, and any value the gate does not define
			span.SetAttributes(attribute.String("outcome", "scope_wait"))
			return tickOutcome{}
		}
	}
	wm, err := sp.Runner.Since(ctx) // [CP-C1] in-memory saved frontier, or persisted checkpoint
	if err != nil {
		if ctx.Err() != nil {
			// [#368] leadership lost / shutdown mid-load: the cancellation, not a checkpoint failure
			span.SetAttributes(attribute.String("outcome", "cancelled"))
			return tickOutcome{}
		}
		m.EmitError(id, "checkpoint_load")
		if s.lim.Allow(sp.Pipeline.limitKey("checkpoint_load", name)) {
			slog.Warn("checkpoint load failed; skipping tick", append([]any{"loop", name, "err", err}, pattrs...)...)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "checkpoint_load")
		return tickOutcome{}
	}
	if !wm.Time.IsZero() {
		// [AR-C6] window_lag floors at ~bucket_settle+cadence (we never emit unsettled buckets), so
		// staleness alerts must be thresholded ABOVE that baseline, not at zero (deploy README).
		m.WindowLag(id, now.Sub(wm.Time))
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
			m.SamplesSkipped(id, "backfill_unstorable", mins)
		}
	}
	if scopeErr != nil {
		// The scope assertion failed: like a failed collect, counted and logged against this loop, which
		// retries with nothing advanced. A 401 or 403 (*source.AuthError, an ErrAuth) also raises the
		// auth-error signal, since the asserter never calls the hook, and feeds the auth backoff streak as a
		// collect's would [#537], so a bad key does not spend the request budget at cadence.
		m.EmitError(id, "collect")
		auth := errors.Is(scopeErr, source.ErrAuth)
		if auth {
			m.AuthError(sp.Loop.Key().Loop, id.Source)
		}
		if s.lim.Allow(sp.Pipeline.limitKey("scope_assertion", name)) {
			slog.Warn("scope assertion failed; skipping collect", append([]any{"loop", name, "err", scopeErr}, pattrs...)...)
		}
		span.RecordError(scopeErr)
		span.SetStatus(codes.Error, "scope_assertion")
		return tickOutcome{auth: auth}
	}
	batch, err := sp.Loop.Collect(ctx, wm)
	switch {
	case errors.Is(err, source.ErrQuotaExceeded):
		// Discard, no advance (F34); the tick loop stretches the wait while the streak lasts [#370].
		m.SamplesSkipped(id, "quota_exceeded", 1)
		span.SetAttributes(attribute.String("outcome", "quota_exceeded"))
		return tickOutcome{quota: true}
	case errors.Is(err, source.ErrGranularityUnexpected):
		// [Cdx-M5] A granularity flip is alertable + no-advance (F27). Degrade the loop so the scheduler
		// backs off to degradedBackoff (10m) instead of re-pulling every cadence and hammering the source
		// with the same loud error; a later good collect+save clears the degrade (commit resets it).
		m.EmitError(id, "granularity_unexpected")
		sp.Runner.Degrade("granularity_unexpected")
		span.RecordError(err)
		span.SetStatus(codes.Error, "granularity_unexpected")
		return tickOutcome{}
	case err != nil && ctx.Err() != nil:
		// [#368] Leadership lost or shutdown mid-collect: the error is the cancellation, not an upstream
		// failure, so neither count nor warn. Nothing was enqueued, so nothing advances.
		span.SetAttributes(attribute.String("outcome", "cancelled"))
		return tickOutcome{}
	case err != nil:
		// An auth failure (401/403, source.ErrAuth) is still a counted collect error, but the tick loop
		// stretches the wait like a quota streak so a bad key does not spend the request budget [#537].
		auth := errors.Is(err, source.ErrAuth)
		m.EmitError(id, "collect")
		if s.lim.Allow(sp.Pipeline.limitKey("collect", name)) {
			slog.Warn("collect failed", append([]any{"loop", name, "err", err}, pattrs...)...)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "collect")
		return tickOutcome{auth: auth}
	}
	// [CP-C2] Enqueue even an EMPTY batch: Collect sets Watermark.Time = the polled settled cutoff
	// (`until`), and ProcessBatch's full-completion path commits it, advancing the frontier over a
	// confirmed-empty/quiet window so window_lag does not grow as if the poller were broken.
	if err := sp.Runner.Enqueue(ctx, batch); err != nil {
		return tickOutcome{collected: true} // ctx cancelled (leadership loss / shutdown)
	}
	span.SetAttributes(
		attribute.String("outcome", "enqueued"),
		attribute.Int("samples", len(batch.Samples)),
		attribute.Int("logs", len(batch.Logs)),
	)
	// Per-tick detail for bring-up (Debug — silent at the default info level). Not rate-limited: it only
	// fires when debug is explicitly enabled, and a steady cadence makes it self-bounding.
	slog.Debug("tick collected", append([]any{"loop", name, "since", wm.Time, "samples", len(batch.Samples), "logs", len(batch.Logs)}, pattrs...)...)
	// [Cdx-C13/F44] Backlog detection: a windowed loop whose just-collected `until` (batch.Watermark.Time)
	// is still more than one window behind `now` has further windows to drain → signal catch-up. A backlog
	// only exists when max_backfill > window (e.g. raised per GS2); at steady state the lag is ~settle <
	// window, so this is false. A snapshot loop (Window==0, no cursor) is always current → never accelerates.
	backlog := sp.Window > 0 && !batch.Watermark.Time.IsZero() && now.Sub(batch.Watermark.Time) > sp.Window
	// [#374] A cursor-driven loop (Window==0: langsmith runs, portkey logs_export) does one step per
	// Collect, so at cadence its throughput is one page or chunk per cadence. While it is mid-window (the
	// batch carries a non-empty cursor) AND the step emitted records, more pages are waiting now: signal
	// catch-up, bounded by max_catchup_per_tick and the source's rate limiter like any backlog. A
	// cursor-only lifecycle step (no records) and the window's last page (empty cursor) keep the cadence,
	// and the busy path above still never accelerates a Window==0 loop on a slow emit.
	draining := batch.Watermark.Cursor != "" && len(batch.Logs) > 0
	return tickOutcome{collected: true, more: backlog || draining}
}

// jitter returns d ± frac (uniform), used on every tick to de-align loops/replicas.
func jitter(d time.Duration, frac float64) time.Duration {
	return d + time.Duration(float64(d)*frac*(rand.Float64()*2-1))
}
