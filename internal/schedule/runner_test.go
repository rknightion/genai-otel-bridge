// SPDX-License-Identifier: AGPL-3.0-only

package schedule

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// --- test doubles ---
type memCP struct {
	mu sync.Mutex
	w  map[string]model.Watermark
}

func newMemCP() *memCP { return &memCP{w: map[string]model.Watermark{}} }
func (m *memCP) Load(_ context.Context, k model.CheckpointKey) (model.Watermark, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.w[k.String()], nil
}
func (m *memCP) Save(_ context.Context, k model.CheckpointKey, w model.Watermark) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkpoint.CheckMonotonic(m.w[k.String()], w); err != nil {
		return err
	}
	m.w[k.String()] = w
	return nil
}

type fakeEmitter struct {
	mu          sync.Mutex
	byTS        map[int64]error // keyed by bucket unix seconds
	emitted     []int64
	emittedLogs int   // total log records emitted across calls
	logsErr     error // injected error for a logs Emit (nil ⇒ success)
}

func (f *fakeEmitter) Emit(_ context.Context, b model.Batch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(b.Logs) > 0 {
		if f.logsErr != nil {
			return f.logsErr
		}
		f.emittedLogs += len(b.Logs)
		return nil
	}
	ts := b.Samples[0].Timestamp.Unix()
	if err := f.byTS[ts]; err != nil {
		return err
	}
	f.emitted = append(f.emitted, ts)
	return nil
}

type fakeLoop struct{ key model.CheckpointKey }

func (l fakeLoop) Key() model.CheckpointKey { return l.key }
func (l fakeLoop) Cadence() time.Duration   { return time.Minute }
func (l fakeLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, nil
}

// TestCommitPersistsCursorAtSameTime guards the runner half of the cursor-progress relaxation: a commit
// that advances the Cursor at an UNCHANGED Time (a logs-export job step within a window) must update the
// in-memory frontier, so the next Since() returns the new cursor and the loop's state machine can step.
func TestCommitPersistsCursorAtSameTime(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	cp := newMemCP()
	r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, &NoopMetrics{})
	ctx := leaderCtx()
	t0 := time.Unix(1000, 0).UTC()
	r.commit(ctx, key, t0, "phaseA", 1)
	r.commit(ctx, key, t0, "phaseB", 1) // same Time, cursor advances
	got, err := r.Since(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor != "phaseB" {
		t.Fatalf("cursor=%q want phaseB (same-Time cursor advance must update the in-memory frontier)", got.Cursor)
	}
	if !got.Time.Equal(t0) {
		t.Fatalf("time=%v want unchanged %v", got.Time, t0)
	}
	// durable store must also carry the advanced cursor.
	if stored, _ := cp.Load(ctx, key); stored.Cursor != "phaseB" {
		t.Fatalf("durable cursor=%q want phaseB", stored.Cursor)
	}
}

func batchAt(key model.CheckpointKey, secs ...int64) model.Batch {
	var ss []model.Sample
	var last int64
	for _, s := range secs {
		ss = append(ss, model.Sample{Name: "portkey_api_requests", Kind: model.Gauge, Value: 1, Timestamp: time.Unix(s, 0).UTC()})
		last = s
	}
	return model.Batch{Key: key, Samples: ss, Watermark: model.Watermark{Time: time.Unix(last, 0).UTC()}}
}

func newRunner(em emit.Emitter, cp checkpoint.Checkpointer) (*LoopRunner, model.CheckpointKey) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	return r, key
}

func leaderCtx() context.Context { return coordinate.WithEpoch(context.Background(), 1) }

func TestProcessAdvancesAcrossBucketsAscending(t *testing.T) {
	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	r, key := newRunner(em, cp)
	r.ProcessBatch(leaderCtx(), batchAt(key, 60, 120, 180))
	if got, _ := cp.Load(context.Background(), key); got.Time.Unix() != 180 {
		t.Fatalf("watermark=%d want 180", got.Time.Unix())
	}
	if len(em.emitted) != 3 {
		t.Fatalf("emitted=%v", em.emitted)
	}
}

