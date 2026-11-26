// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// recMetrics records the honesty counters a logs-plane outcome must move.
type recMetrics struct {
	NoopMetrics
	mu        sync.Mutex
	errs      map[string]int // EmitError kind → count
	skipped   map[string]int // SamplesSkipped reason → total n
	guardDrop int
}

func newRecMetrics() *recMetrics {
	return &recMetrics{errs: map[string]int{}, skipped: map[string]int{}}
}
func (m *recMetrics) EmitError(_, kind string) { m.mu.Lock(); m.errs[kind]++; m.mu.Unlock() }
func (m *recMetrics) SamplesSkipped(_, reason string, n int) {
	m.mu.Lock()
	m.skipped[reason] += n
	m.mu.Unlock()
}
func (m *recMetrics) GuardDropped(_ string, n int) { m.mu.Lock(); m.guardDrop += n; m.mu.Unlock() }

// logsPlaneRun seeds a durable mid-window position, processes one logs batch that would step the cursor
// (and, for a final page, the Time) and returns what was persisted.
func logsPlaneRun(t *testing.T, logsErr error, guard *source.Guard, recs []model.LogRecord) (durable model.Watermark, em *fakeEmitter, m *recMetrics) {
	t.Helper()
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	cp := newMemCP()
	if err := cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(1000, 0).UTC(), Cursor: "win2:page1", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	em = &fakeEmitter{byTS: map[int64]error{}, logsErr: logsErr}
	m = newRecMetrics()
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, guard, 4, 1, m)
	r.ProcessBatch(leaderCtx(), model.Batch{Key: key, Logs: recs,
		Watermark: model.Watermark{Time: time.Unix(1000, 0).UTC(), Cursor: "win2:page2", Epoch: 1}})
	durable, _ = cp.Load(context.Background(), key)
	return durable, em, m
}

func allowedRecs(n int) []model.LogRecord {
	out := make([]model.LogRecord, n)
	for i := range out {
		out[i] = model.LogRecord{Timestamp: time.Unix(990, 0).UTC(), IndexedAttributes: map[string]string{"ai_model": "m"}}
	}
	return out
}

func allowModelGuard() *source.Guard {
	return source.NewGuard(source.GuardConfig{AllowLabelKeys: []string{"ai_model"}})
}

// TestProcessLogsRetryableDoesNotAdvance [#395]: a logs emit whose retry budget is exhausted must leave
// the durable Time and Cursor where they were (the step is re-done next tick) and count
// retryable_exhausted. Advancing the cursor here would lose the chunk silently.
func TestProcessLogsRetryableDoesNotAdvance(t *testing.T) {
	durable, _, m := logsPlaneRun(t, &emit.RetryableError{Status: 503}, allowModelGuard(), allowedRecs(3))
	if durable.Cursor != "win2:page1" || !durable.Time.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("retryable logs failure advanced the checkpoint: %+v (want cursor win2:page1)", durable)
	}
	if m.errs["retryable_exhausted"] != 1 {
		t.Fatalf("emit_errors{retryable_exhausted}=%d want 1", m.errs["retryable_exhausted"])
	}
}

// TestProcessLogsAdvancePastCommitsWithCountedGap [#395]: a known per-record reject (too old) skips the
// chunk with a counted gap and still commits the step, so the loop never stalls on it.
func TestProcessLogsAdvancePastCommitsWithCountedGap(t *testing.T) {
	durable, _, m := logsPlaneRun(t, &emit.RejectError{Reason: emit.ReasonTooOld, Status: 400, Msg: "too far behind"}, allowModelGuard(), allowedRecs(3))
	if durable.Cursor != "win2:page2" {
		t.Fatalf("advance-past logs reject did not commit the step: %+v (want cursor win2:page2)", durable)
	}
	if m.skipped["too_old"] != 3 {
		t.Fatalf("samples_skipped{too_old}=%d want 3 (one per skipped record)", m.skipped["too_old"])
	}
	if len(m.errs) != 0 {
		t.Fatalf("a counted skip is not an emit error: %v", m.errs)
	}
}

// TestProcessLogsAllDroppedStillCommits [#395]: a chunk whose every record the guard denies emits
// nothing but still commits the step (counted), so a fully-denied page cannot stall the loop.
func TestProcessLogsAllDroppedStillCommits(t *testing.T) {
	denied := []model.LogRecord{
		{Timestamp: time.Unix(990, 0).UTC(), IndexedAttributes: map[string]string{"not_allowed": "x"}},
		{Timestamp: time.Unix(991, 0).UTC(), IndexedAttributes: map[string]string{"not_allowed": "y"}},
	}
	durable, em, m := logsPlaneRun(t, nil, allowModelGuard(), denied)
	if em.emittedLogs != 0 {
		t.Fatalf("guard-denied records were emitted: %d", em.emittedLogs)
	}
	if m.guardDrop != 2 {
		t.Fatalf("guard_dropped=%d want 2", m.guardDrop)
	}
	if durable.Cursor != "win2:page2" {
		t.Fatalf("all-dropped chunk did not commit the step: %+v (want cursor win2:page2)", durable)
	}
}
