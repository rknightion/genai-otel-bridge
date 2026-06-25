// SPDX-License-Identifier: AGPL-3.0-only

package schedule

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	cpfile "github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// [ext-review-2] The watermark must not advance once leadership is lost — even when the checkpointer
// ignores ctx. Here the REAL file checkpointer (Save ignores ctx) is paired with an emitter that
// cancels the leaderCtx then returns nil; without the commit-time ctx re-check the file Save would
// run and advance the watermark.
func TestNoAdvanceWhenLeadershipLostDuringEmit(t *testing.T) {
	cp, err := cpfile.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	ctx, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), 1))
	em := emitterFunc(func(context.Context, model.Batch) error {
		cancel()
		return nil
	})
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})

	r.ProcessBatch(ctx, batchAt(key, 60))

	got, err := cp.Load(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Time.IsZero() {
		t.Fatalf("watermark advanced after leadership loss: got %v", got.Time)
	}
}

// [ext-review-2] Same invariant against an in-memory (ctx-ignoring) checkpointer: leadership is
// cancelled while Emit is blocked, then Emit returns nil; the commit must be skipped.
func TestNoCommitAfterLeadershipCancelledDuringEmit(t *testing.T) {
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	started := make(chan struct{})
	release := make(chan struct{})
	em := emitterFunc(func(_ context.Context, _ model.Batch) error {
		close(started)
		<-release
		return nil
	})
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	leaderCtx, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), 1))
	done := make(chan struct{})
	go func() {
		r.ProcessBatch(leaderCtx, batchAt(key, 60))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("emit did not start")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ProcessBatch did not return")
	}
	got, _ := cp.Load(context.Background(), key)
	if !got.Time.IsZero() {
		t.Fatalf("watermark advanced after leaderCtx cancellation: got %s", got.Time)
	}
}
