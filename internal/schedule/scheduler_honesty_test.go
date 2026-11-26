// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// tsRecorder records every emitted metric bucket and log batch; safe for the worker goroutine.
type tsRecorder struct {
	mu    sync.Mutex
	ts    []int64
	logsN int
}

func (e *tsRecorder) Emit(_ context.Context, b model.Batch) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(b.Logs) > 0 {
		e.logsN += len(b.Logs)
		return nil
	}
	e.ts = append(e.ts, b.Samples[0].Timestamp.Unix())
	return nil
}

func (e *tsRecorder) emitted() ([]int64, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int64(nil), e.ts...), e.logsN
}

// TestSchedulerRunResetsBeforeLeading [#399 / CP-R3]: a batch left in a runner's queue by an earlier
// leadership must never be emitted under the next one. Scheduler.Run resets every runner before it
// starts the new leadership's goroutines, so only the freshly collected window is emitted.
func TestSchedulerRunResetsBeforeLeading(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	const staleTS, freshTS = 600, 1200
	em := &tsRecorder{}
	loop := &collectStub{key: key, out: batchAt(key, freshTS)}
	r := NewLoopRunner(loop, em, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	// A previous leadership collected a batch and lost the lease before its worker took it.
	if err := r.Enqueue(leaderCtx(), batchAt(key, staleTS)); err != nil {
		t.Fatal(err)
	}
	sch := NewScheduler([]LoopSpec{{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}}, NoopMetrics{})
	runLeadership(t, sch, func() bool { ts, _ := em.emitted(); return len(ts) > 0 })
	ts, _ := em.emitted()
	for _, v := range ts {
		if v == staleTS {
			t.Fatalf("a batch queued under the previous leadership was emitted under the new one: %v", ts)
		}
	}
	if len(ts) != 1 || ts[0] != freshTS {
		t.Fatalf("emitted=%v want only the fresh window %d", ts, freshTS)
	}
}

// TestRunOnceQuota [#399]: a Collect that returns ErrQuotaExceeded counts
// samples_skipped{reason="quota_exceeded"}, enqueues nothing and leaves the checkpoint unchanged.
func TestRunOnceQuota(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	durable := model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 1}
	if err := cp.Save(context.Background(), key, durable); err != nil {
		t.Fatal(err)
	}
	loop := &quotaThenOKLoop{key: key, quota: true}
	m := &capMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	out := NewScheduler(nil, m).tick(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
	if !out.quota || out.more {
		t.Fatalf("tick outcome=%+v want quota and no catch-up", out)
	}
	if got := m.skipped["quota_exceeded"]; got != 1 {
		t.Fatalf("samples_skipped{quota_exceeded}=%d want 1", got)
	}
	if r.Busy() || len(r.q) != 0 {
		t.Fatalf("a quota tick must enqueue nothing: busy=%v queued=%d", r.Busy(), len(r.q))
	}
	if got, _ := cp.Load(context.Background(), key); got != durable {
		t.Fatalf("a quota tick advanced the checkpoint: %+v want %+v", got, durable)
	}
}

// TestRunOnceCheckpointLoadError [#399]: when the checkpoint cannot be read the tick counts
// emit_errors{kind="checkpoint_load"} and never calls Collect (it has no lower bound to pull from).
func TestRunOnceCheckpointLoadError(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	collects := 0
	loop := loopFunc{key: key, collect: func(context.Context, model.Watermark) (model.Batch, error) {
		collects++
		return batchAt(key, 60), nil
	}}
	m := newErrCap()
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, loadErrorCP{}, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	NewScheduler(nil, m).tick(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
	if got := m.count("checkpoint_load"); got != 1 {
		t.Fatalf("emit_errors{checkpoint_load}=%d want 1", got)
	}
	if collects != 0 {
		t.Fatalf("Collect ran %d times after a checkpoint load failure, want 0", collects)
	}
}

// TestProcessBatchCancelledLeadershipEmitsNothing [#399 / CP-R3b]: ProcessBatch under an already
// cancelled leader context emits nothing on either plane, whoever the caller is (the worker's own
// re-check is a separate guard).
func TestProcessBatchCancelledLeadershipEmitsNothing(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	em := &tsRecorder{}
	r := NewLoopRunner(fakeLoop{key: key}, em, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	ctx, cancel := context.WithCancel(leaderCtx())
	cancel()
	b := batchAt(key, 60, 120)
	b.Logs = []model.LogRecord{{Timestamp: time.Unix(60, 0).UTC()}}
	r.ProcessBatch(ctx, b)
	if ts, logs := em.emitted(); len(ts) != 0 || logs != 0 {
		t.Fatalf("emitted under a cancelled leadership: buckets=%v logs=%d", ts, logs)
	}
}
