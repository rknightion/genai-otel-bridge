// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// partialRejectWarning is the partial-reject warning text main logged before the factories existed.
// It is a literal so a reworded warning fails here.
const partialRejectWarning = "OTLP gateway rejected part of an emit batch via a 200 partial_success response; the rejected data was NOT delivered (alert on genai_otel_bridge_emit_partial_success_rejected_total)"

// TestPartialRejectLimitKey pins the warning's rate-limit key: today's per-plane key with no pipeline,
// and the pipeline id in it otherwise, so one tenant's rejections cannot throttle another's warning.
func TestPartialRejectLimitKey(t *testing.T) {
	for _, tc := range []struct{ pipeline, plane, want string }{
		{"", "metrics", "partial:metrics"},
		{"", "logs", "partial:logs"},
		{"team-a.prod", "logs", "partial:team-a.prod:logs"},
		{"team-a.prod", "metrics", "partial:team-a.prod:metrics"},
	} {
		if got := partialRejectLimitKey(tc.pipeline, tc.plane); got != tc.want {
			t.Errorf("partialRejectLimitKey(%q, %q) = %q, want %q", tc.pipeline, tc.plane, got, tc.want)
		}
	}
}

// emitRecorder is a PipelineTelemetry that records the three request observers.
type emitRecorder struct {
	noopTelemetry
	mu       sync.Mutex
	emits    []string // plane of each ObserveEmitRequest
	rejected map[string]int64
	upstream []string // target of each ObserveUpstreamRequest
	codes    []int
}

func (r *emitRecorder) ObserveEmitRequest(plane string, _ int, _ error, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emits = append(r.emits, plane)
}

func (r *emitRecorder) ObserveEmitPartialReject(plane string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rejected == nil {
		r.rejected = map[string]int64{}
	}
	r.rejected[plane] += n
}

func (r *emitRecorder) ObserveUpstreamRequest(target, _ string, code int, _ error, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upstream = append(r.upstream, target)
	r.codes = append(r.codes, code)
}

// partialSuccessGateway answers every OTLP POST with 200 and a partial_success body rejecting 3 items
// (ExportMetricsServiceResponse field 1: rejected_* field 1, error_message field 2).
func partialSuccessGateway(t *testing.T) *httptest.Server {
	t.Helper()
	var ps []byte
	ps = protowire.AppendTag(ps, 1, protowire.VarintType)
	ps = protowire.AppendVarint(ps, 3)
	ps = protowire.AppendTag(ps, 2, protowire.BytesType)
	ps = protowire.AppendBytes(ps, []byte("3 data points rejected"))
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, ps)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureWarnings swaps the process-global slog default for a JSON handler and returns a reader of the
// partial-reject warnings logged since. Tests using it are not parallel.
func captureWarnings(t *testing.T) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatalf("log line is not JSON: %q", line)
			}
			if rec["msg"] == partialRejectWarning {
				out = append(out, rec)
			}
		}
		return out
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func oneSampleBatch() model.Batch {
	return model.Batch{Samples: []model.Sample{{Name: "bridge_test_requests", Kind: model.Gauge, Value: 1, Timestamp: time.Unix(1_700_000_000, 0).UTC()}}}
}

func fastRetry() otlp.RetryPolicy {
	return otlp.RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 30 * time.Millisecond}
}

