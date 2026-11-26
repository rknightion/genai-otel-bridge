// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// [AR-H-beat] runOnce must feed the heartbeat each tick attempt so /healthz tracks loop liveness.
func TestRunOnceCallsBeat(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	loop := &collectStub{key: key, out: batchAt(key, 1060)}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	sch := NewScheduler(nil, NoopMetrics{})
	var beats int32
	sch.SetBeat(func() { atomic.AddInt32(&beats, 1) })
	sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
	if atomic.LoadInt32(&beats) != 1 {
		t.Fatalf("beat called %d times, want 1 (AR-H-beat wiring)", beats)
	}
}

// errCapMetrics records EmitError kinds.
type errCapMetrics struct {
	NoopMetrics
	mu    sync.Mutex
	kinds map[string]int
}

func newErrCap() *errCapMetrics { return &errCapMetrics{kinds: map[string]int{}} }
func (m *errCapMetrics) EmitError(_ LoopID, kind string) {
	m.mu.Lock()
	m.kinds[kind]++
	m.mu.Unlock()
}
func (m *errCapMetrics) count(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kinds[kind]
}

// [round3-#3] A forward commit rejected by the epoch fence (durable BEHIND our time, but our epoch is
// lower — e.g. a lease-transition under-read) must be LOUD (checkpoint_fenced counter), not silent,
// and the in-memory frontier must re-sync to the durable value (not run ahead of a rejected write).
func TestForwardWriteFencedIsLoud(t *testing.T) {
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	// Durable frontier from a higher-epoch leader: time=100s, epoch=5.
	cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 5})

	m := newErrCap()
	em := &fakeEmitter{byTS: map[int64]error{}}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)

	// Process under a LOWER epoch (1) a batch advancing to 200s. Emit succeeds; the commit Save is
	// rejected by the epoch fence (incoming epoch 1 < stored 5) even though 200s > durable 100s.
	r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), batchAt(key, 200))

	if m.count("checkpoint_fenced") == 0 {
		t.Fatal("epoch-fenced forward write must increment checkpoint_fenced (not be silent)")
	}
	// Durable must be unchanged (still 100s); in-memory frontier re-synced to durable, not 200s.
	if got, _ := cp.Load(context.Background(), key); got.Time.Unix() != 100 {
		t.Fatalf("durable watermark=%d want 100 (forward write must stay fenced)", got.Time.Unix())
	}
	since, _ := r.Since(context.Background())
	if since.Time.Unix() != 100 {
		t.Fatalf("in-memory frontier=%d want 100 (re-synced to durable, not the rejected 200)", since.Time.Unix())
	}
	// [#367] A fenced loop must back off (degraded) rather than re-collect and re-emit the same window at
	// full cadence until a human restarts the pod.
	if !r.Degraded() {
		t.Fatal("an epoch-fenced forward write must degrade the loop so the scheduler backs off")
	}
}

// degradeCall is one recorded Metrics.LoopDegraded call.
type degradeCall struct {
	loop, reason string
	on           bool
}

// stateCapMetrics records EmitError kinds (via errCapMetrics) and every LoopDegraded call, so a test can
// assert the degraded gauge's {loop, reason} series is set and later zeroed on the same series.
type stateCapMetrics struct {
	errCapMetrics
	dmu      sync.Mutex
	degrades []degradeCall
}

func newStateCap() *stateCapMetrics {
	return &stateCapMetrics{errCapMetrics: errCapMetrics{kinds: map[string]int{}}}
}
func (m *stateCapMetrics) LoopDegraded(id LoopID, reason string, on bool) {
	m.dmu.Lock()
	m.degrades = append(m.degrades, degradeCall{loop: id.Loop, reason: reason, on: on})
	m.dmu.Unlock()
}
func (m *stateCapMetrics) degradeCalls() []degradeCall {
	m.dmu.Lock()
	defer m.dmu.Unlock()
	return append([]degradeCall(nil), m.degrades...)
}

