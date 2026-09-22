// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestUpstreamClientSpanNestedUnderLoopTick covers the production path from a scheduler tick through
// source.Loop.Collect to the shared outbound client. The initial red phase deliberately uses an
// uninstrumented client: the assertion must reject that bypass before the test loop is wired to httpx.
func TestUpstreamClientSpanNestedUnderLoopTick(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := httpx.New(httpx.Config{AllowPrivate: true}) // httptest listens on loopback.
	loop := &upstreamNestingLoop{
		key:    model.CheckpointKey{SourceInstance: "test-source", Loop: "test-loop", OutputFingerprint: "test"},
		target: server.URL,
		do:     client.Do,
	}
	runner := NewLoopRunner(loop, upstreamNestingEmitter{}, upstreamNestingCheckpoint{}, source.NewGuard(source.GuardConfig{}), 1, 1, NoopMetrics{})
	scheduler := NewScheduler(nil, NoopMetrics{})
	scheduler.RunOnceForTest(context.Background(), LoopSpec{
		Runner: runner, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour,
	}, time.Unix(1_700_000_000, 0).UTC())

	if !loop.collected {
		t.Fatal("Collect was not called")
	}
	_ = provider.ForceFlush(context.Background())
	assertUpstreamSpanAncestry(t, recorder.Ended())
}

type upstreamNestingLoop struct {
	key       model.CheckpointKey
	target    string
	do        func(*http.Request) (*http.Response, error)
	collected bool
}

func (l *upstreamNestingLoop) Key() model.CheckpointKey { return l.key }
func (l *upstreamNestingLoop) Cadence() time.Duration   { return time.Minute }

func (l *upstreamNestingLoop) Collect(ctx context.Context, _ model.Watermark) (model.Batch, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.target, nil)
	if err != nil {
		return model.Batch{}, err
	}
	resp, err := l.do(req)
	if err != nil {
		return model.Batch{}, err
	}
	defer resp.Body.Close()
	l.collected = true
	return model.Batch{Key: l.key, Watermark: model.Watermark{Time: time.Unix(1_700_000_000, 0).UTC()}}, nil
}

type upstreamNestingEmitter struct{}

func (upstreamNestingEmitter) Emit(context.Context, model.Batch) error { return nil }

var _ emit.Emitter = upstreamNestingEmitter{}

type upstreamNestingCheckpoint struct{}

func (upstreamNestingCheckpoint) Load(context.Context, model.CheckpointKey) (model.Watermark, error) {
	return model.Watermark{}, nil
}

func (upstreamNestingCheckpoint) Save(context.Context, model.CheckpointKey, model.Watermark) error {
	return nil
}

func assertUpstreamSpanAncestry(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	byID := make(map[trace.SpanID]sdktrace.ReadOnlySpan, len(spans))
	var client sdktrace.ReadOnlySpan
	for _, span := range spans {
		byID[span.SpanContext().SpanID()] = span
		if span.SpanKind() == trace.SpanKindClient {
			client = span
		}
	}
	if client == nil {
		t.Fatal("no upstream CLIENT span recorded: source HTTP must use the shared otelhttp-wrapped httpx client")
	}
	for current := client; ; {
		parent := current.Parent()
		if !parent.IsValid() {
			t.Fatalf("CLIENT span %q has no loop.tick ancestor", client.Name())
		}
		ancestor, ok := byID[parent.SpanID()]
		if !ok {
			t.Fatalf("CLIENT span %q parent %s was not recorded", client.Name(), parent.SpanID())
		}
		if ancestor.Name() == "loop.tick" {
			return
		}
		current = ancestor
	}
}
