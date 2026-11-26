// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// Pipeline isolation (unit 5b-2, #529; design G3, run-time cases). Three pipelines are assembled through
// BuildFromPlan as the release gate does (crosstenant_helpers_test.go), then driven tick by tick on
// simulated time: the vendor's clock, every loop's clock and the scope gate's clock (Options.Now) move
// together, and each tick is Scheduler.RunOnceForTest, so ten minutes of outage cost milliseconds. The
// emitters use fastRetry (recorder_test.go's buildCycleApp policy), so no real backoff runs. One pipeline
// is made to fail, and every other pipeline's window_lag must stay within settle plus two cadences while
// the failing one's grows past that bound, so the bound is shown to be tight enough to see a fault.
// Run: CGO_ENABLED=1 go test -race -tags acceptance ./internal/app/ -run TestIsolation
//
// The failing pipeline is tried in the first and in the last position of the plan. Pass 1 and pass 3
// take rows in order, so a fault that leaks by sharing one thing among all rows (one emitter, one gate)
// shares the first row's: the first position makes the leak reach the other two pipelines.

// isoRecorder is the self-telemetry hooks of every pipeline of an isolation world: window_lag of the
// analytics loop and auth errors, by pipeline id. It sees what the scheduler and sources report, so the
// tests assert on the signals an operator alerts on, not on a re-derived number.
type isoRecorder struct {
	mu     sync.Mutex
	maxLag map[string]time.Duration
	last   map[string]time.Duration
	seen   map[string]int
	auth   map[string]int
}

func newIsoRecorder() *isoRecorder {
	return &isoRecorder{maxLag: map[string]time.Duration{}, last: map[string]time.Duration{}, seen: map[string]int{}, auth: map[string]int{}}
}

// resetLag forgets the lag observed so far, so the next phase is judged on its own.
func (r *isoRecorder) resetLag() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxLag, r.last, r.seen = map[string]time.Duration{}, map[string]time.Duration{}, map[string]int{}
}

func (r *isoRecorder) lag(pipeline string) (maxLag, last time.Duration, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxLag[pipeline], r.last[pipeline], r.seen[pipeline]
}

func (r *isoRecorder) authErrors(pipeline string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth[pipeline]
}

// isoTelemetry is a pipeline's real telemetry (the one the self-telemetry provider exports) with window_lag
// and auth errors also recorded under the pipeline id.
type isoTelemetry struct {
	PipelineTelemetry
	rec      *isoRecorder
	pipeline string
}

func (t isoTelemetry) WindowLag(id schedule.LoopID, lag time.Duration) {
	if id.Loop == "analytics" {
		t.rec.mu.Lock()
		t.rec.maxLag[t.pipeline] = max(t.rec.maxLag[t.pipeline], lag)
		t.rec.last[t.pipeline] = lag
		t.rec.seen[t.pipeline]++
		t.rec.mu.Unlock()
	}
	t.PipelineTelemetry.WindowLag(id, lag)
}

func (t isoTelemetry) AuthError(loop, src string) {
	t.rec.mu.Lock()
	t.rec.auth[t.pipeline]++
	t.rec.mu.Unlock()
	t.PipelineTelemetry.AuthError(loop, src)
}

// SeedLoop forwards the optional seeding capability, as telemetryAdapter does.
func (t isoTelemetry) SeedLoop(id schedule.LoopID, hookLoop string) {
	if s, ok := t.PipelineTelemetry.(interface {
		SeedLoop(id schedule.LoopID, hookLoop string)
	}); ok {
		s.SeedLoop(id, hookLoop)
	}
}