func warningKeys(rec map[string]any) []string {
	var keys []string
	for k := range rec {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestOTLPEmitterFactoryWarningAttrs: the partial-reject warning a legacy spec logs carries exactly
// today's attributes (plane, rejected, gateway_message), and a spec with a pipeline adds tenant and
// pipeline (design §8). The partial-success counter and the emit-request observer reach the
// pipeline's telemetry either way.
func TestOTLPEmitterFactoryWarningAttrs(t *testing.T) {
	gw := partialSuccessGateway(t)
	for _, tc := range []struct {
		name     string
		spec     EmitterSpec
		wantKeys []string
		want     map[string]any
	}{
		{
			name:     "legacy",
			spec:     EmitterSpec{Endpoint: gw.URL, InstanceID: "1", Token: "t"},
			wantKeys: []string{"gateway_message", "level", "msg", "plane", "rejected", "time"},
			want:     map[string]any{"plane": "metrics", "rejected": float64(3), "gateway_message": "3 data points rejected", "level": "WARN"},
		},
		{
			name:     "pipeline",
			spec:     EmitterSpec{Endpoint: gw.URL, InstanceID: "1", Token: "t", Tenant: "team-a", Pipeline: "team-a.prod"},
			wantKeys: []string{"gateway_message", "level", "msg", "pipeline", "plane", "rejected", "tenant", "time"},
			want:     map[string]any{"plane": "metrics", "rejected": float64(3), "gateway_message": "3 data points rejected", "level": "WARN", "tenant": "team-a", "pipeline": "team-a.prod"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureWarnings(t)
			rec := &emitRecorder{}
			em := OTLPEmitterFactory(1<<20, fastRetry(), logging.NewLimiter(time.Minute))(tc.spec, rec)
			if err := em.Emit(context.Background(), oneSampleBatch()); err != nil {
				t.Fatalf("a 200 partial_success is a delivered emit, got %v", err)
			}
			got := warnings()
			if len(got) != 1 {
				t.Fatalf("want one partial-reject warning, got %d: %v", len(got), got)
			}
			if keys := warningKeys(got[0]); !slices.Equal(keys, tc.wantKeys) {
				t.Errorf("warning attributes = %v, want exactly %v", keys, tc.wantKeys)
			}
			for k, v := range tc.want {
				if got[0][k] != v {
					t.Errorf("warning %s = %v, want %v", k, got[0][k], v)
				}
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.rejected["metrics"] != 3 {
				t.Errorf("ObserveEmitPartialReject recorded %v, want metrics=3", rec.rejected)
			}
			if !slices.Equal(rec.emits, []string{"metrics"}) {
				t.Errorf("ObserveEmitRequest recorded planes %v, want one metrics POST", rec.emits)
			}
		})
	}
}

// TestOTLPEmitterFactoryWarningLimitKey: the factory throttles the warning under
// partialRejectLimitKey, so a key already spent in the shared limiter silences only its own pipeline.
func TestOTLPEmitterFactoryWarningLimitKey(t *testing.T) {
	gw := partialSuccessGateway(t)
	warnings := captureWarnings(t)
	lim := logging.NewLimiter(time.Minute)
	ef := OTLPEmitterFactory(1<<20, fastRetry(), lim)
	legacy := ef(EmitterSpec{Endpoint: gw.URL}, noopTelemetry{})
	piped := ef(EmitterSpec{Endpoint: gw.URL, Tenant: "team-a", Pipeline: "team-a.prod"}, noopTelemetry{})

	if !lim.Allow("partial:metrics") { // spend today's key: the legacy warning is throttled
		t.Fatal("fresh limiter refused a key")
	}
	for _, em := range []emit.Emitter{legacy, piped} {
		if err := em.Emit(context.Background(), oneSampleBatch()); err != nil {
			t.Fatal(err)
		}
	}
	got := warnings()
	if len(got) != 1 || got[0]["pipeline"] != "team-a.prod" {
		t.Fatalf("with partial:metrics spent, want only the pipeline's warning, got %v", got)
	}
	if lim.Allow("partial:team-a.prod:metrics") {
		t.Error("the pipeline's warning must have spent partial:team-a.prod:metrics")
	}
}

// TestBuildWiresUpstreamObserver: Build wires the source HTTP client's observer to the telemetry the
// factory returned (main wired it before the factories existed, so a forgotten move would be silent).
// One tick against a fake source records one upstream observation per request the source received.
func TestBuildWiresUpstreamObserver(t *testing.T) {
	var served atomic.Int64
	pk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": 0}, "data_points": []any{}, "is_quota_exceeded": false})
	}))
	t.Cleanup(pk.Close)
	u, err := url.Parse(pk.URL)
	if err != nil {
		t.Fatal(err)
	}

	cp, err := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	rec := &emitRecorder{}
	var calls [][2]string
	tf := func(tenant, pipeline string) PipelineTelemetry {
		calls = append(calls, [2]string{tenant, pipeline})
		return rec
	}
	ef := func(EmitterSpec, PipelineTelemetry) emit.Emitter { return noopEmitter{} }
	a, err := Build(context.Background(), minimalConfig(pk.URL), cp, coordinate.Noop{}, ef, tf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != [2]string{"", ""} {
		t.Fatalf("the legacy shape asks the telemetry factory once for the unlabelled hooks, got %v", calls)
	}

	sp := a.Specs()[0]
	lc := coordinate.WithEpoch(context.Background(), 1)
	since, err := sp.Runner.Since(lc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sp.Loop.Collect(lc, since)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	sp.Runner.ProcessBatch(lc, b)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if served.Load() == 0 {
		t.Fatal("the tick made no upstream request")
	}
	if int64(len(rec.upstream)) != served.Load() {
		t.Fatalf("recorded %d upstream observations for %d requests served", len(rec.upstream), served.Load())
	}
	for i, target := range rec.upstream {
		if target != u.Hostname() || rec.codes[i] != http.StatusOK {
			t.Errorf("observation %d = (%q, %d), want (%q, 200)", i, target, rec.codes[i], u.Hostname())
		}
	}
}
