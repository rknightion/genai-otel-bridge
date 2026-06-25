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
	hist, ok := findHistogram(&rm, "decant_upstream_request_duration_seconds")
	if !ok {
		t.Fatal("decant_upstream_request_duration_seconds histogram not recorded")
	}
	if len(hist.DataPoints) == 0 {
		t.Fatal("histogram has no data points")
	}
	// Guard that the SECOND-shaped explicit buckets actually took effect — without them the OTel
	// default boundaries are ms-shaped (up to 10000) and a _seconds histogram is useless granularity.
	wantBounds := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
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
			if md.Name == "decant_samples_capped_total" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("decant_samples_capped_total not exported")
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
			if md.Name != "decant_auth_errors_total" {
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
		t.Fatal("decant_auth_errors_total not exported")
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
