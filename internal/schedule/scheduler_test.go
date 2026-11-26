// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

type collectStub struct {
	key model.CheckpointKey
	got model.Watermark // records the `since` it was called with
	out model.Batch
}

func (c *collectStub) Key() model.CheckpointKey { return c.key }
func (c *collectStub) Cadence() time.Duration   { return time.Minute }
func (c *collectStub) Collect(_ context.Context, since model.Watermark) (model.Batch, error) {
	c.got = since
	return c.out, nil
}

type capMetrics struct {
	NoopMetrics
	lag     time.Duration
	skipped map[string]int // reason → total count, recorded from SamplesSkipped
}

func (m *capMetrics) WindowLag(_ string, d time.Duration) { m.lag = d }

func (m *capMetrics) SamplesSkipped(_ string, reason string, n int) {
	if m.skipped == nil {
		m.skipped = map[string]int{}
	}
	m.skipped[reason] += n
}

// TestSnapshotLoopNoSpuriousBackfillSkip guards a snapshot loop (Window==0, MaxBackfill==0) whose
// heartbeat watermark is necessarily a past `now`: the backfill_unstorable check (a time-bucket concept)
// must NOT fire for it (it would falsely report skipped data every tick — the langsmith/groups loops
// carry no MaxBackfill). A windowed loop with an old watermark still fires it.
func TestSnapshotLoopNoSpuriousBackfillSkip(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	run := func(window, maxBackfill time.Duration, wmSec int64) *capMetrics {
		key := model.CheckpointKey{SourceInstance: "s", Loop: "snap", OutputFingerprint: "fp"}
		cp := newMemCP()
		cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(wmSec, 0).UTC(), Epoch: 1})
		loop := &collectStub{key: key, out: batchAt(key, wmSec)}
		m := &capMetrics{}
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
		sch := NewScheduler(nil, m)
		sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, Window: window, MaxBackfill: maxBackfill}, now)
		return m
	}
	// Snapshot loop: heartbeat wm 60s in the past, MaxBackfill 0 ⇒ floor==now ⇒ wm<floor, but Window==0
	// so the backfill check is N/A and must NOT fire.
	if got := run(0, 0, 1940).skipped["backfill_unstorable"]; got != 0 {
		t.Fatalf("snapshot loop must not count backfill_unstorable, got %d", got)
	}
	// Windowed loop genuinely behind the accept window still fires (regression guard for the real case).
	if got := run(50*time.Minute, 10*time.Minute, 200).skipped["backfill_unstorable"]; got == 0 {
		t.Fatal("a windowed loop older than max_backfill must still count backfill_unstorable")
	}
}

func TestRunOnceLoadsWatermarkAndEnqueues(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 1})
	loop := &collectStub{key: key, out: batchAt(key, 1060)}
	m := &capMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	sch := NewScheduler(nil, m)
	now := time.Unix(1100, 0).UTC()
	sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, now)
	if !loop.got.Time.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("Collect since=%v want 1000", loop.got.Time)
	}
	if m.lag != 100*time.Second {
		t.Fatalf("window_lag=%v want 100s", m.lag)
	}
}

// TestRunOnceEmitsTickSpan guards the opt-in self-APM tracing (followup §4): each tick emits a
// `loop.tick` span tagged with the loop name and the outcome (sample/log counts), via the OTel GLOBAL
// tracer — so when tracing is disabled the global stays the no-op tracer and the tick pays nothing, and
// when enabled (main installs an SDK provider) the poll/emit pipeline is traceable. Asserted with an
// in-memory recorder, no collector.
func TestRunOnceEmitsTickSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(noop.NewTracerProvider()) // restore the no-op global

	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 1})
	loop := &collectStub{key: key, out: batchAt(key, 1060)}
	m := &capMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	sch := NewScheduler(nil, m)
	sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())

	var tick bool
	for _, sp := range sr.Ended() {
		if sp.Name() != "loop.tick" {
			continue
		}
		tick = true
		var loopAttr string
		for _, kv := range sp.Attributes() {
			if string(kv.Key) == "loop" {
				loopAttr = kv.Value.AsString()
			}
		}
		if loopAttr != "analytics" {
			t.Fatalf("loop.tick span loop attr=%q want analytics", loopAttr)
		}
	}
	if !tick {
		t.Fatal("no loop.tick span recorded")
	}
}

// granularityStub always fails Collect with ErrGranularityUnexpected (a persistent flip).
type granularityStub struct{ key model.CheckpointKey }

