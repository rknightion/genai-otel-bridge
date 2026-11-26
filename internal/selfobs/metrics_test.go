// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
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

// --- [#521] tenant and pipeline labels (With) ---

// legacyLabelKeys is every self-metric's attribute key set when no With copy carries labels: the label
// sets a legacy install exports today, written out by name. With("", "") must record exactly these.
var legacyLabelKeys = map[string][]string{
	"genai_otel_bridge_emitted_total":                           {"loop", "source"},
	"genai_otel_bridge_emitted_logs_total":                      {"loop", "source"},
	"genai_otel_bridge_samples_skipped_total":                   {"loop", "reason", "source"},
	"genai_otel_bridge_emit_errors_total":                       {"kind", "loop", "source"},
	"genai_otel_bridge_guard_dropped_total":                     {"loop", "source"},
	"genai_otel_bridge_bucket_revised_after_settle_total":       {"loop"},
	"genai_otel_bridge_new_label_values_total":                  {"series"},
	"genai_otel_bridge_samples_capped_total":                    {"loop", "reason", "source"},
	"genai_otel_bridge_source_capability_total":                 {"graph", "loop", "state"},
	"genai_otel_bridge_source_data_incomplete_total":            {"loop", "reason"},
	"genai_otel_bridge_auth_errors_total":                       {"loop", "source"},
	"genai_otel_bridge_emit_partial_success_rejected_total":     {"plane"},
	"genai_otel_bridge_last_success_timestamp_seconds":          {"loop", "source"},
	"genai_otel_bridge_window_lag_seconds":                      {"loop", "source"},
	"genai_otel_bridge_queue_depth":                             {"loop", "source"},
	"genai_otel_bridge_loop_degraded":                           {"loop", "reason", "source"},
	"genai_otel_bridge_leader":                                  {},
	"genai_otel_bridge_go_heap_inuse_bytes":                     {},
	"genai_otel_bridge_go_memory_limit_bytes":                   {},
	"genai_otel_bridge_upstream_request_duration_seconds":       {"method", "status_class", "target"},
	"genai_otel_bridge_emit_request_duration_seconds":           {"plane", "status_class"},
	"genai_otel_bridge_bucket_revised_after_settle_age_seconds": {"loop"},
}

// neverLabelled are the per-process instruments With never labels.
var neverLabelled = map[string]bool{
	"genai_otel_bridge_leader":                true,
	"genai_otel_bridge_go_heap_inuse_bytes":   true,
	"genai_otel_bridge_go_memory_limit_bytes": true,
}

// newTestMetrics builds a Metrics on its own manual reader.
func newTestMetrics(t *testing.T) (*Metrics, *metric.ManualReader) {
	t.Helper()
	r := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(r), metric.WithView(selfHistogramView()))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	return m, r
}

// driveAll calls every recording method once, SeedLoop included.
func driveAll(m *Metrics) {
	id := loopid.ID{Source: "src-a", Loop: "analytics"}
	m.SeedLoop(id, "analytics")
	m.EmittedSamples(id, 1)
	m.EmittedLogs(id, 1)
	m.SamplesSkipped(id, "too_old", 1)
	m.EmitError(id, "collect")
	m.GuardDropped(id, 1)
	m.BucketRevisedAfterSettle("analytics", time.Minute)
	m.NewLabelValue("series-a")
	m.SamplesCapped(id, 1)
	m.SourceCapability("analytics", "graph-a", "no-data")
	m.SourceDataIncomplete("analytics", "window_truncated")
	m.AuthError("analytics", "src-a")
	m.QueueDepth(id, 1)
	m.LastSuccess(id, time.Unix(1700, 0))
	m.WindowLag(id, time.Minute)
	m.LoopDegraded(id, "terminal emit reject", true)
	m.ObserveEmitRequest("metrics", 200, nil, time.Second)
	m.ObserveEmitPartialReject("logs", 2)
	m.ObserveUpstreamRequest("upstream.example", "GET", 200, nil, time.Second)
	m.SetLeader(true)
}

// seriesByName collects every data point's attribute set, keyed by instrument name.
func seriesByName(t *testing.T, r metric.Reader) map[string][]attribute.Set {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]attribute.Set{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], dp.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], dp.Attributes)
				}
			case metricdata.ExponentialHistogram[float64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], dp.Attributes)
				}
			default:
				t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
			}
		}
	}
	return out
}

func sortedLabelKeys(s attribute.Set) []string {
	keys := []string{}
	for _, kv := range s.ToSlice() {
		keys = append(keys, string(kv.Key))
	}
	sort.Strings(keys)
	return keys
}

