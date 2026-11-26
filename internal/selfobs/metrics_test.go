// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/loopid"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

var _ schedule.Metrics = (*Metrics)(nil) // compile-time: satisfies the seam

func gaugeValues(t *testing.T, reader metric.Reader, name string) []float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 gauge", name, m.Data)
			}
			for _, dp := range g.DataPoints {
				out = append(out, dp.Value)
			}
		}
	}
	return out
}

func TestStandbyExportsLeaderGauge(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatal(err)
	}
	const name = "genai_otel_bridge_leader"
	// AC1: no loop has ever run and nothing led: the replica still exports 0.
	if got := gaugeValues(t, reader, name); len(got) != 1 || got[0] != 0 {
		t.Fatalf("never-elected replica exports %v, want [0]", got)
	}
	// AC2: promotion and demotion are both reflected.
	m.SetLeader(true)
	if got := gaugeValues(t, reader, name); len(got) != 1 || got[0] != 1 {
		t.Fatalf("leader exports %v, want [1]", got)
	}
	m.SetLeader(false)
	if got := gaugeValues(t, reader, name); len(got) != 1 || got[0] != 0 {
		t.Fatalf("demoted replica exports %v, want [0]", got)
	}
}

func TestRuntimeMemoryGauges(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if _, err := NewMetrics(provider); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValues(t, reader, "genai_otel_bridge_go_heap_inuse_bytes"); len(got) != 1 || got[0] <= 0 {
		t.Fatalf("heap in use = %v, want one positive value", got)
	}
	prev := debug.SetMemoryLimit(1 << 30)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	if got := gaugeValues(t, reader, "genai_otel_bridge_go_memory_limit_bytes"); len(got) != 1 || got[0] != 1<<30 {
		t.Fatalf("memory limit = %v, want [1 GiB]", got)
	}
	debug.SetMemoryLimit(math.MaxInt64)
	if got := gaugeValues(t, reader, "genai_otel_bridge_go_memory_limit_bytes"); len(got) != 1 || got[0] != 0 {
		t.Fatalf("unset memory limit = %v, want [0]", got)
	}
}

func TestEmitRetryRecordsEachAttemptHistogramBothPlanes(t *testing.T) {
	for _, plane := range []string{"metrics", "logs"} {
		t.Run(plane, func(t *testing.T) {
			reader := metric.NewManualReader()
			provider := metric.NewMeterProvider(metric.WithReader(reader), metric.WithView(selfHistogramView()))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			metrics, err := NewMetrics(provider)
			if err != nil {
				t.Fatal(err)
			}
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/"+plane {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if attempts.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			emitter := otlp.New(otlp.Config{Endpoint: server.URL, MaxBytes: 1 << 20,
				Retry:    otlp.RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxElapsed: time.Second, Multiplier: 1},
				Observer: metrics.ObserveEmitRequest})
			batch := model.Batch{Samples: []model.Sample{{Name: "requests", Kind: model.Gauge, Value: 1, Timestamp: time.Unix(60, 0)}}}
			if plane == "logs" {
				batch.Samples = nil
				batch.Logs = []model.LogRecord{{Timestamp: time.Unix(60, 0)}}
			}
			if err := emitter.Emit(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			var collected metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &collected); err != nil {
				t.Fatal(err)
			}
			histogram, ok := findHistogram(&collected, "genai_otel_bridge_emit_request_duration_seconds")
			if !ok {
				t.Fatal("native emit latency histogram missing")
			}
			counts := map[string]uint64{}
			for _, point := range histogram.DataPoints {
				p, _ := point.Attributes.Value("plane")
				class, _ := point.Attributes.Value("status_class")
				if p.AsString() != plane || point.Attributes.Len() != 2 || point.Sum <= 0 {
					t.Fatalf("invalid attempt observation: %+v", point)
				}
				counts[class.AsString()] += point.Count
			}
			if attempts.Load() != 2 || counts["5xx"] != 1 || counts["2xx"] != 1 || len(counts) != 2 {
				t.Fatalf("attempts=%d histogram counts=%v", attempts.Load(), counts)
			}
		})
	}
}

