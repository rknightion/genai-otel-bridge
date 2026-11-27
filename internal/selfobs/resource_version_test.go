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
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

func resourceAttrs(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[kv.Key] = kv.GetValue().GetStringValue()
	}
	return out
}

// TestMetricsResourceOmitsServiceVersion guards #314: the Grafana Cloud OTLP ingest promotes
// service.version to a per-series service_version label, so every release would mint a new set of
// self series. The self metrics resource must not carry it (the trace resource keeps it).
func TestMetricsResourceOmitsServiceVersion(t *testing.T) {
	got := make(chan *collectormetric.ExportMetricsServiceRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := new(collectormetric.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		got <- req
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	ctx := context.Background()
	mp, shutdown, err := NewProvider(ctx, ProviderConfig{
		Endpoint: server.URL, Interval: time.Hour, Version: "9.9.9",
		ServiceNamespace: "genai-otel-bridge-meta", Environment: "dev", Instance: "pod-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(ctx) }()
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.ObserveEmitRequest("metrics", 200, nil, time.Second)
	if err := mp.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	req := <-got
	if len(req.ResourceMetrics) == 0 {
		t.Fatal("no resource metrics exported")
	}
	attrs := resourceAttrs(req.ResourceMetrics[0].GetResource().GetAttributes())
	if v, ok := attrs["service.version"]; ok {
		t.Fatalf("metrics resource must not carry service.version (#314), got %q", v)
	}
	if attrs["service.name"] != "genai-otel-bridge" || attrs["service.instance.id"] != "pod-1" {
		t.Fatalf("identity attributes lost: %v", attrs)
	}
}

// TestTraceResourceKeepsServiceVersion pins that traces still correlate to a build (#314, #91).
func TestTraceResourceKeepsServiceVersion(t *testing.T) {
	got := make(chan *collectortrace.ExportTraceServiceRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := new(collectortrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(body, req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		got <- req
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	ctx := context.Background()
	tp, shutdown, err := NewTracerProvider(ctx, TracingConfig{
		Endpoint: server.URL, Version: "9.9.9",
		ServiceNamespace: "genai-otel-bridge-meta", Environment: "dev", Instance: "pod-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, span := tp.Tracer("test").Start(ctx, "s")
	span.End()
	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(ctx) }()
	req := <-got
	attrs := resourceAttrs(req.ResourceSpans[0].GetResource().GetAttributes())
	if attrs["service.version"] != "9.9.9" {
		t.Fatalf("trace resource must carry service.version, got %v", attrs)
	}
}
