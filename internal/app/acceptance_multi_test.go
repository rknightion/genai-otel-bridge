// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

// The three single-pipeline acceptance scenarios of acceptance_test.go run again with three pipelines
// (unit 5b-2, #529; design G5). Every pipeline has its own fake vendor serving its own values and its own
// Mimir-model recorder (recorder_test.go: a changed value for an existing series is a 400), and all three
// share one checkpoint file, as the pipelines of one process do. Run:
// CGO_ENABLED=1 go test -race -tags acceptance ./internal/app/ -run TestMultiPipeline
package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// multiPipe is one pipeline of the multi-pipeline acceptance world: its vendor, its destination recorder
// and the base of its values. A bucket's value is base minus its age in minutes, so the three pipelines'
// values never overlap and a value that reaches the wrong recorder, or a series a pipeline wrote from
// another's checkpoint, is visible.
type multiPipe struct {
	tenant, target, instance string
	base                     float64
	pk                       *httptest.Server
	gw                       *mimirRecorder
}

func (p multiPipe) id() string { return tenancy.PipelineID(p.tenant, p.target) }

// newValuedPortkey is newFakePortkey with a per-pipeline base: n one-minute requests buckets whose starts
// are anchor-n .. anchor-1m, each worth base-age.
func newValuedPortkey(t *testing.T, anchor time.Time, n int, base float64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var dps []map[string]any
		for i := n; i >= 1; i-- {
			ts := anchor.Add(-time.Duration(i) * time.Minute)
			dps = append(dps, map[string]any{"timestamp": ts.Format(time.RFC3339), "total": base - float64(i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": 1}, "data_points": dps, "is_quota_exceeded": false})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMultiPipes is three pipelines on three vendors and three recorders. With empty set, each vendor
// answers with no data points at all.
func newMultiPipes(t *testing.T, anchor time.Time, empty bool) []multiPipe {
	t.Helper()
	pipes := []multiPipe{
		{tenant: "tenant-a", target: "primary", instance: "1001", base: 1000},
		{tenant: "tenant-b", target: "secondary", instance: "1002", base: 2000},
		{tenant: "tenant-c", target: "spare", instance: "1003", base: 3000},
	}
	for i := range pipes {
		pipes[i].gw = newMimirRecorder(t)
		if empty {
			pipes[i].pk = newEmptyPortkey(t)
		} else {
			pipes[i].pk = newValuedPortkey(t, anchor, 20, pipes[i].base)
		}
	}
	return pipes
}

func (p multiPipe) row() tenancy.Row {
	r := tenantRow(p.pk.URL, p.tenant, p.target, p.gw.URL, p.instance)
	r.Destination.Token = "token-" + p.tenant
	return r
}

// multiApp is one process of the three pipelines over the shared checkpoint file: a fresh app (fresh
// in-memory frontiers) built through BuildFromPlan, one loop clock per pipeline. Its specs are by pipeline
// id; each pipeline has the one analytics loop. The store is the app's own checkpointer.
func multiApp(t *testing.T, cpPath string, pipes []multiPipe, clock func(multiPipe) time.Time) (map[string]schedule.LoopSpec, *file.Store) {
	t.Helper()
	cp, err := file.New(cpPath, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]tenancy.Row, 0, len(pipes))
	for _, p := range pipes {
		rows = append(rows, p.row())
	}
	cfg := acceptanceConfig("http://unused.invalid")
	ef := OTLPEmitterFactory(1<<20, fastRetry(), logging.NewLimiter(time.Minute))
	tf := func(string, string) PipelineTelemetry { return telemetryAdapter{Metrics: schedule.NoopMetrics{}} }
	a, err := BuildFromPlan(context.Background(), cfg, tenancy.Plan{Rows: rows}, cp, coordinate.Noop{}, ef, tf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]schedule.LoopSpec{}
	for _, sp := range a.Specs() {
		specs[sp.Pipeline.ID] = sp
	}
	for _, p := range pipes {
		sp, ok := specs[p.id()]
		if !ok {
			t.Fatalf("no loop for pipeline %s", p.id())
		}
		c := clock(p)
		if !portkey.SetLoopClockForTest(sp.Loop, func() time.Time { return c }) {
			t.Fatalf("cannot set the clock of pipeline %s", p.id())
		}
	}
	if len(specs) != len(pipes) {
		t.Fatalf("%d specs for %d pipelines", len(specs), len(pipes))
	}
	return specs, cp
}

// assertOnlyOwnValues fails unless the recorder holds at least one sample and every one is within the
// pipeline's own value range (base-100, base).
func (p multiPipe) assertOnlyOwnValues(t *testing.T) {
	t.Helper()
	p.gw.mu.Lock()
	defer p.gw.mu.Unlock()
	if len(p.gw.seen) == 0 {
		t.Errorf("%s: its recorder holds nothing", p.id())
	}
	for k, v := range p.gw.seen {
		if v >= p.base || v <= p.base-100 {
			t.Errorf("%s: its recorder holds %s = %v, outside its own values (%v, %v)", p.id(), k, v, p.base-100, p.base)
		}
	}
}

// TestMultiPipelineFailoverHandoff (AC3): the handoff between two leaders is contiguous and value-stable
// for each of three pipelines: each recorder holds every minute exactly once, none saw a changed value,
// and the second leader advanced every pipeline's own checkpoint.
func TestMultiPipelineFailoverHandoff(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	pipes := newMultiPipes(t, anchor, false)
	cpPath := tmpCP(t)

	// "A": leader at t=anchor; emits settled buckets, then "dies".
	specsA, _ := multiApp(t, cpPath, pipes, func(multiPipe) time.Time { return anchor })
	wmA := map[string]model.Watermark{}
	for _, p := range pipes {
		runCycle(t, specsA[p.id()])
		wmA[p.id()] = loadWatermark(t, cpPath, specsA[p.id()].Loop.Key())
	}
	// "B": a fresh process 10m later resumes from the SHARED checkpoint and emits the newly-settled tail.
	specsB, _ := multiApp(t, cpPath, pipes, func(multiPipe) time.Time { return anchor.Add(10 * time.Minute) })
	for _, p := range pipes {
		runCycle(t, specsB[p.id()])
	}

	for _, p := range pipes {
		p.gw.AssertNoValueChangedRejects(t)
		// A covers bucket-ends anchor-19m..anchor-3m; B covers anchor-2m..anchor: the union is contiguous.
		p.gw.AssertContiguousMinuteBuckets(t, "portkey_api_requests", anchor.Add(-19*time.Minute), anchor)
		p.assertOnlyOwnValues(t)
		if wmB := loadWatermark(t, cpPath, specsB[p.id()].Loop.Key()); !wmB.Time.After(wmA[p.id()].Time) {
			t.Errorf("%s: B (%v) did not advance its checkpoint past A (%v)", p.id(), wmB.Time, wmA[p.id()].Time)
		}
	}
}

// TestMultiPipelineStaleQueueDropped (AC3): a batch each pipeline queued under a prior leadership is
// discarded on re-election, and each pipeline's Since re-reads its own durable checkpoint, which an
// intervening leader advanced to a time of its own (so a checkpoint read under another pipeline's key is
// seen).
func TestMultiPipelineStaleQueueDropped(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	pipes := newMultiPipes(t, anchor, false)
	cpPath := tmpCP(t)
	specs, cp := multiApp(t, cpPath, pipes, func(multiPipe) time.Time { return anchor })

	lc := coordinate.WithEpoch(context.Background(), 1)
	advanced := map[string]model.Watermark{}
	for i, p := range pipes {
		sp := specs[p.id()]
		// Collect a batch and Enqueue it, but DO NOT run the worker (leadership lost before emit).
		b, err := sp.Loop.Collect(lc, model.Watermark{})
		if err != nil {
			t.Fatal(err)
		}
		if err := sp.Runner.Enqueue(lc, b); err != nil {
			t.Fatal(err)
		}
		// An intervening leader advances the shared checkpoint well past the window, to a time of its own.
		advanced[p.id()] = model.Watermark{Time: anchor.Add(time.Duration(30+i) * time.Minute), Epoch: 2}
		if err := cp.Save(context.Background(), sp.Loop.Key(), advanced[p.id()]); err != nil {
			t.Fatal(err)
		}
	}
	// Re-election: Reset() drains each stale batch and clears its in-memory frontier.
	for _, p := range pipes {
		sp := specs[p.id()]
		sp.Runner.Reset()
		if sp.Runner.Busy() {
			t.Fatalf("%s: Reset must clear busy", p.id())
		}
		since, _ := sp.Runner.Since(context.Background())
		if !since.Time.Equal(advanced[p.id()].Time) {
			t.Fatalf("%s: after Reset, Since must re-read its own durable checkpoint (%v), got %v", p.id(), advanced[p.id()].Time, since.Time)
		}
	}
	// Run the workers briefly: no drained stale batch may reach any gateway.
	rc, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), 2))
	for _, p := range pipes {
		go specs[p.id()].Runner.Run(rc)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	for _, p := range pipes {
		p.gw.AssertNothingEmitted(t)
	}
}

// TestMultiPipelineEmptyWindowAdvances (AC3): a confirmed-empty window advances each pipeline's frontier
// to its own until (its clock less settle) and emits nothing. The three clocks differ, so a frontier
// written under another pipeline's checkpoint key is seen.
func TestMultiPipelineEmptyWindowAdvances(t *testing.T) {
	anchor := time.Now().UTC().Truncate(time.Minute)
	pipes := newMultiPipes(t, anchor, true)
	cpPath := tmpCP(t)
	clockOf := func(p multiPipe) time.Time {
		for i, q := range pipes {
			if q.id() == p.id() {
				return anchor.Add(time.Duration(i) * time.Minute)
			}
		}
		return anchor
	}
	specs, _ := multiApp(t, cpPath, pipes, clockOf)
	for _, p := range pipes {
		runCycle(t, specs[p.id()])
	}
	for _, p := range pipes {
		want := clockOf(p).Add(-3 * time.Minute) // until = now - settle(3m)
		if got := loadWatermark(t, cpPath, specs[p.id()].Loop.Key()); !got.Time.Equal(want) {
			t.Errorf("%s: an empty window must advance its frontier to until (%v), got %v", p.id(), want, got.Time)
		}
		p.gw.AssertNothingEmitted(t)
	}
}
