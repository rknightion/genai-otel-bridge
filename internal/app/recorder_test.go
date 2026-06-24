// SPDX-License-Identifier: AGPL-3.0-only

//go:build acceptance

package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/grafana-ps/aip-oi/internal/checkpoint/file"
	"github.com/grafana-ps/aip-oi/internal/config"
	"github.com/grafana-ps/aip-oi/internal/coordinate"
	"github.com/grafana-ps/aip-oi/internal/emit/otlp"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/schedule"
	"github.com/grafana-ps/aip-oi/internal/source"
	"github.com/grafana-ps/aip-oi/internal/source/portkey"
)

// mimirRecorder models the GC OTLP gateway → Mimir: it records each (series, ts)→value and returns
// 200 on a value-IDENTICAL resend (idempotent), but 400 err-mimir-sample-duplicate-timestamp on a
// value-CHANGED (series, ts). So any non-contiguous or value-divergent re-emit is a hard failure.
type mimirRecorder struct {
	*httptest.Server
	mu           sync.Mutex
	seen         map[string]float64 // (name|labels|tsNanos) → value
	tsBySeries   map[string][]int64 // name|labels → sorted bucket ts (nanos)
	valueChanged int                // count of value-changed rejects
}

func newMimirRecorder(t *testing.T) *mimirRecorder {
	t.Helper()
	r := &mimirRecorder{seen: map[string]float64{}, tsBySeries: map[string][]int64{}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/metrics" {
			w.WriteHeader(404)
			return
		}
		raw, _ := io.ReadAll(req.Body)
		body := raw
		if req.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				w.WriteHeader(400)
				return
			}
			body, _ = io.ReadAll(zr)
		}
		var msg collectormetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &msg); err != nil {
			w.WriteHeader(400)
			w.Write([]byte("failed to parse: " + err.Error()))
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		changed := false
		for _, rm := range msg.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					g := m.GetGauge()
					if g == nil {
						continue
					}
					for _, dp := range g.DataPoints {
						series := m.Name + "|" + kvString(dp.Attributes)
						key := fmt.Sprintf("%s|%d", series, dp.TimeUnixNano)
						v := dp.GetAsDouble()
						if prev, ok := r.seen[key]; ok {
							if prev != v {
								changed = true
							}
							continue // value-identical resend → idempotent no-op
						}
						r.seen[key] = v
						r.tsBySeries[series] = append(r.tsBySeries[series], int64(dp.TimeUnixNano))
					}
				}
			}
		}
		if changed {
			r.valueChanged++
			w.WriteHeader(400)
			w.Write([]byte("err-mimir-sample-duplicate-timestamp for series {...}"))
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(r.Close)
	return r
}

// kvString renders OTLP attributes as a deterministic sorted "k=v;" string (series identity).
func kvString(attrs []*commonpb.KeyValue) string {
	if len(attrs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		parts = append(parts, kv.Key+"="+kv.Value.GetStringValue())
	}
	sort.Strings(parts)
	out := ""
	for _, p := range parts {
		out += p + ";"
	}
	return out
}

func (r *mimirRecorder) AssertNoValueChangedRejects(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.valueChanged != 0 {
		t.Fatalf("gateway saw %d value-changed (series,ts) rejects — a settled bucket was re-emitted with a different value", r.valueChanged)
	}
}

func (r *mimirRecorder) AssertNothingEmitted(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) != 0 {
		t.Fatalf("expected nothing emitted, got %d samples", len(r.seen))
	}
}

func (r *mimirRecorder) AssertNoEmitBefore(t *testing.T, ts time.Time) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := ts.UnixNano()
	for _, tss := range r.tsBySeries {
		for _, x := range tss {
			if x < cut {
				t.Fatalf("emitted a sample at %v, before %v (stale window leaked)", time.Unix(0, x).UTC(), ts)
			}
		}
	}
}

// AssertContiguousMinuteBuckets checks the named series' recorded bucket timestamps form a gap-free
// run of 1-minute buckets covering [from, to] (each minute present exactly once across all cycles).
func (r *mimirRecorder) AssertContiguousMinuteBuckets(t *testing.T, name string, from, to time.Time) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	got := map[int64]bool{}
	for series, tss := range r.tsBySeries {
		if seriesName(series) != name {
			continue
		}
		for _, x := range tss {
			got[x] = true
		}
	}
	for m := from.UTC(); !m.After(to.UTC()); m = m.Add(time.Minute) {
		if !got[m.UnixNano()] {
			t.Fatalf("missing bucket %v for %s (not contiguous)", m, name)
		}
	}
}