func TestProcessAdvancesPastDuplicateTimestamp(t *testing.T) {
	em := &fakeEmitter{byTS: map[int64]error{120: &emit.RejectError{Reason: emit.ReasonDuplicateTimestamp}}}
	cp := newMemCP()
	r, key := newRunner(em, cp)
	r.ProcessBatch(leaderCtx(), batchAt(key, 60, 120, 180))
	// bucket 120 skipped-with-gap, but the loop progresses to 180 and emits 60 & 180.
	if got, _ := cp.Load(context.Background(), key); got.Time.Unix() != 180 {
		t.Fatalf("watermark=%d want 180 (advance-past gap)", got.Time.Unix())
	}
	if len(em.emitted) != 2 {
		t.Fatalf("emitted=%v want [60 180]", em.emitted)
	}
}

func TestProcessStopsWithoutAdvanceOnRetryable(t *testing.T) {
	em := &fakeEmitter{byTS: map[int64]error{120: &emit.RetryableError{Status: 500}}}
	cp := newMemCP()
	r, key := newRunner(em, cp)
	r.ProcessBatch(leaderCtx(), batchAt(key, 60, 120, 180))
	if got, _ := cp.Load(context.Background(), key); got.Time.Unix() != 60 {
		t.Fatalf("watermark=%d want 60 (stuck, re-pull next tick)", got.Time.Unix())
	}
	if len(em.emitted) != 1 {
		t.Fatalf("emitted=%v want [60]", em.emitted)
	}
}

func TestProcessStopsWithoutAdvanceOnBadEncoding(t *testing.T) {
	em := &fakeEmitter{byTS: map[int64]error{120: &emit.RejectError{Reason: emit.ReasonBadEncoding}}}
	cp := newMemCP()
	r, key := newRunner(em, cp)
	r.ProcessBatch(leaderCtx(), batchAt(key, 60, 120, 180))
	if got, _ := cp.Load(context.Background(), key); got.Time.Unix() != 60 {
		t.Fatalf("watermark=%d want 60 (bad-encoding halts loudly, no silent advance)", got.Time.Unix())
	}
	// [#93] The degrade set by the terminal reject on bucket 120 must SURVIVE the interior commit of
	// bucket 60 — otherwise the scheduler re-ticks at cadence instead of backing off on this very tick.
	if !r.Degraded() {
		t.Fatal("[#93] loop must stay degraded after a terminal reject on a non-first bucket (interior commit must not clear it)")
	}
}

// TestAuditTerminalHaltOnLaterBucketStaysDegraded is the [#93] regression guard: with the reject on the
// SECOND bucket, bucket 1 commits (interior watermark advances to 60) AND the loop stays degraded, so
// tickPlan backs off to DegradedBackoff rather than re-collecting at cadence. Asserts both legs together.
func TestAuditTerminalHaltOnLaterBucketStaysDegraded(t *testing.T) {
	em := &fakeEmitter{byTS: map[int64]error{120: &emit.RejectError{Reason: emit.ReasonUnknown, Status: 400, Msg: "bad"}}}
	cp := newMemCP()
	r, key := newRunner(em, cp)
	r.ProcessBatch(leaderCtx(), batchAt(key, 60, 120, 180))
	if got, _ := cp.Load(context.Background(), key); got.Time.Unix() != 60 {
		t.Fatalf("interior watermark=%d want 60 (bucket 1 committed, no advance past the reject)", got.Time.Unix())
	}
	if !r.Degraded() {
		t.Fatal("[#93] degrade must stick through the interior commit so the scheduler backs off")
	}
	// tickPlan must now choose the slow backoff (degraded precedence), proving the scheduler stops hammering.
	if w, _, _ := tickPlan(r.Degraded(), false, 0, 4, time.Minute); w != DegradedBackoff {
		t.Fatalf("degraded loop must back off to %v, got %v", DegradedBackoff, w)
	}
}

// TestTransientSaveDegradeClearedByLaterSuccess guards that the [#93] fix does NOT break the intended
// clear path: a degrade caused by repeated checkpoint-save failures (saveFails>=threshold) must still be
// cleared by a later successful save. Uses a checkpointer that fails N times then succeeds.
func TestTransientSaveDegradeClearedByLaterSuccess(t *testing.T) {
	cp := &flakyCP{memCP: newMemCP(), failFirst: checkpointFailThreshold}
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	r := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	ctx := leaderCtx()
	// Drive `checkpointFailThreshold` failing saves at ascending times → the loop degrades.
	for i := 0; i < checkpointFailThreshold; i++ {
		r.commit(ctx, key, time.Unix(int64(100+i), 0).UTC(), "", 1)
	}
	if !r.Degraded() {
		t.Fatalf("precondition: %d save failures must degrade the loop", checkpointFailThreshold)
	}
	// A later successful save (flakyCP now accepts) must clear the transient degrade.
	r.commit(ctx, key, time.Unix(1000, 0).UTC(), "", 1)
	if r.Degraded() {
		t.Fatal("a successful save must clear a transient checkpoint-save degrade")
	}
}

