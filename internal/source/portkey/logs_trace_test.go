// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// TestLogsExportLifecycleStepSpans keeps the multi-tick export visible in self-APM: the individual
// create/start/poll/download/page timings are spans and each later tick links back to the first create span.
// The lifecycle link lives only in the durable cursor; it never becomes an emitted product-log field.
func TestLogsExportLifecycleStepSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 2, func(int) string { return nLines(1, "m") })
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{
		"window": "1h", "settle": "10m", "page_size": "1", "chunk_max_records": "100",
	}), now.Add(24*time.Hour))
	if !SetLoopClockForTest(l, func() time.Time { return now }) {
		t.Fatal("SetLoopClockForTest rejected logs_export loop")
	}

	wm := model.Watermark{}
	for step := 0; step < 8; step++ { // create -> start -> poll -> download -> page -> start -> poll -> download
		batch, err := l.Collect(context.Background(), wm)
		if err != nil {
			t.Fatalf("step %d: Collect: %v", step, err)
		}
		wm = batch.Watermark
	}

	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Ended() {
		spans[span.Name()] = span
	}
	for _, name := range []string{
		"portkey.logs_export.create",
		"portkey.logs_export.start",
		"portkey.logs_export.poll",
		"portkey.logs_export.download",
		"portkey.logs_export.page",
	} {
		if spans[name] == nil {
			t.Fatalf("missing lifecycle timing span %q", name)
		}
		if !spans[name].EndTime().After(spans[name].StartTime()) {
			t.Fatalf("span %q did not record a positive timing interval", name)
		}
	}

	created := spans["portkey.logs_export.create"].SpanContext()
	for _, name := range []string{"portkey.logs_export.start", "portkey.logs_export.poll", "portkey.logs_export.download", "portkey.logs_export.page"} {
		if !hasSpanLink(spans[name].Links(), created) {
			t.Fatalf("%s is not linked to the create step; cross-tick lifecycle correlation was lost", name)
		}
	}
}

func hasSpanLink(links []sdktrace.Link, want trace.SpanContext) bool {
	for _, link := range links {
		// Persisted contexts are reconstructed as remote links, whereas the recorder retains the
		// original local create context. Remote is transport metadata, not lifecycle identity.
		if link.SpanContext.TraceID() == want.TraceID() && link.SpanContext.SpanID() == want.SpanID() {
			return true
		}
	}
	return false
}
