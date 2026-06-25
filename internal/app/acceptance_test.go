// SPDX-License-Identifier: AGPL-3.0-only

//go:build acceptance

// Acceptance / integration gates (DESIGN §9). Run: `go test -tags acceptance ./internal/app/`.
// [CP-M9] `acceptance` is the build tag; the soak additionally guards with testing.Short() at runtime.
// These are REQUIRED gates — they exercise the wired app + a Mimir-model gateway (recorder_test.go)
// that 400s on a value-CHANGED (series,ts), so non-contiguous/value-divergent re-emits hard-fail.
package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

// Step 1 — gap-free failover handoff is contiguous and value-stable.
func TestFailoverHandoffIsContiguous(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	gw := newMimirRecorder(t)
	pk := newFakePortkey(t, anchor, 20) // buckets anchor-20m .. anchor-1m (starts)
	cpPath := tmpCP(t)

	// "A": leader at t=anchor; emits settled buckets, then "dies".
	_, spA, _ := buildCycleApp(t, cpPath, pk.URL, gw.URL, func() time.Time { return anchor }, schedule.NoopMetrics{})
	key := spA.Loop.Key()
	runCycle(t, spA)
	wmA := loadWatermark(t, cpPath, key)

	// "B": a fresh process 10m later resumes from the SHARED checkpoint and emits the newly-settled tail.
	_, spB, _ := buildCycleApp(t, cpPath, pk.URL, gw.URL, func() time.Time { return anchor.Add(10 * time.Minute) }, schedule.NoopMetrics{})
	runCycle(t, spB)
	wmB := loadWatermark(t, cpPath, key)

	gw.AssertNoValueChangedRejects(t)
	// A covers bucket-ends anchor-19m..anchor-3m; B covers anchor-2m..anchor → union is contiguous.
	gw.AssertContiguousMinuteBuckets(t, "portkey_api_requests", anchor.Add(-19*time.Minute), anchor)
	if !wmB.Time.After(wmA.Time) {
		t.Fatalf("B (%v) did not advance the shared checkpoint past A (%v)", wmB.Time, wmA.Time)
	}
}

// Step 1b — a stale batch queued under a prior leadership is DISCARDED on re-election (Reset), and
// Since re-reads the durable checkpoint an intervening leader advanced (complements the unit-level
// CP-R3b worker-race test).
func TestStaleQueuedBatchDroppedOnReelection(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	gw := newMimirRecorder(t)
	pk := newFakePortkey(t, anchor, 20)
	cpPath := tmpCP(t)

	_, sp, cp := buildCycleApp(t, cpPath, pk.URL, gw.URL, func() time.Time { return anchor }, schedule.NoopMetrics{})
	key := sp.Loop.Key()
	lc := coordinate.WithEpoch(context.Background(), 1)

	// Collect a batch and Enqueue it, but DO NOT run the worker (simulate leadership lost before emit).
	b, err := sp.Loop.Collect(lc, model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Runner.Enqueue(lc, b); err != nil {
		t.Fatal(err)
	}
	// An intervening leader advances the shared checkpoint well past the batch's window.
	advanced := model.Watermark{Time: anchor.Add(30 * time.Minute), Epoch: 2}
	if err := cp.Save(context.Background(), key, advanced); err != nil {
		t.Fatal(err)
	}

	// Re-election: Reset() drains the stale batch + clears the in-memory frontier.
	sp.Runner.Reset()
	if sp.Runner.Busy() {
		t.Fatal("Reset must clear busy")
	}
	since, _ := sp.Runner.Since(context.Background())
	if !since.Time.Equal(advanced.Time) {
		t.Fatalf("after Reset, Since must re-read the durable cp (%v), got %v", advanced.Time, since.Time)
	}
	// Run the worker briefly: the drained stale batch must never reach the gateway.
	rc, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), 2))
	go sp.Runner.Run(rc)
	time.Sleep(50 * time.Millisecond)
	cancel()
	gw.AssertNoEmitBefore(t, anchor.Add(30*time.Minute))
}

// Step 1c — a confirmed-empty/quiet window advances the frontier to `until` (so window_lag stays
// bounded) and emits nothing.
func TestEmptyWindowAdvancesFrontier(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	gw := newMimirRecorder(t)
	pk := newEmptyPortkey(t)
	cpPath := tmpCP(t)
	_, sp, _ := buildCycleApp(t, cpPath, pk.URL, gw.URL, func() time.Time { return anchor }, schedule.NoopMetrics{})
	key := sp.Loop.Key()
	runCycle(t, sp)
	got := loadWatermark(t, cpPath, key)
	if !got.Time.Equal(anchor.Add(-3 * time.Minute)) { // until = now - settle(3m)
		t.Fatalf("empty window must advance frontier to until (anchor-3m), got %v", got.Time)
	}
	gw.AssertNothingEmitted(t)
}