// flakyCP fails Save the first `failFirst` times (generic error → saveFails increments), then behaves
// like a monotonic in-memory checkpointer.
type flakyCP struct {
	*memCP
	failFirst int
	calls     int
}

func (c *flakyCP) Save(ctx context.Context, k model.CheckpointKey, w model.Watermark) error {
	c.calls++
	if c.calls <= c.failFirst {
		return errors.New("configmap unavailable")
	}
	return c.memCP.Save(ctx, k, w)
}

func TestQueueBlocksOnFull(t *testing.T) {
	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 1, 1, NoopMetrics{})
	ctx := leaderCtx()
	if err := r.Enqueue(ctx, batchAt(key, 60)); err != nil { // fills the cap-1 queue
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	go func() { r.Enqueue(ctx, batchAt(key, 120)); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("second Enqueue should block on the full queue (backpressure), not return")
	case <-time.After(100 * time.Millisecond):
		// expected: still blocked
	}
}

// emitterFunc adapts a func to emit.Emitter (for blocking/observable test emitters).
type emitterFunc func(context.Context, model.Batch) error

func (f emitterFunc) Emit(ctx context.Context, b model.Batch) error { return f(ctx, b) }

// [CP-R3b] The load-bearing round-3 test: a batch is queued, leadership is LOST, then the worker
// runs. Go `select` may dequeue the stale batch in the same tick ctx is done — the leaderCtx.Err()
// guard must prevent any emit. (Drives the real worker, unlike the earlier Reset-only check.)
func TestWorkerDropsQueuedBatchOnLeadershipLoss(t *testing.T) {
	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	r.Enqueue(coordinate.WithEpoch(context.Background(), 1), batchAt(key, 60)) // queued under a live ctx
	leaderCtx, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), 1))
	cancel() // leadership lost BEFORE the worker runs — both ctx.Done and r.q are ready
	done := make(chan struct{})
	go func() { r.Run(leaderCtx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop on cancelled leadership")
	}
	if len(em.emitted) != 0 {
		t.Fatalf("stale queued batch emitted under cancelled leadership (CP-R3b): %v", em.emitted)
	}
}

// countingMetrics embeds NoopMetrics and records SamplesCapped calls.
type countingMetrics struct {
	NoopMetrics
	capped       int
	emittedLogs  int
	guardDrop    int
	lastSuccess  time.Time
	lastSuccessN int
}

func (c *countingMetrics) SamplesCapped(_ string, n int)     { c.capped += n }
func (c *countingMetrics) EmittedLogs(_ string, n int)       { c.emittedLogs += n }
func (c *countingMetrics) GuardDropped(_ string, n int)      { c.guardDrop += n }
func (c *countingMetrics) LastSuccess(_ string, t time.Time) { c.lastSuccess = t; c.lastSuccessN++ }

// TestProcessBatchLogsRecordsLastSuccess: a clean logs emit must stamp last_success with the committed
// window-end (Watermark.Time), mirroring the metrics path — otherwise scrape_healthy and the
// poller-stale/leader-absent alerts silently exclude the logs loops (logs_export, runs).
func TestProcessBatchLogsRecordsLastSuccess(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	m := &countingMetrics{}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{AllowLabelKeys: []string{"ai_model"}}), 4, 1, m)
	ts := time.Unix(1000, 0).UTC()
	b := model.Batch{
		Key:       key,
		Logs:      []model.LogRecord{{Timestamp: ts, IndexedAttributes: map[string]string{"ai_model": "gpt-5"}}},
		Watermark: model.Watermark{Time: ts, Cursor: "win1:done", Epoch: 1},
	}
	r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), b)
	if m.lastSuccessN != 1 || !m.lastSuccess.Equal(ts) {
		t.Fatalf("clean logs emit must record last_success=%v once; got n=%d t=%v", ts, m.lastSuccessN, m.lastSuccess)
	}
}