func TestMetricsRecordViaManualReader(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	id := loopid.ID{Source: "portkey", Loop: "analytics"}
	m.EmittedSamples(id, 5)
	m.SamplesSkipped(id, "duplicate_timestamp", 1)
	m.LastSuccess(id, time.Unix(1700, 0))

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var names int
	for _, sm := range rm.ScopeMetrics {
		names += len(sm.Metrics)
	}
	if names == 0 {
		t.Fatal("no self-metrics recorded")
	}
}

func TestObserveUpstreamRequestRecordsHistogram(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.ObserveUpstreamRequest("api.portkey.ai", "GET", 200, nil, 100*time.Millisecond)
	m.ObserveUpstreamRequest("api.portkey.ai", "GET", 503, nil, 200*time.Millisecond)
	m.ObserveUpstreamRequest("api.portkey.ai", "GET", 0, context.DeadlineExceeded, 2*time.Second)

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	hist, ok := findHistogram(&rm, "genai_otel_bridge_upstream_request_duration_seconds")
	if !ok {
		t.Fatal("genai_otel_bridge_upstream_request_duration_seconds histogram not recorded")
	}
	if len(hist.DataPoints) == 0 {
		t.Fatal("histogram has no data points")
	}
	classes := map[string]bool{}
	var total uint64
	for _, dp := range hist.DataPoints {
		total += dp.Count
		if v, ok := dp.Attributes.Value("status_class"); ok {
			classes[v.AsString()] = true
		}
		if v, ok := dp.Attributes.Value("target"); !ok || v.AsString() != "api.portkey.ai" {
			t.Fatalf("data point missing/wrong target attribute: %v", dp.Attributes.ToSlice())
		}
		if v, ok := dp.Attributes.Value("method"); !ok || v.AsString() != "GET" {
			t.Fatalf("data point missing/wrong method attribute: %v", dp.Attributes.ToSlice())
		}
	}
	if total != 3 {
		t.Fatalf("expected 3 observations, got count=%d", total)
	}
	for _, want := range []string{"2xx", "5xx", "error"} {
		if !classes[want] {
			t.Fatalf("status_class %q not recorded; got %v", want, classes)
		}
	}
}

// [#121] A 25s upstream request (LangSmith's client timeout is 30s and slow responses are expected)
// must resolve into a FINITE bucket, not the +Inf overflow — otherwise histogram_quantile p95/p99 on
// the dashboard cannot distinguish an 11s regime from a 29s regime, one step from timeout.
func TestUpstreamHistogramResolvesLongRequestIntoFiniteBucket(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.ObserveUpstreamRequest("api.smith.langchain.com", "GET", 200, nil, 25*time.Second)

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	hist, ok := findHistogram(&rm, "genai_otel_bridge_upstream_request_duration_seconds")
	if !ok || len(hist.DataPoints) == 0 {
		t.Fatal("upstream histogram not recorded")
	}
	dp := hist.DataPoints[0]
	var bucketCount uint64
	for _, count := range dp.PositiveBucket.Counts {
		bucketCount += count
	}
	if bucketCount != 1 {
		t.Fatalf("25s observation missing from finite exponential buckets: %v", dp.PositiveBucket)
	}
	if dp.Count != 1 {
		t.Fatalf("expected 1 observation, got %d", dp.Count)
	}
}

// [#60] Emit-leg POST latency histogram: bucketed by {plane,status_class}, base2 exponential buckets.
func TestObserveEmitRequestRecordsHistogram(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.ObserveEmitRequest("metrics", 200, nil, 150*time.Millisecond)
	m.ObserveEmitRequest("logs", 204, nil, 300*time.Millisecond)
	m.ObserveEmitRequest("metrics", 0, context.DeadlineExceeded, 20*time.Second)

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	hist, ok := findHistogram(&rm, "genai_otel_bridge_emit_request_duration_seconds")
	if !ok || len(hist.DataPoints) == 0 {
		t.Fatal("genai_otel_bridge_emit_request_duration_seconds histogram not recorded")
	}
	planes, classes := map[string]bool{}, map[string]bool{}
	var total uint64
	for _, dp := range hist.DataPoints {
		total += dp.Count
		if v, ok := dp.Attributes.Value("plane"); ok {
			planes[v.AsString()] = true
		}
		if v, ok := dp.Attributes.Value("status_class"); ok {
			classes[v.AsString()] = true
		}
	}
	if total != 3 {
		t.Fatalf("expected 3 observations, got count=%d", total)
	}
	for _, want := range []string{"metrics", "logs"} {
		if !planes[want] {
			t.Fatalf("plane %q not recorded; got %v", want, planes)
		}
	}
	for _, want := range []string{"2xx", "error"} {
		if !classes[want] {
			t.Fatalf("status_class %q not recorded; got %v", want, classes)
		}
	}
}

