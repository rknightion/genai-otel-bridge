// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// runLeadership runs one Scheduler.Run leadership over a single spec, waits until ready() holds (polled
// against a deadline), then cancels the leader context and waits for Run to return (goroutines joined).
func runLeadership(t *testing.T, sch *Scheduler, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(leaderCtx())
	defer cancel()
	done := make(chan struct{})
	go func() { sch.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("leadership never reached the expected state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Scheduler.Run did not return after the leader context was cancelled")
	}
}

// [#369] loop_degraded was zeroed only in Reset, at the NEXT election on the same process, so a leader
// demoted while a loop was degraded kept exporting 1 for that {loop, reason} series for as long as it
// lived, while the new leader was healthy. Run must zero it once leadership ends.
func TestDegradedGaugeZeroedAtDemotion(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}

	t.Run("degraded_runner_zeroed", func(t *testing.T) {
		loop := &granularityStub{key: key} // the first, immediate tick degrades the loop
		m := newStateCap()
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, m)
		sch := NewScheduler([]LoopSpec{{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}}, m)
		runLeadership(t, sch, r.Degraded)
		calls := m.degradeCalls()
		want := degradeCall{loop: "analytics", reason: "granularity_unexpected", on: false}
		if len(calls) == 0 || calls[len(calls)-1] != want {
			t.Fatalf("loop_degraded calls=%v; want the last to be %v (zeroed at demotion)", calls, want)
		}
		if len(calls) != 2 {
			t.Fatalf("loop_degraded calls=%v; want exactly set-then-zero", calls)
		}
	})

	t.Run("healthy_runner_untouched", func(t *testing.T) {
		loop := &failCollectStub{key: key} // a counted collect failure, not a degrade
		m := newStateCap()
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, m)
		sch := NewScheduler([]LoopSpec{{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}}, m)
		runLeadership(t, sch, func() bool { return m.count("collect") > 0 })
		if calls := m.degradeCalls(); len(calls) != 0 {
			t.Fatalf("a runner that was not degraded must produce no LoopDegraded call at demotion, got %v", calls)
		}
	})
}