// isoBuild is gateWorld.build for a world where a row may send somewhere other than the receiver (an
// endpoint that answers 503), over the world's checkpoint file, with isoTelemetry as the telemetry.
func (w *gateWorld) isoBuild(rows []tenancy.Row, rec *isoRecorder) *App {
	w.t.Helper()
	cp, err := file.New(w.cpPath, false)
	if err != nil {
		w.t.Fatal(err)
	}
	ef := OTLPEmitterFactory(w.maxBatchBytes, fastRetry(), logging.NewLimiter(time.Minute))
	tf := func(tenant, pipeline string) PipelineTelemetry {
		return isoTelemetry{PipelineTelemetry: w.metrics.With(tenant, pipeline), rec: rec, pipeline: pipeline}
	}
	a, err := BuildFromPlan(context.Background(), w.cfg, tenancy.Plan{Rows: rows}, cp, coordinate.Noop{}, ef, tf,
		Options{Secrets: w.secrets, Now: w.clock})
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

// isoRun is one leadership of an app on simulated time: what Scheduler.Run does, with the timers replaced
// by step. Every loop ticks when its own cadence has elapsed, so the groups loop ticks every fifth minute.
type isoRun struct {
	t       *testing.T
	w       *gateWorld
	a       *App
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	gates   []*source.ScopeGate
	due     []time.Time
}

// isoStart begins a leadership of a as gateWorld.lead does: a new epoch, every distinct gate told of the
// election first, every runner reset, its loop clock set and its emit worker started.
func (w *gateWorld) isoStart(a *App) *isoRun {
	w.t.Helper()
	w.epoch++
	ctx, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), w.epoch))
	r := &isoRun{t: w.t, w: w, a: a, ctx: ctx, cancel: cancel, due: make([]time.Time, len(a.Specs()))}
	for _, sp := range a.Specs() {
		if sp.Gate != nil && !slices.Contains(r.gates, sp.Gate) {
			r.gates = append(r.gates, sp.Gate)
		}
	}
	for _, g := range r.gates {
		g.Elected(ctx)
	}
	for _, sp := range a.Specs() {
		sp.Runner.Reset()
		if !portkey.SetLoopClockForTest(sp.Loop, w.clock) {
			w.t.Fatalf("cannot set the clock of loop %s", sp.Loop.Key().String())
		}
		r.workers.Add(1)
		go func() { defer r.workers.Done(); sp.Runner.Run(ctx) }()
	}
	return r
}

// step moves simulated time on by d, then ticks every loop that is due, all at once, each awaited until
// its batch is emitted (or abandoned) and its runner idle.
func (r *isoRun) step(d time.Duration) {
	r.t.Helper()
	r.w.advance(d)
	now := r.w.clock()
	var wg sync.WaitGroup
	errs := make(chan string, len(r.due))
	for i, sp := range r.a.Specs() {
		if now.Before(r.due[i]) {
			continue
		}
		r.due[i] = now.Add(sp.Cadence)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.a.sched.RunOnceForTest(r.ctx, sp, now)
			for deadline := time.Now().Add(10 * time.Second); sp.Runner.Busy(); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					errs <- "loop " + sp.Loop.Key().String() + " never finished its batch"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		r.t.Error(e)
	}
}

// stop ends the leadership as Scheduler.Run does: self-telemetry is flushed while still leader, then the
// workers join, the degraded gauges are zeroed and every gate is reset.
func (r *isoRun) stop() {
	r.t.Helper()
	r.w.flush()
	r.cancel()
	r.workers.Wait()
	for _, sp := range r.a.Specs() {
		sp.Runner.ClearDegradedGauge()
	}
	for _, g := range r.gates {
		g.Reset()
	}
}

// specsOf is the specs of one pipeline.
func (r *isoRun) specsOf(pipeline string) []schedule.LoopSpec {
	var out []schedule.LoopSpec
	for _, sp := range r.a.Specs() {
		if sp.Pipeline.ID == pipeline {
			out = append(out, sp)
		}
	}
	return out
}

// analytics is the pipeline's analytics loop spec.
func (r *isoRun) analytics(pipeline string) schedule.LoopSpec {
	r.t.Helper()
	for _, sp := range r.specsOf(pipeline) {
		if sp.Loop.Key().Loop == "analytics" {
			return sp
		}
	}
	r.t.Fatalf("pipeline %s has no analytics loop", pipeline)
	return schedule.LoopSpec{}
}

// isoOrder is the three base tenants with faulty at the front or at the back, the others in their
// usual order.
func isoOrder(faulty string, first bool) []string {
	var rest []string
	for _, n := range []string{"tenant-a", "tenant-b", "tenant-c"} {
		if n != faulty {
			rest = append(rest, n)
		}
	}
	if first {
		return append([]string{faulty}, rest...)
	}
	return append(rest, faulty)
}