// Step 2 — sustained gateway outage: the watermark is stuck (re-pull, no loss) while down, and
// advances once recovered, with no minute-bucket gap.
func TestSustainedOutageRecovery(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	gw := newMimirRecorder(t)
	pk := newFakePortkey(t, anchor, 20)
	cpPath := tmpCP(t)

	var down atomic.Bool
	down.Store(true)
	outage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		gw.Config.Handler.ServeHTTP(w, req) // delegate to the recorder once recovered
	}))
	defer outage.Close()

	// Cycle 1 (down): emits fail (retryable, tiny budget) → no advance.
	_, sp1, _ := buildCycleApp(t, cpPath, pk.URL, outage.URL, func() time.Time { return anchor }, schedule.NoopMetrics{})
	key := sp1.Loop.Key()
	runCycle(t, sp1)
	if got := loadWatermark(t, cpPath, key); !got.Time.IsZero() {
		t.Fatalf("watermark advanced during outage (%v) — should be stuck", got.Time)
	}
	gw.AssertNothingEmitted(t)

	// Recover; cycle 2 emits the window contiguously and advances.
	down.Store(false)
	_, sp2, _ := buildCycleApp(t, cpPath, pk.URL, outage.URL, func() time.Time { return anchor }, schedule.NoopMetrics{})
	runCycle(t, sp2)
	if got := loadWatermark(t, cpPath, key); got.Time.IsZero() {
		t.Fatal("watermark did not advance after recovery")
	}
	gw.AssertNoValueChangedRejects(t)
	gw.AssertContiguousMinuteBuckets(t, "portkey_api_requests", anchor.Add(-19*time.Minute), anchor.Add(-3*time.Minute))
}

// Step 3 — content minimisation: a denylisted content field on a sample is dropped by the wired
// guard and never reaches the gateway.
func TestContentMinimisationGuardDrops(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	gw := newMimirRecorder(t)
	pk := newFakePortkey(t, anchor, 20)
	cpPath := tmpCP(t)
	_, sp, _ := buildCycleApp(t, cpPath, pk.URL, gw.URL, func() time.Time { return anchor }, schedule.NoopMetrics{})
	key := sp.Loop.Key()
	lc := coordinate.WithEpoch(context.Background(), 1)

	leakTS := anchor.Add(-5 * time.Minute)
	keepTS := anchor.Add(-6 * time.Minute)
	crafted := model.Batch{
		Key: key,
		Samples: []model.Sample{
			{Name: "portkey_api_requests", Kind: model.Gauge, Labels: map[string]string{"gen_ai.prompt": "secret-prompt"}, Value: 1, Timestamp: leakTS},
			{Name: "portkey_api_requests", Kind: model.Gauge, Value: 2, Timestamp: keepTS},
		},
		Watermark: model.Watermark{Time: anchor.Add(-3 * time.Minute)},
	}
	sp.Runner.ProcessBatch(lc, crafted)

	gw.AssertNoContentLabel(t, "gen_ai.prompt")
	gw.AssertBucketAbsent(t, "portkey_api_requests", leakTS) // content-bearing sample dropped
	gw.AssertBucketPresent(t, "portkey_api_requests", keepTS)
}

// metricsCounter captures skipped reasons for the soak test.
type metricsCounter struct {
	schedule.NoopMetrics
	mu   sync.Mutex
	skip map[string]int
}

func (m *metricsCounter) SamplesSkipped(_, reason string, n int) {
	m.mu.Lock()
	m.skip[reason] += n
	m.mu.Unlock()
}
func (m *metricsCounter) get(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.skip[reason]
}

// Step 4 — soak (skippable): many cycles + a clock jump beyond max_backfill must count an
// abandoned-as-unstorable span (loud, not silent) and never deadlock.
func TestSoakBackfillUnstorableCounted(t *testing.T) {
	if testing.Short() {
		t.Skip("soak")
	}
	anchor := time.Now().UTC().Truncate(time.Minute)
	gw := newMimirRecorder(t)
	pk := newFakePortkey(t, anchor, 20)
	cpPath := tmpCP(t)
	m := &metricsCounter{skip: map[string]int{}}

	// A normal cycle establishes a watermark near anchor-3m.
	_, sp, _ := buildCycleApp(t, cpPath, pk.URL, gw.URL, func() time.Time { return anchor }, m)
	runCycle(t, sp)

	// A fresh process resumes 2h later — the watermark now predates now-max_backfill(55m) → the
	// scheduler must COUNT an abandoned-as-unstorable span (F25/F47), loudly.
	clock := func() time.Time { return anchor.Add(2 * time.Hour) }
	_, sp2, _ := buildCycleApp(t, cpPath, pk.URL, gw.URL, clock, m)
	sch := schedule.NewScheduler(nil, m)
	for i := 0; i < 5; i++ { // several ticks; must not deadlock
		sch.RunOnceForTest(coordinate.WithEpoch(context.Background(), 1), sp2, clock())
	}
	if m.get("backfill_unstorable") == 0 {
		t.Fatal("a watermark older than max_backfill must count backfill_unstorable (loud counted loss), got 0")
	}
}
