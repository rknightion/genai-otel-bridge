// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// stampMetrics records every LastSuccess stamp in order.
type stampMetrics struct {
	NoopMetrics
	stamps []time.Time
}

func (m *stampMetrics) LastSuccess(_ LoopID, t time.Time) { m.stamps = append(m.stamps, t) }

// TestQuietWindowRefreshesLastSuccess [#371]: an empty window (no samples, no log records) still commits
// its watermark at until, and that fully handled window must stamp last_success at the committed time.
// Without the stamp a quiet source ages last_success exactly as if the poller were down.
func TestQuietWindowRefreshesLastSuccess(t *testing.T) {
	for _, loopName := range []string{"analytics", "runs"} {
		t.Run(loopName, func(t *testing.T) {
			key := model.CheckpointKey{SourceInstance: "s", Loop: loopName, OutputFingerprint: "fp"}
			cp := newMemCP()
			m := &stampMetrics{}
			r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
			var want []time.Time
			for i := int64(1); i <= 20; i++ {
				until := time.Unix(1_700_000_000+300*i, 0).UTC()
				want = append(want, until)
				r.ProcessBatch(leaderCtx(), model.Batch{Key: key, Watermark: model.Watermark{Time: until}})
			}
			if got, _ := cp.Load(context.Background(), key); !got.Time.Equal(want[len(want)-1]) {
				t.Fatalf("precondition: durable watermark=%v want %v", got.Time, want[len(want)-1])
			}
			if len(m.stamps) != len(want) {
				t.Fatalf("20 committed quiet windows produced %d last_success stamps, want 20", len(m.stamps))
			}
			for i := range want {
				if !m.stamps[i].Equal(want[i]) {
					t.Fatalf("stamp %d = %v, want the committed watermark %v", i, m.stamps[i], want[i])
				}
			}
		})
	}
}

// saveErrCP fails every Save with a non-stale error.
type saveErrCP struct{ *memCP }

func (saveErrCP) Save(context.Context, model.CheckpointKey, model.Watermark) error {
	return errors.New("injected save failure")
}

// TestQuietWindowNoStampWithoutCommit [#371]: the stamp follows the commit, so an empty window whose
// commit fails, is epoch-fenced, or is a benign stale write produces no last_success stamp.
func TestQuietWindowNoStampWithoutCommit(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "runs", OutputFingerprint: "fp"}
	until := time.Unix(1_700_000_300, 0).UTC()
	for _, tc := range []struct {
		name string
		cp   func() checkpoint.Checkpointer
	}{
		{"save_error", func() checkpoint.Checkpointer { return saveErrCP{newMemCP()} }},
		{"fenced", func() checkpoint.Checkpointer {
			cp := newMemCP()
			_ = cp.Save(context.Background(), key, model.Watermark{Time: until.Add(-time.Hour), Epoch: 9}) // higher epoch, behind: forward write fenced
			return cp
		}},
		{"stale_already_advanced", func() checkpoint.Checkpointer {
			cp := newMemCP()
			_ = cp.Save(context.Background(), key, model.Watermark{Time: until.Add(time.Hour), Epoch: 1})
			return cp
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &stampMetrics{}
			r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, tc.cp(), source.NewGuard(source.GuardConfig{}), 4, 1, m)
			r.ProcessBatch(leaderCtx(), model.Batch{Key: key, Watermark: model.Watermark{Time: until}})
			if len(m.stamps) != 0 {
				t.Fatalf("an empty window whose commit did not land stamped last_success: %v", m.stamps)
			}
		})
	}
}

// TestCursorOnlyStepAtZeroTimeNoStamp [#371]: a cursor-only step at the zero Time (the logs-export first
// window) commits its cursor but has no completed window, so it must not stamp an epoch-0 last_success.
func TestCursorOnlyStepAtZeroTimeNoStamp(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	cp := newMemCP()
	m := &stampMetrics{}
	r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	r.ProcessBatch(leaderCtx(), model.Batch{Key: key, Watermark: model.Watermark{Cursor: "created:job-1", Epoch: 1}})
	if got, _ := cp.Load(context.Background(), key); got.Cursor != "created:job-1" {
		t.Fatalf("precondition: cursor-only step must commit, durable=%+v", got)
	}
	if len(m.stamps) != 0 {
		t.Fatalf("cursor-only step at zero Time stamped last_success: %v", m.stamps)
	}
}
