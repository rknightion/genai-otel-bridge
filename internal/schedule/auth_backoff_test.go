// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// authMetrics counts EmitError reasons on top of capMetrics.
type authMetrics struct {
	capMetrics
	emitErrs map[string]int
}

func (m *authMetrics) EmitError(_ LoopID, reason string) {
	if m.emitErrs == nil {
		m.emitErrs = map[string]int{}
	}
	m.emitErrs[reason]++
}

// authThenOKLoop returns a source.ErrAuth-wrapped error while auth is set, a one-bucket batch otherwise.
type authThenOKLoop struct {
	key  model.CheckpointKey
	auth bool
	n    int // Collect calls
}

func (l *authThenOKLoop) Key() model.CheckpointKey { return l.key }
func (l *authThenOKLoop) Cadence() time.Duration   { return time.Minute }
func (l *authThenOKLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	l.n++
	if l.auth {
		return model.Batch{}, source.WrapAuth(401, fmt.Errorf("vendor: status 401"))
	}
	return batchAt(l.key, 1060), nil
}

// TestAuthStreakStretchesWait [#537]: consecutive auth (401/403) Collect failures double the wait up to
// DegradedBackoff, one successful Collect returns it to cadence, and every failed tick is still counted
// as a collect error (the source's auth_errors_total hook fires inside Collect, so it is not skipped).
func TestAuthStreakStretchesWait(t *testing.T) {
	const cad = time.Minute
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	durable := model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 1}
	if err := cp.Save(context.Background(), key, durable); err != nil {
		t.Fatal(err)
	}
	loop := &authThenOKLoop{key: key, auth: true}
	m := &authMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	sch := NewScheduler(nil, m)
	sp := LoopSpec{Runner: r, Loop: loop, Cadence: cad, MaxBackfill: time.Hour}
	now := time.Unix(1100, 0).UTC()
	var st tickState
	step := func() time.Duration {
		w, _ := st.next(r.Degraded(), sch.tick(leaderCtx(), sp, now), sp.MaxCatchupPerTick, sp.Cadence)
		return w
	}

	// AC1: the wait doubles per tick up to the cap.
	for i, want := range []time.Duration{2 * cad, 4 * cad, 8 * cad, DegradedBackoff, DegradedBackoff} {
		if got := step(); got != want {
			t.Fatalf("auth tick %d: wait=%v want %v", i+1, got, want)
		}
	}
	// AC3: every failed tick is counted; nothing advances or is enqueued.
	if got := m.emitErrs["collect"]; got != 5 {
		t.Fatalf("emit_errors{collect}=%d want 5 (one per failed auth tick)", got)
	}
	if got, _ := cp.Load(context.Background(), key); got != durable {
		t.Fatalf("an auth tick must not advance the watermark: durable=%+v want %+v", got, durable)
	}
	if r.Busy() || len(r.q) != 0 {
		t.Fatalf("an auth tick must enqueue nothing: busy=%v queued=%d", r.Busy(), len(r.q))
	}
	// A loop slower than DegradedBackoff keeps its cadence.
	if w, jit := (&tickState{}).next(false, tickOutcome{auth: true}, 1, time.Hour); w != time.Hour || !jit {
		t.Fatalf("slow loop with auth failure: got (%v, jit=%v) want (1h, true)", w, jit)
	}

	// AC2: one successful Collect returns the wait to the cadence.
	loop.auth = false
	if got := step(); got != cad {
		t.Fatalf("after a successful Collect: wait=%v want cadence %v", got, cad)
	}
	r.Reset()
	loop.auth = true
	if got := step(); got != 2*cad {
		t.Fatalf("new auth streak: wait=%v want %v", got, 2*cad)
	}
}