// [#367] A logs loop persisting a job step at an UNCHANGED Time with a new Cursor, rejected only because
// the stored epoch is higher, made no Time progress, so the old "stored.Time.Before(w.Time)" test classed
// it as a benign stale write: no checkpoint_fenced, no log, no degrade. In the first window (Time zero)
// window_lag is not recorded either, so nothing signalled the stall. It must count and degrade.
func TestCursorOnlyEpochFenceIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		t0   time.Time
	}{
		{name: "first_window_zero_time", t0: time.Time{}},
		{name: "mid_window", t0: time.Unix(100, 0).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := newMemCP()
			key := model.CheckpointKey{SourceInstance: "pk", Loop: "logs_export", OutputFingerprint: "fp"}
			if err := cp.Save(context.Background(), key, model.Watermark{Time: tc.t0, Cursor: "phaseA", Epoch: 5}); err != nil {
				t.Fatal(err)
			}
			m := newStateCap()
			r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
			r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), model.Batch{Key: key, Watermark: model.Watermark{Time: tc.t0, Cursor: "phaseB"}})

			got, _ := cp.Load(context.Background(), key)
			if got.Cursor != "phaseA" {
				t.Fatalf("precondition: the epoch fence must reject the step, durable cursor=%q want phaseA", got.Cursor)
			}
			if m.count("checkpoint_fenced") != 1 {
				t.Fatalf("epoch-fenced cursor-only step must count checkpoint_fenced once, kinds=%v", m.kinds)
			}
			if !r.Degraded() {
				t.Fatal("a fenced write must degrade the loop so the scheduler backs off")
			}
			if calls := m.degradeCalls(); len(calls) != 1 || calls[0] != (degradeCall{loop: "logs_export", reason: "checkpoint fenced", on: true}) {
				t.Fatalf("loop_degraded calls=%v want one {logs_export, checkpoint fenced, true}", calls)
			}
			if since, _ := r.Since(context.Background()); since.Cursor != "phaseA" {
				t.Fatalf("in-memory frontier cursor=%q want phaseA (re-synced to durable)", since.Cursor)
			}
		})
	}
}

// [#367] AC3: a stale write that would make NO progress over the durable value is the benign
// already-advanced case and stays quiet: equal epochs with durable ahead, an identical retry, and an
// interior-bucket commit (empty cursor) at the durable Time. Durable ahead under a higher stored epoch
// is also not progress: the frontier re-syncs and a later forward write is what gets fenced.
func TestBenignStaleWriteNeitherCountsNorDegrades(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	for _, tc := range []struct {
		name   string
		stored model.Watermark
		t      int64
		cursor string
		epoch  int64
	}{
		{name: "durable_ahead_equal_epoch", stored: model.Watermark{Time: time.Unix(200, 0).UTC(), Epoch: 1}, t: 100, epoch: 1},
		{name: "identical_retry_equal_epoch", stored: model.Watermark{Time: time.Unix(200, 0).UTC(), Cursor: "c", Epoch: 1}, t: 200, cursor: "c", epoch: 1},
		{name: "empty_cursor_same_time_higher_stored_epoch", stored: model.Watermark{Time: time.Unix(200, 0).UTC(), Cursor: "c", Epoch: 5}, t: 200, epoch: 1},
		{name: "durable_ahead_higher_stored_epoch", stored: model.Watermark{Time: time.Unix(300, 0).UTC(), Epoch: 5}, t: 200, epoch: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := newMemCP()
			if err := cp.Save(context.Background(), key, tc.stored); err != nil {
				t.Fatal(err)
			}
			m := newStateCap()
			r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
			r.commit(coordinate.WithEpoch(context.Background(), tc.epoch), key, time.Unix(tc.t, 0).UTC(), tc.cursor, tc.epoch)
			if len(m.kinds) != 0 {
				t.Fatalf("benign stale write must not count, kinds=%v", m.kinds)
			}
			if r.Degraded() || len(m.degradeCalls()) != 0 {
				t.Fatalf("benign stale write must not degrade, degraded=%v calls=%v", r.Degraded(), m.degradeCalls())
			}
			if since, _ := r.Since(context.Background()); !since.Time.Equal(tc.stored.Time) || since.Cursor != tc.stored.Cursor {
				t.Fatalf("in-memory frontier=%+v want re-synced to durable %+v", since, tc.stored)
			}
		})
	}
}

// [#367] AC4: once the fence lifts (the loop writes under an epoch at or above the stored one), the
// successful commit clears the fence degrade and zeroes the gauge on the SAME {loop, reason} series.
func TestFenceDegradeClearedBySuccessfulCommit(t *testing.T) {
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	if err := cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 5}); err != nil {
		t.Fatal(err)
	}
	m := newStateCap()
	r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	r.commit(coordinate.WithEpoch(context.Background(), 1), key, time.Unix(200, 0).UTC(), "", 1)
	if !r.Degraded() {
		t.Fatal("precondition: the fenced forward write must degrade the loop")
	}
	r.commit(coordinate.WithEpoch(context.Background(), 5), key, time.Unix(200, 0).UTC(), "", 5)
	if r.Degraded() {
		t.Fatal("a successful commit after the fence must clear the degraded state")
	}
	want := []degradeCall{{loop: "analytics", reason: "checkpoint fenced", on: true}, {loop: "analytics", reason: "checkpoint fenced", on: false}}
	if calls := m.degradeCalls(); len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("loop_degraded calls=%v want %v", calls, want)
	}
}