// [#120] Degraded-state gauge: 1 while degraded (with reason attr), 0 after the clearing commit records
// the SAME {loop,reason} series — so it returns to 0 rather than sticking at 1.
func TestLoopDegradedGauge(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.LoopDegraded(loopid.ID{Source: "portkey", Loop: "analytics"}, "terminal emit reject", true)

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	g, ok := findGauge(&rm, "genai_otel_bridge_loop_degraded")
	if !ok || len(g.DataPoints) != 1 {
		t.Fatalf("loop_degraded gauge not recorded as a single point: %+v", g)
	}
	dp := g.DataPoints[0]
	if dp.Value != 1 {
		t.Fatalf("degraded gauge value=%v want 1", dp.Value)
	}
	if v, ok := dp.Attributes.Value("reason"); !ok || v.AsString() != "terminal emit reject" {
		t.Fatalf("degraded gauge missing/wrong reason attr: %v", dp.Attributes.ToSlice())
	}

	// Clearing commit records 0 on the SAME {loop,reason} series → gauge returns to 0 (not stuck at 1).
	m.LoopDegraded(loopid.ID{Source: "portkey", Loop: "analytics"}, "terminal emit reject", false)
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	g, _ = findGauge(&rm, "genai_otel_bridge_loop_degraded")
	if len(g.DataPoints) != 1 || g.DataPoints[0].Value != 0 {
		t.Fatalf("degraded gauge should return to a single 0-valued point after clear, got %+v", g.DataPoints)
	}
}

func findGauge(rm *metricdata.ResourceMetrics, name string) (metricdata.Gauge[float64], bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != name {
				continue
			}
			if g, ok := mm.Data.(metricdata.Gauge[float64]); ok {
				return g, true
			}
		}
	}
	return metricdata.Gauge[float64]{}, false
}

func TestSamplesCappedCounter(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.SamplesCapped(loopid.ID{Source: "portkey", Loop: "analytics"}, 3)

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "genai_otel_bridge_samples_capped_total" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("genai_otel_bridge_samples_capped_total not exported")
	}
}

func TestAuthErrorCounter(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.AuthError("analytics", "pk-prod")

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var dp metricdata.DataPoint[int64]
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "genai_otel_bridge_auth_errors_total" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) != 1 {
				t.Fatalf("auth_errors_total not a single-point Int64 sum: %#v", md.Data)
			}
			dp = sum.DataPoints[0]
			found = true
		}
	}
	if !found {
		t.Fatal("genai_otel_bridge_auth_errors_total not exported")
	}
	if dp.Value != 1 {
		t.Fatalf("auth_errors_total value=%d want 1", dp.Value)
	}
	loop, _ := dp.Attributes.Value("loop")
	src, _ := dp.Attributes.Value("source")
	if loop.AsString() != "analytics" || src.AsString() != "pk-prod" {
		t.Fatalf("attributes loop=%q source=%q want analytics/pk-prod", loop.AsString(), src.AsString())
	}
}

func findHistogram(rm *metricdata.ResourceMetrics, name string) (metricdata.ExponentialHistogram[float64], bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != name {
				continue
			}
			if h, ok := mm.Data.(metricdata.ExponentialHistogram[float64]); ok {
				return h, true
			}
		}
	}
	return metricdata.ExponentialHistogram[float64]{}, false
}

// The partial-success reject counter is alerted with increase(), which cannot see a series' first
// sample: a series that appears at its first rejection has no earlier point to rise from. NewMetrics
// therefore creates both plane series at 0 so the first rejection is a visible increase.
func TestPartialRejectCounterExistsAtZeroForBothPlanes(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if _, err := NewMetrics(provider); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "genai_otel_bridge_emit_partial_success_rejected_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data type %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value("plane")
				got[v.AsString()] = dp.Value
			}
		}
	}
	for _, plane := range []string{"metrics", "logs"} {
		if v, ok := got[plane]; !ok || v != 0 {
			t.Errorf("plane %q: series present=%v value=%d, want present at 0 (got %v)", plane, ok, v, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("want exactly the two plane series, got %v", got)
	}
}