// TestProcessBatchLogsNoLastSuccessAtZeroTime: a logs emit whose committed Time is still zero (the
// first-window cursor-only step shape) must NOT stamp last_success — an epoch-0 timestamp would read
// as permanently stale. Mirrors the scheduler.go WindowLag zero-Time guard.
func TestProcessBatchLogsNoLastSuccessAtZeroTime(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	m := &countingMetrics{}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{AllowLabelKeys: []string{"ai_model"}}), 4, 1, m)
	b := model.Batch{
		Key:       key,
		Logs:      []model.LogRecord{{Timestamp: time.Unix(1000, 0).UTC(), IndexedAttributes: map[string]string{"ai_model": "gpt-5"}}},
		Watermark: model.Watermark{Cursor: "created:job-1", Epoch: 1}, // Time == zero
	}
	r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), b)
	if m.lastSuccessN != 0 {
		t.Fatalf("zero-Time logs emit must NOT record last_success; got n=%d t=%v", m.lastSuccessN, m.lastSuccess)
	}
}

// TestProcessBatchEmitsLogs: a logs batch is guard-sanitised, emitted, and its watermark (Time + Cursor)
// committed. A denied/non-allow-listed record is dropped (counted) and not emitted.
func TestProcessBatchEmitsLogs(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	m := &countingMetrics{}
	guard := source.NewGuard(source.GuardConfig{AllowLabelKeys: []string{"ai_model"}, DenyFieldKeys: []string{"metadata"}})
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, guard, 4, 1, m)
	ts := time.Unix(1000, 0).UTC()
	b := model.Batch{
		Key: key,
		Logs: []model.LogRecord{
			{Timestamp: ts, IndexedAttributes: map[string]string{"ai_model": "gpt-5"}, RecordAttributes: map[string]string{"trace_id": "a"}},
			{Timestamp: ts, IndexedAttributes: map[string]string{"ai_model": "claude"}},
			{Timestamp: ts, IndexedAttributes: map[string]string{"ai_model": "x"}, RecordAttributes: map[string]string{"metadata": "pii"}}, // denied → dropped
		},
		Watermark: model.Watermark{Time: ts, Cursor: "win1:done", Epoch: 1},
	}
	r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), b)
	if em.emittedLogs != 2 {
		t.Fatalf("emitted logs=%d want 2 (1 denied dropped)", em.emittedLogs)
	}
	if m.emittedLogs != 2 || m.guardDrop != 1 {
		t.Fatalf("metrics emittedLogs=%d guardDrop=%d want 2/1", m.emittedLogs, m.guardDrop)
	}
	stored, _ := cp.Load(context.Background(), key)
	if !stored.Time.Equal(ts) || stored.Cursor != "win1:done" {
		t.Fatalf("watermark not committed: %+v", stored)
	}
}

// TestProcessBatchPersistsCursorOnlyStepAtZeroTime guards the FIRST-WINDOW case of the logs-export step
// machine. stepIdle creates an export job and returns an EMPTY batch (no Logs, no Samples) whose only
// progress is the advanced Cursor — at Watermark.Time == zero, because no window has completed yet so the
// frontier Time is still the zero value. The commit gate must persist that cursor; otherwise the machine
// re-runs stepIdle every tick and the first window loops forever (never starting the job it just created).
func TestProcessBatchPersistsCursorOnlyStepAtZeroTime(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, &NoopMetrics{})
	// A pure cursor advance at the zero Time: no Logs, no Samples — just the job-state token.
	b := model.Batch{Key: key, Watermark: model.Watermark{Cursor: "created:job-1", Epoch: 1}}
	r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), b)
	stored, _ := cp.Load(context.Background(), key)
	if stored.Cursor != "created:job-1" {
		t.Fatalf("cursor-only step at zero Time must persist; got %+v", stored)
	}
	if !stored.Time.IsZero() {
		t.Fatalf("Time must stay zero (no window completed yet); got %v", stored.Time)
	}
	// And the in-memory frontier must reflect it, so the next Since() steps the machine forward.
	if got, _ := r.Since(context.Background()); got.Cursor != "created:job-1" {
		t.Fatalf("in-memory frontier cursor=%q want created:job-1", got.Cursor)
	}
}