// AssertNoContentLabel fails if any recorded series carries a label key/value containing substr.
func (r *mimirRecorder) AssertNoContentLabel(t *testing.T, substr string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for series := range r.tsBySeries {
		if i := indexPipe(series); i >= 0 && contains(series[i+1:], substr) {
			t.Fatalf("content label %q leaked to the gateway in series %q", substr, series)
		}
	}
}

func (r *mimirRecorder) bucketRecorded(name string, ts time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := ts.UnixNano()
	for series, tss := range r.tsBySeries {
		if seriesName(series) != name {
			continue
		}
		for _, x := range tss {
			if x == want {
				return true
			}
		}
	}
	return false
}

func (r *mimirRecorder) AssertBucketAbsent(t *testing.T, name string, ts time.Time) {
	t.Helper()
	if r.bucketRecorded(name, ts) {
		t.Fatalf("bucket %v for %s should have been guard-dropped but reached the gateway", ts, name)
	}
}

func (r *mimirRecorder) AssertBucketPresent(t *testing.T, name string, ts time.Time) {
	t.Helper()
	if !r.bucketRecorded(name, ts) {
		t.Fatalf("bucket %v for %s should have been emitted", ts, name)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func seriesName(series string) string {
	if i := indexPipe(series); i >= 0 {
		return series[:i]
	}
	return series
}

func indexPipe(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			return i
		}
	}
	return -1
}

// --- shared acceptance harness ---

func acceptanceConfig(pkURL string) *config.Config {
	cfg := minimalConfig(pkURL) // graphs:[requests], file checkpoint, no coordinator
	return cfg
}

// fakePortkeyFixed serves 1-min `requests` buckets at anchor-n..anchor-1 (bucket STARTS) with a
// deterministic value per bucket, regardless of the queried window (derive filters by since/settle).
func newFakePortkey(t *testing.T, anchor time.Time, nBuckets int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var dps []map[string]any
		for i := nBuckets; i >= 1; i-- {
			ts := anchor.Add(-time.Duration(i) * time.Minute)
			dps = append(dps, map[string]any{"timestamp": ts.Format(time.RFC3339), "total": float64(1000 - i)})
		}
		json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": 1}, "data_points": dps, "is_quota_exceeded": false})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newEmptyPortkey serves a valid 200 with zero data points for every graph.
func newEmptyPortkey(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": 0}, "data_points": []any{}, "is_quota_exceeded": false})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// buildCycleApp builds a fresh app (fresh in-memory frontier) sharing cpPath, with an injected clock
// and a fast retry policy so an outage exhausts quickly. Returns the shared Checkpointer too.
func buildCycleApp(t *testing.T, cpPath, pkURL, gwURL string, clock func() time.Time, m schedule.Metrics) (*App, schedule.LoopSpec, *file.Store) {
	t.Helper()
	cfg := acceptanceConfig(pkURL)
	cp, err := file.New(cpPath, false)
	if err != nil {
		t.Fatal(err)
	}
	em := otlp.New(otlp.Config{
		Endpoint: gwURL, InstanceID: "1", Token: "t",
		Identity: map[string]string{"service.namespace": "aip-oi"}, MaxBytes: 1 << 20,
		Retry: otlp.RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 30 * time.Millisecond, Jitter: 0},
	})
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, em, m, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	sp := a.Specs()[0]
	if !portkey.SetLoopClockForTest(sp.Loop, clock) {
		t.Fatal("failed to inject clock into portkey loop")
	}
	return a, sp, cp
}

// runCycle performs one Since→Collect→ProcessBatch tick on the loop (a fresh "process").
func runCycle(t *testing.T, sp schedule.LoopSpec) {
	t.Helper()
	lc := coordinate.WithEpoch(context.Background(), 1)
	wm, err := sp.Runner.Since(lc)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	b, err := sp.Loop.Collect(lc, wm)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	sp.Runner.ProcessBatch(lc, b)
}

func loadWatermark(t *testing.T, cpPath string, key model.CheckpointKey) model.Watermark {
	t.Helper()
	cp, err := file.New(cpPath, false)
	if err != nil {
		t.Fatal(err)
	}
	w, err := cp.Load(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func tmpCP(t *testing.T) string { return filepath.Join(t.TempDir(), "wm.yaml") }
