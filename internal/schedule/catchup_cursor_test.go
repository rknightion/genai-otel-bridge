// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// scriptLoop returns its scripted batches in order, then repeats the last one.
type scriptLoop struct {
	key   model.CheckpointKey
	out   []model.Batch
	calls int
}

func (l *scriptLoop) Key() model.CheckpointKey { return l.key }
func (l *scriptLoop) Cadence() time.Duration   { return time.Minute }
func (l *scriptLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	b := l.out[min(l.calls, len(l.out)-1)]
	l.calls++
	return b, nil
}

// driveTicks runs n real ticks through the same tickState.next the tick loop uses, handling each
// enqueued batch before the next tick (as the worker would), and returns the planned waits.
func driveTicks(t *testing.T, loop *scriptLoop, maxCatchup, n int) []time.Duration {
	t.Helper()
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	sch := NewScheduler(nil, NoopMetrics{})
	sp := LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, Window: 0, MaxCatchupPerTick: maxCatchup}
	now := time.Unix(1_700_000_000, 0).UTC()
	var st tickState
	var waits []time.Duration
	for i := 0; i < n; i++ {
		out := sch.tick(leaderCtx(), sp, now)
		select {
		case q := <-r.q:
			r.ProcessBatch(leaderCtx(), q.batch)
		default:
		}
		w, _ := st.next(r.Degraded(), out, sp.MaxCatchupPerTick, sp.Cadence)
		waits = append(waits, w)
	}
	return waits
}

func logsBatch(key model.CheckpointKey, t time.Time, cursor string) model.Batch {
	return model.Batch{Key: key, Logs: []model.LogRecord{{Timestamp: t}}, Watermark: model.Watermark{Time: t, Cursor: cursor}}
}

// TestCursorBearingLoopAccelerates [#374]: a Window 0 loop that is mid-window on a cursor (its batch
// carries a non-empty cursor and emitted records) uses the catch-up interval, so a page-per-tick logs
// loop drains a busy window at the limiter's pace rather than one page per cadence. The window's last
// page (empty cursor) returns to the cadence, and max_catchup_per_tick still caps the burst.
func TestCursorBearingLoopAccelerates(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "ls", Loop: "runs", OutputFingerprint: "fp"}
	t0 := time.Unix(1_699_999_000, 0).UTC()
	script := func() *scriptLoop {
		return &scriptLoop{key: key, out: []model.Batch{
			logsBatch(key, t0, "page-1"),
			logsBatch(key, t0, "page-2"),
			logsBatch(key, t0, "page-3"),
			logsBatch(key, t0.Add(time.Hour), ""), // last page: window complete, cursor cleared
		}}
	}
	cad := time.Minute
	if got, want := driveTicks(t, script(), 10, 5), []time.Duration{catchupInterval, catchupInterval, catchupInterval, cad, cad}; !equalWaits(got, want) {
		t.Fatalf("waits=%v want %v (three cursor pages accelerate, the last page returns to cadence)", got, want)
	}
	// The burst is capped by max_catchup_per_tick: 2 ⇒ one accelerated tick, then a cadence breather.
	if got, want := driveTicks(t, script(), 2, 4), []time.Duration{catchupInterval, cad, catchupInterval, cad}; !equalWaits(got, want) {
		t.Fatalf("waits=%v want %v (max_catchup_per_tick=2 caps each burst)", got, want)
	}
	// max_catchup_per_tick 1 (the v1 behaviour) never accelerates.
	if got, want := driveTicks(t, script(), 1, 4), []time.Duration{cad, cad, cad, cad}; !equalWaits(got, want) {
		t.Fatalf("waits=%v want %v (max_catchup_per_tick=1 never accelerates)", got, want)
	}
}

// TestSnapshotLoopNeverAccelerates [#374]: a Window 0 loop without a cursor (a snapshot loop) never gets
// the catch-up interval, nor does a cursor-only lifecycle step (no records emitted), nor a busy loop
// whose emit outlasts its cadence.
func TestSnapshotLoopNeverAccelerates(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "ls", Loop: "sessions", OutputFingerprint: "fp"}
	t0 := time.Unix(1_699_999_000, 0).UTC()
	cad := time.Minute
	for name, b := range map[string]model.Batch{
		"snapshot_samples": batchAt(key, t0.Unix()),
		"logs_no_cursor":   logsBatch(key, t0, ""),
		"cursor_only_step": {Key: key, Watermark: model.Watermark{Time: t0, Cursor: "created:job-1"}},
	} {
		loop := &scriptLoop{key: key, out: []model.Batch{b}}
		if got, want := driveTicks(t, loop, 10, 3), []time.Duration{cad, cad, cad}; !equalWaits(got, want) {
			t.Fatalf("%s: waits=%v want %v", name, got, want)
		}
	}
	// Slow emit: a busy Window 0 loop skips the tick and must not accelerate, cursor or not.
	loop := &scriptLoop{key: key, out: []model.Batch{logsBatch(key, t0, "page-1")}}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	if err := r.Enqueue(leaderCtx(), logsBatch(key, t0, "page-0")); err != nil { // in flight; no worker drains it
		t.Fatal(err)
	}
	sp := LoopSpec{Runner: r, Loop: loop, Cadence: cad, MaxCatchupPerTick: 10}
	var st tickState
	if w, _ := st.next(false, NewScheduler(nil, NoopMetrics{}).tick(leaderCtx(), sp, t0), 10, cad); w != cad {
		t.Fatalf("busy Window 0 loop: wait=%v want cadence %v", w, cad)
	}
}

func equalWaits(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
