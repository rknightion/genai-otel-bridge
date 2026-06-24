// SPDX-License-Identifier: AGPL-3.0-only

package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// TestOTelClientSpan verifies the otelhttp-wrapped transport emits a semconv CLIENT span per upstream
// request (method + status), parented to the caller's context span. This is the self-APM tracing of our
// own poll/emit pipeline (decision #14 / Cycle 2) — a regression guard against the transport being
// unwrapped, which would silently drop all upstream spans.
func TestOTelClientSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()

	c := New(Config{AllowPrivate: true}) // httptest listens on loopback — the egress guard needs allow_private
	ctx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()
	_ = tp.ForceFlush(context.Background())

	var client sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			client = s
		}
	}
	if client == nil {
		t.Fatal("no CLIENT span recorded — the otelhttp transport wrap is missing")
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range client.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if got := attrs[attribute.Key("http.request.method")].AsString(); got != http.MethodGet {
		t.Errorf("http.request.method = %q, want GET (attrs: %v)", got, client.Attributes())
	}
	if got := attrs[attribute.Key("http.response.status_code")].AsInt64(); got != 200 {
		t.Errorf("http.response.status_code = %d, want 200 (attrs: %v)", got, client.Attributes())
	}
	if client.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("CLIENT span not parented to loop.tick-style parent span")
	}
}
