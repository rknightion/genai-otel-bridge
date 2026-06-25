// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// TestEffectiveContentDenylist verifies the per-deployment denylist computation (review HIGH-1): a
// default deployment (no opt-in) keeps the FULL backstop (floor + every gray field); opting a gray field
// in releases ONLY that field; the never-subtractable floor (message bodies + injected PII) is denied
// regardless of opt-in — so opting in a body never weakens the floor.
func TestEffectiveContentDenylist(t *testing.T) {
	// no opt-in → full backstop: floor + gray both denied.
	base := contentDenylist(map[string]bool{})
	for _, floor := range source.AbsoluteNeverDenyKeys() {
		if !slices.Contains(base, floor) {
			t.Errorf("floor key %q must always be denied", floor)
		}
	}
	for _, gray := range []string{"error", "events", "name", "inputs_preview"} {
		if !slices.Contains(base, gray) {
			t.Errorf("default (no opt-in) deployment must keep gray backstop key %q", gray)
		}
	}

	// opt in a gray field → released, but the rest of the backstop is intact.
	opted := contentDenylist(map[string]bool{"error": true})
	if slices.Contains(opted, "error") {
		t.Error("opted-in gray field error must be released from the effective denylist")
	}
	if !slices.Contains(opted, "events") {
		t.Error("a non-opted gray field (events) must stay on the denylist")
	}

	// opting in a FLOOR field (a body) must NOT release it — the floor is never subtracted.
	floorOpt := contentDenylist(map[string]bool{"inputs": true, "metadata": true})
	if !slices.Contains(floorOpt, "inputs") || !slices.Contains(floorOpt, "metadata") {
		t.Error("floor keys (inputs/metadata) must remain denied even if named in extra_record_fields")
	}

	// optedInRecordFields unions across enabled loops and ignores disabled ones / blanks.
	cfg := &config.Config{Sources: []config.SourceConfig{{
		Enabled: true,
		Loops: map[string]config.LoopConfig{
			"runs":  {Enabled: true, Settings: map[string]string{"extra_record_fields": " tags , error "}},
			"other": {Enabled: false, Settings: map[string]string{"extra_record_fields": "secret"}},
		},
	}}}
	got := optedInRecordFields(cfg)
	if !got["tags"] || !got["error"] {
		t.Errorf("expected tags+error opted in, got %v", got)
	}
	if got["secret"] {
		t.Error("a disabled loop's extra_record_fields must be ignored")
	}
}

// minimalConfig: one enabled portkey source, analytics loop, file checkpoint, no coordinator.
func minimalConfig(baseURL string) *config.Config {
	return &config.Config{
		Emit:     config.EmitConfig{Telemetry: config.OTLPTarget{OTLP: config.OTLPConn{Endpoint: "https://otlp.example", InstanceID: "1", Token: "t"}}},
		Identity: config.IdentityConfig{ServiceNamespace: "decant", DeploymentEnvironment: "dev"},
		HA:       config.HAConfig{Coordinator: "none", Checkpoint: "file"},
		Queue:    config.QueueConfig{MaxBatches: 4, MaxBatchBytes: 1 << 20, EmitWorkers: 1},
		Sources: []config.SourceConfig{{
			Type: "portkey", Enabled: true, BaseURL: baseURL, SourceInstance: "pk-test",
			Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
			RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
			HTTP:      config.HTTPConfig{AllowPrivate: true},
			Loops: map[string]config.LoopConfig{"analytics": {
				Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
				BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
				MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
				Graphs: []string{"requests"},
			}},
		}},
	}
}

func TestBuildAndOneEmitCycle(t *testing.T) {
	// Fake Portkey: 1-min buckets relative to NOW (so they fall inside the live poll window and the
	// max_backfill floor, avoiding the clock-dependence the plan flagged).
	pk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		var dps []map[string]any
		for i := 9; i >= 1; i-- {
			ts := now.Add(-time.Duration(i) * time.Minute).Truncate(time.Minute)
			dps = append(dps, map[string]any{"timestamp": ts.Format(time.RFC3339), "total": i})
		}
		json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": 45}, "data_points": dps, "is_quota_exceeded": false})
	}))
	defer pk.Close()
	// Fake OTLP gateway: count POSTs to /v1/metrics.
	var posts int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			atomic.AddInt32(&posts, 1)
		}
		w.WriteHeader(200)
	}))
	defer gw.Close()

	cfg := minimalConfig(pk.URL)
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	em := otlp.New(otlp.Config{Endpoint: gw.URL, InstanceID: "1", Token: "t", Identity: map[string]string{"service.namespace": "decant"}, MaxBytes: 1 << 20})
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, em, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	specs := a.Specs()
	if len(specs) != 1 {
		t.Fatalf("specs=%d want 1", len(specs))
	}
	// Drive one tick deterministically: Collect → process. since=zero ⇒ bootstrap lookback.
	sp := specs[0]
	leaderCtx := coordinate.WithEpoch(context.Background(), 1)
	b, err := sp.Loop.Collect(leaderCtx, model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	sp.Runner.ProcessBatch(leaderCtx, b)
	if atomic.LoadInt32(&posts) == 0 {
		t.Fatal("no OTLP POST reached the gateway through the wired path")
	}
	if got, _ := cp.Load(context.Background(), sp.Loop.Key()); got.Time.IsZero() {
		t.Fatal("watermark not advanced after a successful emit cycle")
	}
}

type noopEmitter struct{}

func (noopEmitter) Emit(context.Context, model.Batch) error { return nil }

func buildMinimalApp(t *testing.T) *App {
	t.Helper()
	cfg := minimalConfig("http://127.0.0.1:1") // unreachable; Collect errors are logged, not fatal
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// [R3-4] A health-server bind failure (occupied port) must propagate out of Run, not be swallowed.
func TestRunReturnsOnHealthBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a := buildMinimalApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := a.Run(ctx, selfobs.NewHealth(time.Minute).Handler(), ln.Addr().String(), func() {}, func() {}, func(bool) {})
	if runErr == nil || !strings.Contains(runErr.Error(), "health server bind") {
		t.Fatalf("expected a health-server bind failure to propagate, got %v", runErr)
	}
}