// isoPositions runs fn for the faulty tenant in the first and in the last position of the plan.
func isoPositions(t *testing.T, fn func(t *testing.T, names []string, faulty string)) {
	const faulty = "tenant-b"
	for _, pos := range []struct {
		name  string
		first bool
	}{{"faulty pipeline first in the plan", true}, {"faulty pipeline last in the plan", false}} {
		t.Run(pos.name, func(t *testing.T) { fn(t, isoOrder(faulty, pos.first), faulty) })
	}
}

// isoBound is the most window_lag a healthy pipeline may show: bucket settle plus two cadences of its
// analytics loop (design G3; the lag floors at settle plus one cadence, AR-C6).
func isoBound(w *gateWorld, tenant string) time.Duration {
	l := w.row(w.tenant(tenant)).Source.Loops["analytics"]
	return time.Duration(l.BucketSettle) + 2*time.Duration(l.Cadence)
}

// requireHealthy fails unless every named pipeline stayed within bound over the phase the recorder saw,
// delivered something new since before, and reported no auth error.
func (w *gateWorld) requireHealthy(rec *isoRecorder, bound time.Duration, before map[string]int, names ...string) {
	w.t.Helper()
	for _, n := range names {
		p := w.tenant(n).pipeline()
		maxLag, _, seen := rec.lag(p)
		switch {
		case seen == 0:
			w.t.Errorf("%s: window_lag was never reported, so the bound proves nothing", p)
		case maxLag > bound:
			w.t.Errorf("%s: window_lag reached %s, want at most %s (settle plus two cadences)", p, maxLag, bound)
		}
		if got := len(w.delivered(n)); got <= before[n] {
			w.t.Errorf("%s: %d points delivered, want more than the %d it had before the fault", p, got, before[n])
		}
		if got := rec.authErrors(p); got != 0 {
			w.t.Errorf("%s: %d auth errors, want none", p, got)
		}
	}
}

// deliveredCounts is how many points each named tenant has had accepted so far.
func (w *gateWorld) deliveredCounts(names ...string) map[string]int {
	out := map[string]int{}
	for _, n := range names {
		out[n] = len(w.delivered(n))
	}
	return out
}

// TestIsolationDestination503 (AC1): one pipeline's destination answers 503 for ten simulated minutes.
// Every other pipeline's window_lag stays within settle plus two cadences and it keeps delivering; the
// failing one's lag grows past the bound, then returns within it once the destination recovers.
func TestIsolationDestination503(t *testing.T) {
	isoPositions(t, func(t *testing.T, names []string, faulty string) {
		w := newGateWorld(t)
		rec := newIsoRecorder()
		var down atomic.Bool
		var refused atomic.Int32
		dest := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if down.Load() {
				refused.Add(1)
				rw.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			rw.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(dest.Close)
		rows := w.rows(names...)
		for i := range rows {
			if rows[i].Tenant == faulty {
				rows[i].Destination.Endpoint = dest.URL
			}
		}
		healthy := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == faulty })
		bound := isoBound(w, healthy[0])
		fp := w.tenant(faulty).pipeline()

		run := w.isoStart(w.isoBuild(rows, rec))
		run.step(0)
		for range 2 {
			run.step(time.Minute)
		}
		w.requireDelivered(healthy...)
		before := w.deliveredCounts(healthy...)
		down.Store(true)
		rec.resetLag()
		for range 10 { // ten simulated minutes of 503
			run.step(time.Minute)
		}

		w.requireHealthy(rec, bound, before, healthy...)
		if refused.Load() == 0 {
			t.Errorf("%s: its destination refused nothing, so the outage never happened", fp)
		}
		if _, last, _ := rec.lag(fp); last <= bound {
			t.Errorf("%s: window_lag is %s after ten minutes of 503, want more than %s, or the bound cannot see a fault", fp, last, bound)
		}

		down.Store(false)
		for range 3 {
			run.step(time.Minute)
		}
		if _, last, _ := rec.lag(fp); last > bound {
			t.Errorf("%s: window_lag is still %s after its destination recovered, want at most %s", fp, last, bound)
		}
		run.stop()
		w.requireClean()
	})
}

// revoke makes the vendor answer 401 to key from now on.
func (v *markerVendor) revoke(key string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.keys, key)
}

