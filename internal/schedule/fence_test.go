// SPDX-License-Identifier: AGPL-3.0-only

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
func (m *errCapMetrics) EmitError(_, kind string) {
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
}
