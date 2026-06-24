// SPDX-License-Identifier: AGPL-3.0-only

package schedule

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
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
}

// TestTickPlan covers the catch-up acceleration state machine (the contiguity-adjacent timing logic):
// degraded precedence, the maxCatchup off-by-one (N=1 ⇒ never accelerate = v1 behaviour), and the
// per-period burst cap + end-of-burst breather.
func TestTickPlan(t *testing.T) {
	const cad = time.Minute
	// Degraded wins over a backlog: slow backoff, burst reset, no jitter.
	if w, b, jit := tickPlan(true, true, 2, 4, cad); w != DegradedBackoff || b != 0 || jit {
		t.Errorf("degraded: got (%v,%d,%v) want (%v,0,false)", w, b, jit, DegradedBackoff)
	}
	// No backlog: steady-state cadence (jittered), burst reset.
	if w, b, jit := tickPlan(false, false, 3, 4, cad); w != cad || b != 0 || !jit {
		t.Errorf("steady: got (%v,%d,%v) want (%v,0,true)", w, b, jit, cad)
	}
	// maxCatchup=1 (default) ⇒ burst<0 ⇒ NEVER accelerates even with a backlog (exact v1 behaviour).
	if w, _, jit := tickPlan(false, true, 0, 1, cad); w != cad || !jit {
		t.Errorf("N=1 backlog must not accelerate: got (%v, jit=%v)", w, jit)
	}
	// maxCatchup=4 + backlog: 3 accelerated ticks (burst 0→1→2→3) then a cadence breather at burst==3.
	for in, wantAccel := range map[int]bool{0: true, 1: true, 2: true, 3: false} {
		w, nb, jit := tickPlan(false, true, in, 4, cad)
		if wantAccel && (w != catchupInterval || nb != in+1 || jit) {
			t.Errorf("burst=%d N=4: expected accelerate, got (%v,%d,%v)", in, w, nb, jit)
		}
		if !wantAccel && (w != cad || nb != 0 || !jit) {
			t.Errorf("burst=%d N=4: expected breather, got (%v,%d,%v)", in, w, nb, jit)
		}
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
