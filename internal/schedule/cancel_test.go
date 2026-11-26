// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// hookCP runs onSave / onLoad (when set) in place of the embedded memCP's Save / Load, so a test can
// cancel the leader context mid-call and return what a ctx-honouring backend would.
type hookCP struct {
	*memCP
	onSave func(ctx context.Context, k model.CheckpointKey, w model.Watermark) error
	onLoad func(ctx context.Context, k model.CheckpointKey) (model.Watermark, error)
}

func (c *hookCP) Save(ctx context.Context, k model.CheckpointKey, w model.Watermark) error {
	if c.onSave != nil {
		return c.onSave(ctx, k, w)
	}
	return c.memCP.Save(ctx, k, w)
}
func (c *hookCP) Load(ctx context.Context, k model.CheckpointKey) (model.Watermark, error) {
	if c.onLoad != nil {
		return c.onLoad(ctx, k)
	}
	return c.memCP.Load(ctx, k)
}

// loopFunc adapts a Collect func to source.Loop.
type loopFunc struct {
	key     model.CheckpointKey
	collect func(context.Context, model.Watermark) (model.Batch, error)
}

func (l loopFunc) Key() model.CheckpointKey { return l.key }
func (l loopFunc) Cadence() time.Duration   { return time.Minute }
func (l loopFunc) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	return l.collect(ctx, since)
}

// failureWarnings are the WARN/ERROR lines a genuine emit, save or collect failure produces; a clean
// shutdown or leadership loss must produce none of them.
var failureWarnings = []string{"emit failed after retries", "checkpoint save failed", "collect failed",
	"checkpoint load failed", "loop degraded", "forward-write fenced"}

func assertNoFailureLogs(t *testing.T, out string) {
	t.Helper()
	for _, w := range failureWarnings {
		if strings.Contains(out, w) {
			t.Fatalf("cancelled context logged a failure (%q):\n%s", w, out)
		}
	}
}

