// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/config"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// newMutablePortkey is a fake Portkey whose per-graph response is read fresh on each request via
// the supplied closure — so a test can change a bucket's value between Collect calls. It serves
// only buckets within the requested [time_of_generation_min, time_of_generation_max] window so the
// detection window's widened lower bound is exercised realistically.
func newMutablePortkey(t *testing.T, bodies func() map[string]graphResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		graph := r.URL.Path[len("/analytics/graphs/"):]
		b, ok := bodies()[graph]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Filter datapoints to the requested window (inclusive), mimicking the real API.
		q := r.URL.Query()
		min, _ := time.Parse(time.RFC3339, q.Get("time_of_generation_min"))
		max, _ := time.Parse(time.RFC3339, q.Get("time_of_generation_max"))
		out := graphResponse{IsQuotaExceeded: b.IsQuotaExceeded, Object: b.Object}
		for _, dp := range b.DataPoints {
			ts, err := time.Parse(time.RFC3339, dp.Timestamp)
			if err != nil {
				continue
			}
			if (ts.Equal(min) || ts.After(min)) && (ts.Equal(max) || ts.Before(max)) {
				out.DataPoints = append(out.DataPoints, dp)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// --- pure detection unit tests (no IO, deterministic) -------------------------------------------

func sampleAt(name string, labels map[string]string, t time.Time, v float64) model.Sample {
	return model.Sample{Name: name, Kind: model.Gauge, Labels: labels, Value: v, Timestamp: t}
}

// TestRevisionHistoryDetectsChange covers the three spec cases against the pure history:
// changed already-seen bucket → revision; unchanged → none; brand-new bucket → none.
func TestRevisionHistoryDetectsChange(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	band := 5 * time.Minute
	h := newRevisionHistory(band)

	// First observation of two buckets — all new, never a revision.
	n := h.observe([]model.Sample{
		sampleAt("portkey_api_requests", nil, base.Add(1*time.Minute), 10),
		sampleAt("portkey_api_requests", nil, base.Add(2*time.Minute), 20),
	}, base.Add(2*time.Minute))
	if n != 0 {
		t.Fatalf("first observation must report 0 revisions, got %d", n)
	}

	// Re-observe both UNCHANGED, plus a brand-new forward bucket → still 0 revisions.
	n = h.observe([]model.Sample{
		sampleAt("portkey_api_requests", nil, base.Add(1*time.Minute), 10),
		sampleAt("portkey_api_requests", nil, base.Add(2*time.Minute), 20),
		sampleAt("portkey_api_requests", nil, base.Add(3*time.Minute), 30), // new forward bucket
	}, base.Add(3*time.Minute))
	if n != 0 {
		t.Fatalf("unchanged re-observe + new bucket must report 0 revisions, got %d", n)
	}

	// Now bucket base+2m CHANGES value (late arrival after settle) → exactly one revision.
	n = h.observe([]model.Sample{
		sampleAt("portkey_api_requests", nil, base.Add(1*time.Minute), 10),
		sampleAt("portkey_api_requests", nil, base.Add(2*time.Minute), 999), // changed!
		sampleAt("portkey_api_requests", nil, base.Add(3*time.Minute), 30),
	}, base.Add(3*time.Minute))
	if n != 1 {
		t.Fatalf("changed settled bucket must report exactly 1 revision, got %d", n)
	}

	// Re-observe with the new value present → no further revision (history was updated).
	n = h.observe([]model.Sample{
		sampleAt("portkey_api_requests", nil, base.Add(2*time.Minute), 999),
	}, base.Add(3*time.Minute))
	if n != 0 {
		t.Fatalf("re-observe of updated value must report 0 revisions, got %d", n)
	}
}

// TestRevisionHistoryDistinctSeries: same bucket time, different label set (quantile) must NOT
// alias — a length-prefixed series key keeps them distinct.
func TestRevisionHistoryDistinctSeries(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	h := newRevisionHistory(10 * time.Minute)
	bt := base.Add(1 * time.Minute)
	h.observe([]model.Sample{
		sampleAt("portkey_api_latency_seconds", map[string]string{"quantile": "p50"}, bt, 0.1),
		sampleAt("portkey_api_latency_seconds", map[string]string{"quantile": "p90"}, bt, 0.4),
	}, bt)
	// p90 changes; p50 unchanged → exactly one revision, no cross-talk.
	n := h.observe([]model.Sample{
		sampleAt("portkey_api_latency_seconds", map[string]string{"quantile": "p50"}, bt, 0.1),
		sampleAt("portkey_api_latency_seconds", map[string]string{"quantile": "p90"}, bt, 0.9),
	}, bt)
	if n != 1 {
		t.Fatalf("distinct-series change must report exactly 1 revision, got %d", n)
	}
}

// TestRevisionHistoryEvicts: entries older than the trailing band are evicted, so memory is bounded
// AND a stale entry can't produce a false revision after it ages out (it's simply re-learned).
func TestRevisionHistoryEvicts(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	band := 5 * time.Minute
	h := newRevisionHistory(band)
	old := base.Add(1 * time.Minute)
	h.observe([]model.Sample{sampleAt("m", nil, old, 1)}, old)
	if got := h.len(); got != 1 {
		t.Fatalf("after first observe len=%d want 1", got)
	}
	// Advance well past the band; observe a fresh bucket. The old entry must be evicted.
	newT := old.Add(30 * time.Minute)
	h.observe([]model.Sample{sampleAt("m", nil, newT, 2)}, newT)
	if got := h.len(); got != 1 {
		t.Fatalf("stale entry not evicted: len=%d want 1 (only the fresh bucket)", got)
	}
	// The old bucket re-appearing with a different value is treated as NEW (it aged out), not a
	// revision — bounded memory means we accept this blind spot beyond the band.
	n := h.observe([]model.Sample{sampleAt("m", nil, old, 12345)}, newT)
	if n != 0 {
		t.Fatalf("re-learning an evicted bucket must not count as a revision, got %d", n)
	}
}

// --- integration: detection fires through Collect with the OnBucketRevised hook ------------------

type revisionRecorder struct {
	mu    sync.Mutex
	loops []string
}

func (r *revisionRecorder) hook(loop string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loops = append(r.loops, loop)
}

func (r *revisionRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.loops)
}

func mkSourceWithRevision(t *testing.T, baseURL string, now time.Time, onRevised func(string)) *analyticsLoop {
	t.Helper()
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: baseURL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
			BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
			MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
			Graphs: []string{"requests"},
		}},
	}
	src, err := New(cfg, source.Deps{OnBucketRevised: onRevised})
	if err != nil {
		t.Fatal(err)
	}
	lp := src.Loops()[0].(*analyticsLoop)
	lp.now = func() time.Time { return now }
	return lp
}