// TestIsolationRevokedKey (AC2): one pipeline's API key is revoked mid-run (401) and stays so for 35
// simulated minutes, longer than a matched scope gate's grace. Only that pipeline
// stops: it delivers nothing more, its checkpoint stops and its auth error is counted; the others keep
// their lag within the bound and report no auth error.
func TestIsolationRevokedKey(t *testing.T) {
	isoPositions(t, func(t *testing.T, names []string, faulty string) {
		w := newGateWorld(t)
		rec := newIsoRecorder()
		healthy := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == faulty })
		bound := isoBound(w, healthy[0])
		fp := w.tenant(faulty).pipeline()

		run := w.isoStart(w.isoBuild(w.rows(names...), rec))
		run.step(0)
		for range 2 {
			run.step(time.Minute)
		}
		w.requireDelivered(names...)
		key := loadWatermark(t, w.cpPath, run.analytics(fp).Loop.Key())
		before := w.deliveredCounts(healthy...)
		frozen := len(w.delivered(faulty))
		w.vendor.revoke(w.tenant(faulty).Key)
		rec.resetLag()
		// A matched gate re-asserts every 10 minutes and still lets a failed call collect for 20 minutes
		// after its last success (scopeReassertInterval, scopeFailGrace), so run well past that: a gate
		// shared with this row would hold every pipeline back from minute 20 on.
		for range 35 {
			run.step(time.Minute)
		}

		w.requireHealthy(rec, bound, before, healthy...)
		if got := len(w.delivered(faulty)); got != frozen {
			t.Errorf("%s: %d points delivered after its key was revoked, want none", fp, got-frozen)
		}
		if got := loadWatermark(t, w.cpPath, run.analytics(fp).Loop.Key()); !got.Time.Equal(key.Time) {
			t.Errorf("%s: checkpoint moved from %s to %s after its key was revoked", fp, key.Time, got.Time)
		}
		if rec.authErrors(fp) == 0 {
			t.Errorf("%s: no auth error was counted for a revoked key", fp)
		}
		run.stop()
		w.requireClean()
	})
}

// TestIsolationScopeMismatch (AC2): one pipeline's API key reads another workspace than its scope names.
// Only that pipeline stops: its gate reports mismatched, no data request leaves it and nothing is
// delivered under its credential, and only its loops are degraded; the others keep their lag within the
// bound.
func TestIsolationScopeMismatch(t *testing.T) {
	isoPositions(t, func(t *testing.T, names []string, faulty string) {
		w := newGateWorld(t)
		rec := newIsoRecorder()
		healthy := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == faulty })
		bound := isoBound(w, healthy[0])
		fp := w.tenant(faulty).pipeline()
		// The faulty tenant's key is a workspace key of a workspace its scope does not name.
		w.vendor.mu.Lock()
		w.vendor.keys["key-elsewhere"] = vendorKey{workspace: "ws-elsewhere"}
		w.vendor.markers["ws-elsewhere"] = 9999
		w.vendor.mu.Unlock()
		w.secrets.values[[2]string{faulty, "api-key-" + w.tenant(faulty).Target}] = "key-elsewhere"

		run := w.isoStart(w.isoBuild(w.rows(names...), rec))
		run.step(0)
		for range 2 {
			run.step(time.Minute)
		}
		w.requireDelivered(healthy...)
		before := w.deliveredCounts(healthy...)
		rec.resetLag()
		for range 10 {
			run.step(time.Minute)
		}

		w.requireHealthy(rec, bound, before, healthy...)
		for _, sp := range run.a.Specs() {
			if want := sp.Pipeline.ID == fp; sp.Runner.Degraded() != want {
				t.Errorf("%s loop %s: degraded is %v, want %v", sp.Pipeline.ID, sp.Loop.Key().Loop, sp.Runner.Degraded(), want)
			}
		}
		for _, r := range w.vendor.seen() {
			if r.Row == fp && !strings.HasSuffix(r.Path, "/analytics/groups/workspace") {
				t.Errorf("%s sent %s although its scope never matched", fp, r.Path)
			}
		}
		w.requireNothingUnder(faulty)
		run.stop()
		if !w.sawScopeState(fp, "mismatched") {
			t.Errorf("%s: self-telemetry never reported scope_state{state=\"mismatched\"}", fp)
		}
		w.requireClean()
	})
}