// [#368] When the leader context is cancelled (SIGTERM, rollout, lease flap) during an emit, a save, a
// load or a collect, the error that comes back is the cancellation, not a failure. Counting it fed the
// critical emit-failing alert (retryable_exhausted, checkpoint_save) with false positives, and on an
// in-process re-campaign the increment stays visible. None of these may count, log a failure, bump
// saveFails or degrade the loop.
func TestCancelNotCountedAsFailure(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	guard := func() *source.Guard { return source.NewGuard(source.GuardConfig{}) }

	t.Run("emit_metrics", func(t *testing.T) {
		buf := captureLogs(t)
		ctx, cancel := context.WithCancel(leaderCtx())
		defer cancel()
		em := emitterFunc(func(ctx context.Context, _ model.Batch) error {
			cancel()                                    // shutdown arrives mid-emit
			return &emit.RetryableError{Err: ctx.Err()} // what otlp.post returns on a cancelled ctx
		})
		m := newStateCap()
		cp := newMemCP()
		r := NewLoopRunner(fakeLoop{key: key}, em, cp, guard(), 4, 1, m)
		r.ProcessBatch(ctx, batchAt(key, 60))
		if len(m.kinds) != 0 {
			t.Fatalf("clean shutdown counted as an emit failure: %v", m.kinds)
		}
		assertNoFailureLogs(t, buf.String())
		if got, _ := cp.Load(context.Background(), key); !got.Time.IsZero() {
			t.Fatalf("no advance under a cancelled context, durable=%v", got.Time)
		}
	})

	t.Run("emit_metrics_interior_bucket", func(t *testing.T) {
		buf := captureLogs(t)
		ctx, cancel := context.WithCancel(leaderCtx())
		defer cancel()
		em := emitterFunc(func(ctx context.Context, b model.Batch) error {
			if b.Samples[0].Timestamp.Unix() == 60 {
				return nil // bucket 1 lands; shutdown arrives during bucket 2
			}
			cancel()
			return &emit.RetryableError{Err: ctx.Err()}
		})
		m := newStateCap()
		cp := newMemCP()
		r := NewLoopRunner(fakeLoop{key: key}, em, cp, guard(), 4, 1, m)
		r.ProcessBatch(ctx, batchAt(key, 60, 120))
		if len(m.kinds) != 0 {
			t.Fatalf("clean shutdown counted as an emit failure: %v", m.kinds)
		}
		assertNoFailureLogs(t, buf.String())
		if got, _ := cp.Load(context.Background(), key); !got.Time.IsZero() {
			t.Fatalf("the interior commit must stay a no-op under a cancelled context, durable=%v", got.Time)
		}
	})

	t.Run("emit_logs", func(t *testing.T) {
		buf := captureLogs(t)
		ctx, cancel := context.WithCancel(leaderCtx())
		defer cancel()
		em := emitterFunc(func(ctx context.Context, _ model.Batch) error {
			cancel()
			return &emit.RetryableError{Err: ctx.Err()}
		})
		m := newStateCap()
		r := NewLoopRunner(fakeLoop{key: key}, em, newMemCP(), guard(), 4, 1, m)
		r.ProcessBatch(ctx, model.Batch{Key: key, Logs: []model.LogRecord{{Timestamp: time.Unix(60, 0)}},
			Watermark: model.Watermark{Time: time.Unix(60, 0).UTC(), Cursor: "c"}})
		if len(m.kinds) != 0 {
			t.Fatalf("clean shutdown counted as a logs emit failure: %v", m.kinds)
		}
		assertNoFailureLogs(t, buf.String())
	})

	t.Run("commit_save_error", func(t *testing.T) {
		buf := captureLogs(t)
		ctx, cancel := context.WithCancel(leaderCtx())
		defer cancel()
		cp := &hookCP{memCP: newMemCP()}
		cp.onSave = func(ctx context.Context, _ model.CheckpointKey, _ model.Watermark) error {
			cancel() // the backend sees the cancellation mid-request
			return ctx.Err()
		}
		m := newStateCap()
		r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, guard(), 4, 1, m)
		r.commit(ctx, key, time.Unix(60, 0).UTC(), "", 1)
		if len(m.kinds) != 0 {
			t.Fatalf("a save cut short by cancellation counted as a failure: %v", m.kinds)
		}
		if r.saveFails != 0 {
			t.Fatalf("saveFails=%d want 0 (a cancelled save is not a consecutive failure)", r.saveFails)
		}
		assertNoFailureLogs(t, buf.String())
	})

	t.Run("commit_fenced_after_cancel", func(t *testing.T) {
		// The new leader's higher epoch fences the demoted leader's last write; with leadership already
		// gone that is the expected hand-over, not a stuck loop to alert on and degrade.
		buf := captureLogs(t)
		ctx, cancel := context.WithCancel(leaderCtx())
		defer cancel()
		cp := &hookCP{memCP: newMemCP()}
		if err := cp.memCP.Save(context.Background(), key, model.Watermark{Time: time.Unix(30, 0).UTC(), Epoch: 5}); err != nil {
			t.Fatal(err)
		}
		cp.onSave = func(ctx context.Context, k model.CheckpointKey, w model.Watermark) error {
			cancel()
			return cp.memCP.Save(ctx, k, w) // ctx-ignoring backend: returns ErrStaleWrite (epoch 1 < 5)
		}
		m := newStateCap()
		r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, guard(), 4, 1, m)
		r.commit(ctx, key, time.Unix(60, 0).UTC(), "", 1)
		if len(m.kinds) != 0 || r.Degraded() {
			t.Fatalf("a fence after leadership loss counted or degraded: kinds=%v degraded=%v", m.kinds, r.Degraded())
		}
		if got, _ := cp.Load(context.Background(), key); got.Time.Unix() != 30 {
			t.Fatalf("durable=%d want 30 (fence still holds)", got.Time.Unix())
		}
		assertNoFailureLogs(t, buf.String())
	})

	t.Run("collect", func(t *testing.T) {
		buf := captureLogs(t)
		ctx, cancel := context.WithCancel(leaderCtx())
		defer cancel()
		loop := loopFunc{key: key, collect: func(ctx context.Context, _ model.Watermark) (model.Batch, error) {
			cancel()
			return model.Batch{}, ctx.Err()
		}}
		m := newStateCap()
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), guard(), 4, 1, m)
		NewScheduler(nil, m).runOnce(ctx, LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
		if len(m.kinds) != 0 {
			t.Fatalf("a Collect cut short by cancellation counted as a failure: %v", m.kinds)
		}
		assertNoFailureLogs(t, buf.String())
	})

	t.Run("checkpoint_load", func(t *testing.T) {
		buf := captureLogs(t)
		ctx, cancel := context.WithCancel(leaderCtx())
		defer cancel()
		cp := &hookCP{memCP: newMemCP()}
		cp.onLoad = func(ctx context.Context, _ model.CheckpointKey) (model.Watermark, error) {
			cancel()
			return model.Watermark{}, ctx.Err()
		}
		loop := &collectStub{key: key, out: batchAt(key, 1060)}
		m := newStateCap()
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, guard(), 4, 1, m)
		NewScheduler(nil, m).runOnce(ctx, LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
		if len(m.kinds) != 0 {
			t.Fatalf("a checkpoint Load cut short by cancellation counted as a failure: %v", m.kinds)
		}
		assertNoFailureLogs(t, buf.String())
	})
}