// TestCollectFiresRevisionHook verifies the wiring end-to-end: a settled bucket that changes value
// on a later Collect (re-fetched because the detection window overlaps the settle band) fires the
// injected OnBucketRevised hook.
func TestCollectFiresRevisionHook(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	var bodyMu sync.Mutex
	body := map[string]graphResponse{}
	srv := newMutablePortkey(t, func() map[string]graphResponse {
		bodyMu.Lock()
		defer bodyMu.Unlock()
		return body
	})
	defer srv.Close()
	setBody := func(b map[string]graphResponse) { bodyMu.Lock(); body = b; bodyMu.Unlock() }

	mk := func(v2 float64) []dataPoint {
		var d []dataPoint
		for i := 1; i <= 6; i++ {
			val := float64(i)
			if i == 2 {
				val = v2
			}
			d = append(d, dataPoint{Timestamp: tAt(base, i), Total: val})
		}
		return d
	}

	rec := &revisionRecorder{}
	lp := mkSourceWithRevision(t, srv.URL, base.Add(10*time.Minute), rec.hook)

	// Collect #1 at base+10m emits the settled buckets; bucket base+2 (end base+3m) has value 2.
	setBody(map[string]graphResponse{"requests": {DataPoints: mk(2)}})
	b1, err := lp.Collect(context.Background(), model.Watermark{Time: base})
	if err != nil {
		t.Fatalf("collect#1: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("first collect must not fire revision, got %d", rec.count())
	}

	// Collect #2 a minute later: the watermark advanced past base+3m, so bucket base+3m is no longer
	// forward-emitted — but the detection window re-fetches it. Change its value → expect a fire.
	lp.now = func() time.Time { return base.Add(11 * time.Minute) }
	setBody(map[string]graphResponse{"requests": {DataPoints: mk(999)}})
	if _, err := lp.Collect(context.Background(), b1.Watermark); err != nil {
		t.Fatalf("collect#2: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("changed settled bucket must fire revision once, got %d", rec.count())
	}
}

// TestCollectFetchWindowNeverExceeds55m is the granularity-safety regression guard. The detection
// pass widens the fetch lower bound by the band (≈2×bucket_settle); without an explicit width clamp,
// a raised max_backfill (operationally recommended for long-outage recovery) + a mid-range catch-up
// `since` pushes until-fetchStart past 59m, flipping Portkey to 10-minute buckets — which, since the
// emit pass shares the same response, silently corrupts the 1-minute product metrics. The fetch width
// must stay ≤55m regardless. (Conditions here yield a 60m fetch pre-fix; the clamp pins it to 55m.)
func TestCollectFetchWindowNeverExceeds55m(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(2 * time.Hour)

	var mu sync.Mutex
	var widest time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lo, _ := time.Parse(time.RFC3339, q.Get("time_of_generation_min"))
		hi, _ := time.Parse(time.RFC3339, q.Get("time_of_generation_max"))
		mu.Lock()
		if d := hi.Sub(lo); d > widest {
			widest = d
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(graphResponse{})
	}))
	defer srv.Close()

	lp := mkSourceWithRevision(t, srv.URL, now, func(string) {})
	lp.maxBackfill = 120 * time.Minute             // raised for long-outage recovery (uncapped, GS2)
	lp.settle = 10 * time.Minute                   // the new default
	lp.band = detectionBand(10 * time.Minute)      // band = 20m
	lp.histories[""] = newRevisionHistory(lp.band) // replace the legacy-pass history

	// Mid-range catch-up: `start` sits well above the now-maxBackfill floor, so the band fully widens
	// the fetch. Without the width clamp until-fetchStart = 60m (granularity flip); with it, ≤55m.
	if _, err := lp.Collect(context.Background(), model.Watermark{Time: now.Add(-50 * time.Minute)}); err != nil {
		t.Fatalf("collect: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if widest == 0 {
		t.Fatal("no request captured — Collect did not issue a fetch")
	}
	if widest > 55*time.Minute {
		t.Fatalf("fetch window %s exceeds 55m — Portkey would flip to 10-min buckets, corrupting the 1-min product metrics", widest)
	}
}