func (g *granularityStub) Key() model.CheckpointKey { return g.key }
func (g *granularityStub) Cadence() time.Duration   { return time.Minute }
func (g *granularityStub) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, source.ErrGranularityUnexpected
}

// TestGranularityFlipDegrades asserts a granularity flip degrades the loop so the scheduler backs off
// (degradedBackoff) instead of re-pulling the same loud error every cadence tick (Cdx-M5).
func TestGranularityFlipDegrades(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	loop := &granularityStub{key: key}
	m := &capMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, m)
	sch := NewScheduler(nil, m)
	if r.Degraded() {
		t.Fatal("precondition: runner should not start degraded")
	}
	sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
	if !r.Degraded() {
		t.Fatal("granularity flip should degrade the loop so the scheduler backs off, not re-pull every tick")
	}
}

// TestCatchupBacklogDetection asserts runOnce signals catch-up (more=true) only when a WINDOWED loop is
// more than one window behind now; a caught-up loop and a snapshot loop (window 0) never accelerate.
func TestCatchupBacklogDetection(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	run := func(window time.Duration, untilSec, nowSec int64) bool {
		cp := newMemCP()
		cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(untilSec, 0).UTC(), Epoch: 1})
		loop := &collectStub{key: key, out: batchAt(key, untilSec)}
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, &NoopMetrics{})
		sch := NewScheduler(nil, &NoopMetrics{})
		return sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour,
			Window: window, MaxCatchupPerTick: 4}, time.Unix(nowSec, 0).UTC())
	}
	// Backlog: collected `until` is >1 window (50m) behind now (66m) → accelerate.
	if !run(50*time.Minute, 100000, 100000+4000) {
		t.Error("expected backlog (until >1 window behind now) → more=true")
	}
	// Caught up: until is <window behind now (10m) → no acceleration.
	if run(50*time.Minute, 100000, 100000+600) {
		t.Error("expected caught-up (until <window behind now) → more=false")
	}
	// Snapshot loop (window 0) is always current → never accelerates.
	if run(0, 100000, 100000+100000) {
		t.Error("snapshot loop (window 0) must never signal catch-up")
	}

	// [#92] Busy-skip path: a batch in flight short-circuits runOnce at Busy() BEFORE the end-of-tick
	// backlog gate. It must still respect the snapshot/windowed distinction — a busy WINDOWED loop
	// signals catch-up (drain fast once the in-flight batch clears), a busy SNAPSHOT loop (Window==0)
	// must NOT accelerate to 2s ticks just because a slow emit outlasted its cadence.
	runBusy := func(window time.Duration) bool {
		cp := newMemCP()
		cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(100000, 0).UTC(), Epoch: 1})
		loop := &collectStub{key: key, out: batchAt(key, 100000)}
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, &NoopMetrics{})
		if err := r.Enqueue(leaderCtx(), batchAt(key, 100000)); err != nil { // mark busy; no worker drains it
			t.Fatal(err)
		}
		if !r.Busy() {
			t.Fatal("precondition: runner should be busy after Enqueue with no worker draining")
		}
		sch := NewScheduler(nil, &NoopMetrics{})
		return sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour,
			Window: window, MaxCatchupPerTick: 4}, time.Unix(100000+4000, 0).UTC())
	}
	if !runBusy(50 * time.Minute) {
		t.Error("busy windowed loop must still signal catch-up (fast retry while draining a backlog)")
	}
	if runBusy(0) {
		t.Error("[#92] busy snapshot loop (window 0) must NOT accelerate to 2s ticks")
	}
}

// failCollectStub always fails Collect with a generic upstream error (the outage that caused the old
// watermark). Used to pin the frontier across ticks so the backfill_unstorable de-dup can be asserted.
type failCollectStub struct{ key model.CheckpointKey }

func (c *failCollectStub) Key() model.CheckpointKey { return c.key }
func (c *failCollectStub) Cadence() time.Duration   { return time.Minute }
func (c *failCollectStub) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, errors.New("upstream still down")
}

