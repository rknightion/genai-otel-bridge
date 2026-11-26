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

// depthMetrics records every QueueDepth value in order (the worker goroutine records, the test reads).
type depthMetrics struct {
	NoopMetrics
	mu     sync.Mutex
	depths []int
}

func (m *depthMetrics) QueueDepth(_ string, d int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.depths = append(m.depths, d)
}

func (m *depthMetrics) snapshot() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.depths...)
}

// TestQueueDepthReflectsInFlightBatch [#372]: collection is single-flight with the emit, so at most one
// batch is ever queued or in flight per loop. queue_depth therefore reports 1 while a batch is being
// emitted (a slow or failing emit holds it at 1, so the backpressure alert can fire) and 0 once the
// batch is handled. Enqueue itself records nothing: its post-send read raced the worker.
func TestQueueDepthReflectsInFlightBatch(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	em := emitterFunc(func(context.Context, model.Batch) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // a gateway outage: the emit is stuck in its retry budget
		return nil
	})
	m := &depthMetrics{}
	r := NewLoopRunner(fakeLoop{key: key}, em, newMemCP(), source.NewGuard(source.GuardConfig{}), 256, 1, m)
	ctx, cancel := context.WithCancel(leaderCtx())
	defer cancel()

	if err := r.Enqueue(ctx, batchAt(key, 60)); err != nil {
		t.Fatal(err)
	}
	if got := m.snapshot(); len(got) != 0 {
		t.Fatalf("Enqueue recorded queue depth %v; only the worker may record it", got)
	}

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never began the emit")
	}
	if got := m.snapshot(); len(got) == 0 || got[len(got)-1] != 1 {
		t.Fatalf("emit blocked, yet the last recorded queue depth is not 1: %v", got)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := m.snapshot()
		if len(got) > 0 && got[len(got)-1] == 0 && !r.Busy() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("emit returned, yet the last recorded queue depth is not 0: %v", got)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if got := m.snapshot(); got[len(got)-1] != 0 {
		t.Fatalf("queue depth after the batch was handled: %v, want last 0", got)
	}
}