func encodedSets(sets []attribute.Set) []string {
	out := make([]string, 0, len(sets))
	for _, s := range sets {
		out = append(out, s.Encoded(attribute.DefaultEncoder()))
	}
	sort.Strings(out)
	return out
}

// TestWithAddsTenantAndPipeline [#521]: two copies with one LoopID and different pipelines record two
// window_lag_seconds series that differ in pipeline, so two tenants' same-named loops never share one.
func TestWithAddsTenantAndPipeline(t *testing.T) {
	m, r := newTestMetrics(t)
	id := loopid.ID{Source: "src-a", Loop: "analytics"}
	m.With("t1", "t1.a").WindowLag(id, time.Minute)
	m.With("t1", "t1.b").WindowLag(id, 2*time.Minute)

	got := encodedSets(seriesByName(t, r)["genai_otel_bridge_window_lag_seconds"])
	want := []string{
		"loop=analytics,pipeline=t1.a,source=src-a,tenant=t1",
		"loop=analytics,pipeline=t1.b,source=src-a,tenant=t1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("window_lag_seconds series = %q, want %q", got, want)
	}
}

// TestWithEmptyMatchesUnlabelled [#521]: With("", "") records the legacy label sets, key for key and
// value for value, on every instrument, so the legacy shape exports exactly what it exports today.
func TestWithEmptyMatchesUnlabelled(t *testing.T) {
	base, baseR := newTestMetrics(t)
	driveAll(base)
	m, r := newTestMetrics(t)
	driveAll(m.With("", ""))

	got, want := seriesByName(t, r), seriesByName(t, baseR)
	if len(got) != len(legacyLabelKeys) {
		t.Errorf("With(\"\", \"\") recorded %d instruments, want the %d pinned ones", len(got), len(legacyLabelKeys))
	}
	for name, keys := range legacyLabelKeys {
		sets, ok := got[name]
		if !ok {
			t.Errorf("%s: no series recorded", name)
			continue
		}
		for _, s := range sets {
			if k := sortedLabelKeys(s); !slices.Equal(k, keys) {
				t.Errorf("%s: label keys %v, want %v", name, k, keys)
			}
		}
		if g, w := encodedSets(sets), encodedSets(want[name]); !slices.Equal(g, w) {
			t.Errorf("%s: With(\"\", \"\") series %q differ from the receiver's %q", name, g, w)
		}
	}
}

// TestWithLabelsEveryPerPipelineSeries [#521]: a copy with a tenant and a pipeline adds both labels to
// every series it records, SeedLoop's included, and never to leader or the two memory gauges.
func TestWithLabelsEveryPerPipelineSeries(t *testing.T) {
	m, r := newTestMetrics(t)
	driveAll(m.With("t1", "t1.a"))

	got := seriesByName(t, r)
	for name, legacy := range legacyLabelKeys {
		sets := got[name]
		if len(sets) == 0 {
			t.Errorf("%s: no series recorded", name)
			continue
		}
		want := legacy
		if !neverLabelled[name] {
			want = slices.Sorted(slices.Values(append(slices.Clone(legacy), "tenant", "pipeline")))
		}
		labelled := 0
		for _, s := range sets {
			k := sortedLabelKeys(s)
			// The receiver's own unlabelled partial-reject seeds (NewMetrics) stay beside the copy's.
			if name == "genai_otel_bridge_emit_partial_success_rejected_total" && slices.Equal(k, legacy) {
				continue
			}
			if !slices.Equal(k, want) {
				t.Errorf("%s: label keys %v, want %v", name, k, want)
				continue
			}
			labelled++
			if neverLabelled[name] {
				continue
			}
			if v, _ := s.Value("tenant"); v.AsString() != "t1" {
				t.Errorf("%s: tenant=%q, want t1", name, v.AsString())
			}
			if v, _ := s.Value("pipeline"); v.AsString() != "t1.a" {
				t.Errorf("%s: pipeline=%q, want t1.a", name, v.AsString())
			}
		}
		if labelled == 0 {
			t.Errorf("%s: no series under %v", name, want)
		}
	}
}