// TestBackfillUnstorableNotRecountedWhileStuck guards [#94]: a windowed loop whose watermark is behind
// now−max_backfill and whose Collect keeps failing (frontier pinned) must count the abandoned span
// ONCE, then only ~cadence per subsequent stuck tick — not the full span every tick (which inflated
// samples_skipped_total by orders of magnitude and tripped skipped-samples SLOs on a one-time gap).
func TestBackfillUnstorableNotRecountedWhileStuck(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	// wm 1000s; max_backfill 90m; pick `now` so floor = now−90m is ~149m ahead of wm.
	// now=15340 ⇒ floor=15340−5400=9940 ⇒ span=(9940−1000)/60=149 minutes.
	cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 1})
	loop := &failCollectStub{key: key}
	m := &capMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	sch := NewScheduler(nil, m)
	sp := LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, Window: 50 * time.Minute, MaxBackfill: 90 * time.Minute}
	base := time.Unix(15340, 0).UTC()
	for i := 0; i < 3; i++ {
		sch.runOnce(leaderCtx(), sp, base.Add(time.Duration(i)*time.Minute)) // 3 failing ticks, 1m apart
	}
	got := m.skipped["backfill_unstorable"]
	// Correct: ~149 (span, counted once) + 1 + 1 (cadence deltas) ≈ 151. Bug counted ~149×3 = ~447.
	if got < 149 || got > 160 {
		t.Fatalf("[#94] backfill_unstorable=%d over 3 stuck ticks; want ~150 (span once + cadence deltas), not the span re-counted each tick", got)
	}
}

// TestTickPlan covers the catch-up acceleration state machine (the contiguity-adjacent timing logic):
// degraded precedence, the maxCatchup off-by-one (N=1 ⇒ never accelerate = v1 behaviour), and the
// per-period burst cap + end-of-burst breather.
func TestTickPlan(t *testing.T) {
	const cad = time.Minute
	// Degraded wins over a backlog: slow backoff, burst reset, no jitter.
	if w, b, jit := tickPlan(true, true, 2, 4, 0, cad); w != DegradedBackoff || b != 0 || jit {
		t.Errorf("degraded: got (%v,%d,%v) want (%v,0,false)", w, b, jit, DegradedBackoff)
	}
	// No backlog: steady-state cadence (jittered), burst reset.
	if w, b, jit := tickPlan(false, false, 3, 4, 0, cad); w != cad || b != 0 || !jit {
		t.Errorf("steady: got (%v,%d,%v) want (%v,0,true)", w, b, jit, cad)
	}
	// maxCatchup=1 (default) ⇒ burst<0 ⇒ NEVER accelerates even with a backlog (exact v1 behaviour).
	if w, _, jit := tickPlan(false, true, 0, 1, 0, cad); w != cad || !jit {
		t.Errorf("N=1 backlog must not accelerate: got (%v, jit=%v)", w, jit)
	}
	// maxCatchup=4 + backlog: 3 accelerated ticks (burst 0→1→2→3) then a cadence breather at burst==3.
	for in, wantAccel := range map[int]bool{0: true, 1: true, 2: true, 3: false} {
		w, nb, jit := tickPlan(false, true, in, 4, 0, cad)
		if wantAccel && (w != catchupInterval || nb != in+1 || jit) {
			t.Errorf("burst=%d N=4: expected accelerate, got (%v,%d,%v)", in, w, nb, jit)
		}
		if !wantAccel && (w != cad || nb != 0 || !jit) {
			t.Errorf("burst=%d N=4: expected breather, got (%v,%d,%v)", in, w, nb, jit)
		}
	}
	// [#370] Quota streak: cadence doubled per consecutive quota tick (jittered), capped at DegradedBackoff.
	for _, tc := range []struct {
		streak int
		want   time.Duration
	}{
		{1, 2 * cad}, {2, 4 * cad}, {3, 8 * cad}, {4, DegradedBackoff}, {maxQuotaStreak, DegradedBackoff},
	} {
		if w, b, jit := tickPlan(false, false, 0, 4, tc.streak, cad); w != tc.want || b != 0 || !jit {
			t.Errorf("quota streak=%d: got (%v,%d,%v) want (%v,0,true)", tc.streak, w, b, jit, tc.want)
		}
	}
	// Degraded keeps precedence over any quota streak: slow backoff, no jitter.
	for _, streak := range []int{1, 3, maxQuotaStreak} {
		if w, _, jit := tickPlan(true, false, 0, 4, streak, cad); w != DegradedBackoff || jit {
			t.Errorf("degraded with quota streak=%d: got (%v, jit=%v) want (%v, false)", streak, w, jit, DegradedBackoff)
		}
	}
	// The stretched quota wait wins over catch-up acceleration and resets the burst.
	if w, b, _ := tickPlan(false, true, 1, 4, 1, cad); w != 2*cad || b != 0 {
		t.Errorf("quota streak with backlog: got (%v,%d) want (%v,0)", w, b, 2*cad)
	}
	// A loop slower than DegradedBackoff keeps its cadence: quota never makes it poll more often.
	if w, _, jit := tickPlan(false, false, 0, 4, 3, time.Hour); w != time.Hour || !jit {
		t.Errorf("slow loop with quota streak: got (%v, jit=%v) want (1h, true)", w, jit)
	}
}

