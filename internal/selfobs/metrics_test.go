// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

var _ schedule.Metrics = (*Metrics)(nil) // compile-time: satisfies the seam

func TestMetricsRecordViaManualReader(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.EmittedSamples("analytics", 5)
	m.SamplesSkipped("analytics", "duplicate_timestamp", 1)
	m.LastSuccess("analytics", time.Unix(1700, 0))

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
	mp := metric.NewMeterProvider(metric.WithReader(r))
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
	// Guard that the SECOND-shaped explicit buckets actually took effect — without them the OTel
	// default boundaries are ms-shaped (up to 10000) and a _seconds histogram is useless granularity.
	// [#121] Boundaries extended past 10s to 20/30/60 so a 30s-timeout (LangSmith) / degrading request
	// resolves in a finite bucket instead of pinning at +Inf.
	wantBounds := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60}
	if !slices.Equal(hist.DataPoints[0].Bounds, wantBounds) {
		t.Fatalf("histogram must use second-shaped explicit buckets, got %v", hist.DataPoints[0].Bounds)
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
	mp := metric.NewMeterProvider(metric.WithReader(r))
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
	// BucketCounts has len(Bounds)+1 entries; the last is the +Inf overflow. A 25s observation must NOT
	// land there (boundaries now reach 60), so the overflow bucket stays 0.
	if overflow := dp.BucketCounts[len(dp.BucketCounts)-1]; overflow != 0 {
		t.Fatalf("25s request landed in the +Inf overflow bucket (count=%d); boundaries do not cover the client timeout", overflow)
	}
	if dp.Count != 1 {
		t.Fatalf("expected 1 observation, got %d", dp.Count)
	}
}

// [#60] Emit-leg POST latency histogram: bucketed by {plane,status_class}, second-shaped buckets to 30s.
func TestObserveEmitRequestRecordsHistogram(t *testing.T) {
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r))
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
	wantBounds := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30}
	if !slices.Equal(hist.DataPoints[0].Bounds, wantBounds) {
		t.Fatalf("emit histogram must use second-shaped buckets to 30s, got %v", hist.DataPoints[0].Bounds)
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
	mp := metric.NewMeterProvider(metric.WithReader(r))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.LoopDegraded("portkey/analytics", "terminal emit reject", true)

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
	m.LoopDegraded("portkey/analytics", "terminal emit reject", false)
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
	mp := metric.NewMeterProvider(metric.WithReader(r))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.SamplesCapped("portkey/analytics", 3)

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
	mp := metric.NewMeterProvider(metric.WithReader(r))
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

func findHistogram(rm *metricdata.ResourceMetrics, name string) (metricdata.Histogram[float64], bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != name {
				continue
			}
			if h, ok := mm.Data.(metricdata.Histogram[float64]); ok {
				return h, true
			}
		}
	}
	return metricdata.Histogram[float64]{}, false
}
