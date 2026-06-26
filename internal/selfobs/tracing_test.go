// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
	"testing"
)

// TestNewTracerProviderBuilds is a construction smoke test: NewTracerProvider builds a provider + a
// shutdown func without error for an (un-dialled) endpoint — otlptracehttp is lazy, so no network is
// touched at construction. The span-content / identity behaviour is exercised at the scheduler level
// (an in-memory recorder) since asserting exported spans here would require a live collector.
func TestNewTracerProviderBuilds(t *testing.T) {
	tp, shutdown, err := NewTracerProvider(context.Background(), TracingConfig{
		Endpoint: "https://otlp-gateway.example/otlp", InstanceID: "id", Token: "tok",
		ServiceNamespace: "genai-otel-bridge-meta", Environment: "dev", Instance: "pod-1",
	})
	if err != nil {
		t.Fatalf("NewTracerProvider: %v", err)
	}
	if tp == nil || shutdown == nil {
		t.Fatal("nil provider or shutdown")
	}
	// A span can be started + ended without panicking (provider wired); then shut down cleanly.
	_, span := tp.Tracer("test").Start(context.Background(), "smoke")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestNewTracerProviderTokenlessNoAuth guards the [CP-M7] cleartext in-cluster path: with empty creds
// (the token-less hop to an in-cluster Alloy receiver) no Authorization header rides the link — same
// otlpAuthHeaders contract the metric provider uses.
func TestNewTracerProviderTokenlessNoAuth(t *testing.T) {
	if h := otlpAuthHeaders("", ""); h != nil {
		t.Fatalf("token-less endpoint must carry no auth headers, got %v", h)
	}
	tp, shutdown, err := NewTracerProvider(context.Background(), TracingConfig{
		Endpoint:         "http://alloy.monitoring.svc.cluster.local:4318", // cleartext in-cluster
		ServiceNamespace: "genai-otel-bridge-meta", Environment: "dev", Instance: "pod-1",
	})
	if err != nil {
		t.Fatalf("NewTracerProvider (tokenless): %v", err)
	}
	_ = shutdown(context.Background())
	_ = tp
}