// quotaThenOKLoop returns ErrQuotaExceeded while quota is set, a generic error while fail is set, and a
// one-bucket batch otherwise.
type quotaThenOKLoop struct {
	key         model.CheckpointKey
	quota, fail bool
}

func (l *quotaThenOKLoop) Key() model.CheckpointKey { return l.key }
func (l *quotaThenOKLoop) Cadence() time.Duration   { return time.Minute }
func (l *quotaThenOKLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	switch {
	case l.quota:
		return model.Batch{}, source.ErrQuotaExceeded
	case l.fail:
		return model.Batch{}, errors.New("upstream 500")
	}
	return batchAt(l.key, 1060), nil
}

// TestQuotaStreakStretchesWait drives real ticks through the same tickState.next the tick loop uses
// [#370]: consecutive quota ticks stretch the wait (doubling, capped at DegradedBackoff), a non-quota
// Collect failure holds the streak, and one successful Collect returns the wait to the cadence. Each
// quota tick still counts samples_skipped{reason="quota_exceeded"} and advances nothing.
func TestQuotaStreakStretchesWait(t *testing.T) {
	const cad = time.Minute
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	durable := model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 1}
	if err := cp.Save(context.Background(), key, durable); err != nil {
		t.Fatal(err)
	}
	loop := &quotaThenOKLoop{key: key, quota: true}
	m := &capMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	sch := NewScheduler(nil, m)
	sp := LoopSpec{Runner: r, Loop: loop, Cadence: cad, MaxBackfill: time.Hour}
	now := time.Unix(1100, 0).UTC()
	var st tickState
	step := func() time.Duration {
		w, _ := st.next(r.Degraded(), sch.tick(leaderCtx(), sp, now), sp.MaxCatchupPerTick, sp.Cadence)
		return w
	}

	for i, want := range []time.Duration{2 * cad, 4 * cad, 8 * cad, DegradedBackoff, DegradedBackoff} {
		if got := step(); got != want {
			t.Fatalf("quota tick %d: wait=%v want %v", i+1, got, want)
		}
	}
	if got := m.skipped["quota_exceeded"]; got != 5 {
		t.Fatalf("samples_skipped{quota_exceeded}=%d want 5 (one per quota tick)", got)
	}
	if got, _ := cp.Load(context.Background(), key); got != durable {
		t.Fatalf("a quota tick must not advance the watermark: durable=%+v want %+v", got, durable)
	}
	if r.Busy() || len(r.q) != 0 {
		t.Fatalf("a quota tick must enqueue nothing: busy=%v queued=%d", r.Busy(), len(r.q))
	}
	// A degraded loop waits DegradedBackoff whatever the streak (no jitter).
	if w, jit := (&tickState{quotaStreak: 2}).next(true, tickOutcome{quota: true}, 1, cad); w != DegradedBackoff || jit {
		t.Fatalf("degraded with a quota streak: wait=(%v, jit=%v) want (%v, false)", w, jit, DegradedBackoff)
	}

	// One successful Collect returns the wait to the cadence.
	loop.quota = false
	if got := step(); got != cad {
		t.Fatalf("after a successful Collect: wait=%v want cadence %v", got, cad)
	}
	r.Reset() // drop the enqueued batch so the next ticks are not skipped as busy

	// A fresh streak starts again from 2× cadence; a non-quota Collect failure holds the streak.
	loop.quota = true
	if got := step(); got != 2*cad {
		t.Fatalf("new quota streak: wait=%v want %v", got, 2*cad)
	}
	loop.quota, loop.fail = false, true
	if got := step(); got != 2*cad {
		t.Fatalf("a non-quota Collect failure must hold the streak: wait=%v want %v", got, 2*cad)
	}
}

func TestJitterBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := jitter(time.Minute, 0.10)
		if d < 54*time.Second || d > 66*time.Second {
			t.Fatalf("jitter out of ±10%%: %v", d)
		}
	}
}