// [#368] AC4: the same failures under a LIVE leader context are still counted, so the cancellation
// guard cannot hide a real outage.
func TestLiveFailureStillCounted(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	guard := func() *source.Guard { return source.NewGuard(source.GuardConfig{}) }
	retryable := emitterFunc(func(context.Context, model.Batch) error {
		return &emit.RetryableError{Err: errors.New("upstream 503")}
	})

	t.Run("emit_metrics", func(t *testing.T) {
		m := newStateCap()
		NewLoopRunner(fakeLoop{key: key}, retryable, newMemCP(), guard(), 4, 1, m).ProcessBatch(leaderCtx(), batchAt(key, 60))
		if m.count("retryable_exhausted") != 1 {
			t.Fatalf("live retryable emit failure must count, kinds=%v", m.kinds)
		}
	})
	t.Run("emit_logs", func(t *testing.T) {
		m := newStateCap()
		NewLoopRunner(fakeLoop{key: key}, retryable, newMemCP(), guard(), 4, 1, m).ProcessBatch(leaderCtx(),
			model.Batch{Key: key, Logs: []model.LogRecord{{Timestamp: time.Unix(60, 0)}}, Watermark: model.Watermark{Time: time.Unix(60, 0).UTC()}})
		if m.count("retryable_exhausted") != 1 {
			t.Fatalf("live retryable logs emit failure must count, kinds=%v", m.kinds)
		}
	})
	t.Run("commit_save_error", func(t *testing.T) {
		cp := &hookCP{memCP: newMemCP(), onSave: func(context.Context, model.CheckpointKey, model.Watermark) error {
			return errors.New("configmap unavailable")
		}}
		m := newStateCap()
		r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, guard(), 4, 1, m)
		r.commit(leaderCtx(), key, time.Unix(60, 0).UTC(), "", 1)
		if m.count("checkpoint_save") != 1 || r.saveFails != 1 {
			t.Fatalf("live save failure must count, kinds=%v saveFails=%d", m.kinds, r.saveFails)
		}
	})
	t.Run("collect", func(t *testing.T) {
		loop := &failCollectStub{key: key}
		m := newStateCap()
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), guard(), 4, 1, m)
		NewScheduler(nil, m).runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
		if m.count("collect") != 1 {
			t.Fatalf("live collect failure must count, kinds=%v", m.kinds)
		}
	})
	t.Run("checkpoint_load", func(t *testing.T) {
		cp := &hookCP{memCP: newMemCP(), onLoad: func(context.Context, model.CheckpointKey) (model.Watermark, error) {
			return model.Watermark{}, errors.New("configmap unavailable")
		}}
		loop := &collectStub{key: key, out: batchAt(key, 1060)}
		m := newStateCap()
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, guard(), 4, 1, m)
		NewScheduler(nil, m).runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}, time.Unix(1100, 0).UTC())
		if m.count("checkpoint_load") != 1 {
			t.Fatalf("live checkpoint load failure must count, kinds=%v", m.kinds)
		}
	})
}

var _ checkpoint.Checkpointer = (*hookCP)(nil)