// TestWithSeedsPartialRejectAtZero [#521]: a copy with a pipeline creates both plane series of
// emit_partial_success_rejected_total at 0 under its labels (as NewMetrics does unlabelled, #499), so
// the pipeline's first rejection is a measurable increase. With("", "") adds no series.
func TestWithSeedsPartialRejectAtZero(t *testing.T) {
	const name = "genai_otel_bridge_emit_partial_success_rejected_total"
	m, r := newTestMetrics(t)
	m.With("t1", "t1.a")
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != name {
				continue
			}
			for _, dp := range mm.Data.(metricdata.Sum[int64]).DataPoints {
				got[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
			}
		}
	}
	want := map[string]int64{
		"plane=metrics": 0, "plane=logs": 0,
		"pipeline=t1.a,plane=metrics,tenant=t1": 0, "pipeline=t1.a,plane=logs,tenant=t1": 0,
	}
	if !maps.Equal(got, want) {
		t.Fatalf("%s series = %v, want %v", name, got, want)
	}

	m2, r2 := newTestMetrics(t)
	m2.With("", "")
	if n := len(seriesByName(t, r2)[name]); n != 2 {
		t.Fatalf("With(\"\", \"\") must add no partial-reject series: got %d, want the 2 unlabelled", n)
	}
}

// --- unit 4a: the scope gate's two signals ---

// scopePoints maps each data point of the named instrument, by its encoded attribute set, to its value.
func scopePoints(t *testing.T, r metric.Reader, name string) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != name {
				continue
			}
			switch d := mm.Data.(type) {
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[dp.Attributes.Encoded(attribute.DefaultEncoder())] = float64(dp.Value)
				}
			default:
				t.Fatalf("%s is %T", name, mm.Data)
			}
		}
	}
	return out
}

// TestScopeStateGauge: through a With copy, scope_state carries exactly tenant, pipeline and state; the
// current state is 1 and the other two 0, no result is all 0, and two pipelines keep separate series.
// A Metrics nothing has asked for a scope state exports no scope_state series (the legacy shape).
func TestScopeStateGauge(t *testing.T) {
	const name = "genai_otel_bridge_scope_state"
	m, r := newTestMetrics(t)
	if got := scopePoints(t, r, name); len(got) != 0 {
		t.Fatalf("unused scope_state exports %v, want nothing", got)
	}
	a, b := m.With("t1", "t1.a"), m.With("t1", "t1.b")
	a.ScopeState("matched")
	b.ScopeState("unverified")
	for _, sets := range seriesByName(t, r)[name] {
		if k := sortedLabelKeys(sets); !slices.Equal(k, []string{"pipeline", "state", "tenant"}) {
			t.Fatalf("label keys %v, want [pipeline state tenant]", k)
		}
	}
	key := func(p, s string) string { return "pipeline=" + p + ",state=" + s + ",tenant=t1" }
	want := map[string]float64{
		key("t1.a", "matched"): 1, key("t1.a", "unverified"): 0, key("t1.a", "mismatched"): 0,
		key("t1.b", "matched"): 0, key("t1.b", "unverified"): 1, key("t1.b", "mismatched"): 0,
	}
	if got := scopePoints(t, r, name); !maps.Equal(got, want) {
		t.Fatalf("scope_state = %v, want %v", got, want)
	}
	a.ScopeState("mismatched")
	b.ScopeState("")
	want = map[string]float64{
		key("t1.a", "matched"): 0, key("t1.a", "unverified"): 0, key("t1.a", "mismatched"): 1,
		key("t1.b", "matched"): 0, key("t1.b", "unverified"): 0, key("t1.b", "mismatched"): 0,
	}
	if got := scopePoints(t, r, name); !maps.Equal(got, want) {
		t.Fatalf("scope_state after a change and a reset = %v, want %v", got, want)
	}
}

// TestScopeAssertionCounterLabels: through a With copy, scope_assertion_total carries exactly tenant,
// pipeline and result and counts each call under its result.
func TestScopeAssertionCounterLabels(t *testing.T) {
	const name = "genai_otel_bridge_scope_assertion_total"
	m, r := newTestMetrics(t)
	if got := scopePoints(t, r, name); len(got) != 0 {
		t.Fatalf("unused scope_assertion_total exports %v, want nothing", got)
	}
	c := m.With("t1", "t1.a")
	for _, res := range []string{"matched", "mismatch", "unverifiable", "error", "error"} {
		c.ScopeAssertion(res)
	}
	for _, sets := range seriesByName(t, r)[name] {
		if k := sortedLabelKeys(sets); !slices.Equal(k, []string{"pipeline", "result", "tenant"}) {
			t.Fatalf("label keys %v, want [pipeline result tenant]", k)
		}
	}
	key := func(res string) string { return "pipeline=t1.a,result=" + res + ",tenant=t1" }
	want := map[string]float64{key("matched"): 1, key("mismatch"): 1, key("unverifiable"): 1, key("error"): 2}
	if got := scopePoints(t, r, name); !maps.Equal(got, want) {
		t.Fatalf("scope_assertion_total = %v, want %v", got, want)
	}
}