// TestProcessBatchLogsTerminalHaltDegrades: a terminal (bad-encoding/unknown-4xx) logs reject degrades
// the loop and does NOT advance the watermark (no silent loss; scheduler backs off).
func TestProcessBatchLogsTerminalHaltDegrades(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "s", Loop: "logs_export", OutputFingerprint: "fp"}
	em := &fakeEmitter{byTS: map[int64]error{}, logsErr: &emit.RejectError{Reason: emit.ReasonUnknown, Status: 400, Msg: "bad"}}
	cp := newMemCP()
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{AllowLabelKeys: []string{"ai_model"}}), 4, 1, &NoopMetrics{})
	ts := time.Unix(1000, 0).UTC()
	b := model.Batch{
		Key:       key,
		Logs:      []model.LogRecord{{Timestamp: ts, IndexedAttributes: map[string]string{"ai_model": "gpt-5"}}},
		Watermark: model.Watermark{Time: ts, Cursor: "win1:done", Epoch: 1},
	}
	r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), b)
	if !r.Degraded() {
		t.Fatal("terminal logs reject must degrade the loop")
	}
	if stored, _ := cp.Load(context.Background(), key); !stored.Time.IsZero() {
		t.Fatalf("terminal halt must NOT advance the watermark, got %+v", stored)
	}
}

// TestProcessBatchCoalescesSubMinute verifies [followup §0]: two sub-minute samples for the same
// series in the same 60s minute are coalesced to 1 before the per-bucket split (LWW), so the DPM
// cap is enforced before any emit. The suppressed sample must be counted via SamplesCapped.
func TestProcessBatchCoalescesSubMinute(t *testing.T) {
	key := model.CheckpointKey{Loop: "analytics"}
	em := &fakeEmitter{byTS: map[int64]error{}} // accepts everything; appends each emitted bucket to .emitted
	cp := newMemCP()
	m := &countingMetrics{}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)

	base := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	b := model.Batch{
		Key: key,
		Samples: []model.Sample{
			{Name: "m", Unit: "1", Timestamp: base.Add(10 * time.Second), Value: 10, Kind: model.Gauge},
			{Name: "m", Unit: "1", Timestamp: base.Add(50 * time.Second), Value: 50, Kind: model.Gauge},
		},
		Watermark: model.Watermark{Time: base.Add(time.Minute)},
	}
	r.ProcessBatch(coordinate.WithEpoch(context.Background(), 1), b)

	if m.capped != 1 {
		t.Fatalf("expected 1 sample capped; got %d", m.capped)
	}
	if len(em.emitted) != 1 {
		t.Fatalf("expected 1 emitted bucket after coalesce; got %v", em.emitted)
	}
}

// [M4] Single-flight emit + completion order: two batches through one worker never overlap and emit
// in order. A gated emitter blocks the first emit while the second is enqueued, proving the worker
// does not start the second concurrently.
func TestSingleFlightEmitInOrder(t *testing.T) {
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	var mu sync.Mutex
	var order []int64
	var inflight, maxIn int32
	started := make(chan struct{}, 2)
	rel := make(chan struct{})
	em := emitterFunc(func(_ context.Context, b model.Batch) error {
		n := atomic.AddInt32(&inflight, 1)
		mu.Lock()
		if n > maxIn {
			maxIn = n
		}
		mu.Unlock()
		started <- struct{}{}
		<-rel
		atomic.AddInt32(&inflight, -1)
		mu.Lock()
		order = append(order, b.Samples[0].Timestamp.Unix())
		mu.Unlock()
		return nil
	})
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	lc, cancel := context.WithCancel(leaderCtx())
	defer cancel()
	go r.Run(lc)
	r.Enqueue(lc, batchAt(key, 60))
	r.Enqueue(lc, batchAt(key, 120))
	<-started                         // first emit began
	time.Sleep(20 * time.Millisecond) // window in which a buggy worker would start the 2nd concurrently
	close(rel)                        // release both
	<-started                         // second emit began only after the first returned
	for i := 0; i < 50 && func() bool { mu.Lock(); defer mu.Unlock(); return len(order) < 2 }(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxIn != 1 {
		t.Fatalf("max concurrent emits = %d, want 1 (single-flight)", maxIn)
	}
	if len(order) != 2 || order[0] != 60 || order[1] != 120 {
		t.Fatalf("emit order = %v, want [60 120]", order)
	}
}
