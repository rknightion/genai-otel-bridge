// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	collectormetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestProviderExportsExponentialHistograms(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION", "explicit_bucket_histogram")
	requests := make(chan *collectormetric.ExportMetricsServiceRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		request := new(collectormetric.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	ctx := context.Background()
	mp, shutdown, err := NewProvider(ctx, ProviderConfig{Endpoint: server.URL, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.ObserveUpstreamRequest("upstream.example", "GET", 200, nil, 25*time.Second)
	m.ObserveEmitRequest("metrics", 200, nil, 20*time.Second)
	m.BucketRevisedAfterSettle("loop", 30*time.Minute)
	if err := mp.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"genai_otel_bridge_upstream_request_duration_seconds":       false,
		"genai_otel_bridge_emit_request_duration_seconds":           false,
		"genai_otel_bridge_bucket_revised_after_settle_age_seconds": false,
	}
	for _, rm := range (<-requests).ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, metric := range sm.Metrics {
				if _, ok := want[metric.Name]; !ok {
					continue
				}
				want[metric.Name] = true
				h := metric.GetExponentialHistogram()
				if h == nil {
					t.Errorf("%s: wanted exponential histogram, got %T", metric.Name, metric.Data)
					continue
				}
				if len(h.DataPoints) != 1 || h.DataPoints[0].Count != 1 {
					t.Errorf("%s: missing observation", metric.Name)
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing histogram %s", name)
		}
	}
}

// [CP-M7] A token-less self endpoint (the in-cluster cleartext Alloy hop, where the collector holds the
// real Grafana Cloud credentials) must export with NO Authorization header — not a useless "Basic Og==".
func TestOTLPAuthHeaders(t *testing.T) {
	if h := otlpAuthHeaders("", ""); len(h) != 0 {
		t.Fatalf("token-less ⇒ no Authorization header, got %v", h)
	}
	h := otlpAuthHeaders("inst", "tok")
	if h["Authorization"] != basicAuth("inst", "tok") {
		t.Fatalf("with creds ⇒ Basic auth header, got %v", h)
	}
}

func TestMinSelfInterval(t *testing.T) {
	cases := []struct {
		maxDPM int
		want   time.Duration
	}{
		{0, time.Minute}, // guard: <1 ⇒ 1
		{1, time.Minute},
		{2, 30 * time.Second},
		{4, 15 * time.Second},
	}
	for _, c := range cases {
		if got := minSelfInterval(c.maxDPM); got != c.want {
			t.Errorf("minSelfInterval(%d)=%v want %v", c.maxDPM, got, c.want)
		}
	}
}

func TestEffectiveSelfInterval(t *testing.T) {
	// [#90] Unset (0) ⇒ 60s for ALL maxDPM≥1 — raising max_dpm must NOT silently speed up the self
	// plane. A configured value below the floor is clamped up; at/above the floor it is unchanged.
	for _, maxDPM := range []int{1, 2, 5, 12, 60} {
		if got := effectiveSelfInterval(0, maxDPM); got != time.Minute {
			t.Errorf("unset @ max_dpm=%d ⇒ 60s default; got %v", maxDPM, got)
		}
	}
	if got := effectiveSelfInterval(10*time.Second, 1); got != time.Minute {
		t.Errorf("10s @ max_dpm=1 ⇒ clamped 60s; got %v", got)
	}
	// Explicitly configured sub-floor value is still clamped UP to the (sub-60s) floor.
	if got := effectiveSelfInterval(5*time.Second, 4); got != 15*time.Second {
		t.Errorf("5s @ max_dpm=4 ⇒ clamped to 15s floor; got %v", got)
	}
	if got := effectiveSelfInterval(90*time.Second, 1); got != 90*time.Second {
		t.Errorf("90s ≥ floor ⇒ unchanged; got %v", got)
	}
}
