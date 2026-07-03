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
	for _, gray := range []string{"error", "events", "name", "serialized"} {
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

	// #95: inputs_preview/outputs_preview are LLM-content previews → now on the never-subtractable floor.
	// They stay denied even when explicitly opted in via extra_record_fields.
	prevOpt := contentDenylist(map[string]bool{"inputs_preview": true, "outputs_preview": true})
	for _, p := range []string{"inputs_preview", "outputs_preview"} {
		if !slices.Contains(source.AbsoluteNeverDenyKeys(), p) {
			t.Errorf("%q must be a content floor key (#95)", p)
		}
		if !slices.Contains(prevOpt, p) {
			t.Errorf("content-preview floor key %q must remain denied even if opted in (#95)", p)
		}
	}

	// #51: optedInContentFieldsForLoop reads ALL THREE knobs from a SINGLE loop's settings, trimming blanks.
	lc := config.LoopConfig{Settings: map[string]string{"extra_record_fields": " tags , error ", "extra_indexed_fields": "name", "metadata_record_fields": "correlation_id"}}
	got := optedInContentFieldsForLoop(lc)
	if !got["tags"] || !got["error"] || !got["name"] || !got["correlation_id"] {
		t.Errorf("expected tags+error (record) + name (indexed) + correlation_id (metadata) opted in, got %v", got)
	}
	if got[""] {
		t.Error("blank/empty opt-in fields must be ignored")
	}
}

// fakeLoop is a minimal source.Loop double used to unit-test the composition root's per-loop denylist
// wiring (#130) without standing up a real vendor source. When logs==true it also implements
// source.IndexedKeyDeclarer (the capability the two real logs loops expose), marking it a logs loop.
type fakeLoop struct {
	key model.CheckpointKey
}

func (f fakeLoop) Key() model.CheckpointKey { return f.key }
func (f fakeLoop) Cadence() time.Duration   { return time.Minute }
func (f fakeLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, nil
}

type fakeLogsLoop struct{ fakeLoop }

func (fakeLogsLoop) IndexedKeys() []string { return []string{"ai_model"} }

// TestContentDenyByLoopScoping (#130) is the composition-root half of the fix: only LOGS loops (those
// exposing IndexedKeyDeclarer) get a per-loop denylist entry, and each releases ONLY the gray fields IT
// opted into its own knobs. A metrics loop that merely warns extra_record_fields away as an unknown
// setting gets NO entry, so its stray opt-in can never subtract a gray key from any loop's backstop.
func TestContentDenyByLoopScoping(t *testing.T) {
	logsA := fakeLogsLoop{fakeLoop{key: model.CheckpointKey{SourceInstance: "pk", Loop: "logs_export", OutputFingerprint: "f"}}}
	logsB := fakeLogsLoop{fakeLoop{key: model.CheckpointKey{SourceInstance: "ls", Loop: "runs", OutputFingerprint: "f"}}}
	metrics := fakeLoop{key: model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "f"}}

	loops := []loopConfigured{
		{loop: logsA, cfg: config.LoopConfig{Settings: map[string]string{"extra_record_fields": "error"}}}, // A releases error
		{loop: logsB, cfg: config.LoopConfig{}}, // B releases nothing
		{loop: metrics, cfg: config.LoopConfig{Settings: map[string]string{"extra_record_fields": "error"}}}, // metrics: must be ignored
	}
	byLoop := contentDenyByLoop(loops)

	// Metrics loop must have NO entry — its opt-in has no effect on the guard (acceptance criterion 2).
	if _, ok := byLoop[metrics.Key().String()]; ok {
		t.Errorf("metrics loop must NOT get a per-loop denylist entry; got one: %v", byLoop[metrics.Key().String()])
	}
	// Loop A released "error" — absent from A's denylist; "events" (a gray field it did not opt in) present.
	dA := byLoop[logsA.Key().String()]
	if slices.Contains(dA, "error") {
		t.Errorf("loop A opted error in — it must be released from A's denylist; got %v", dA)
	}
	if !slices.Contains(dA, "events") {
		t.Errorf("loop A did not opt events in — it must stay denied for A; got %v", dA)
	}
	// Loop B released nothing — the FULL gray backstop (incl. error) stays denied for B (criterion 1).
	dB := byLoop[logsB.Key().String()]
	if !slices.Contains(dB, "error") {
		t.Errorf("loop B opted nothing in — A's opt-in must not release error for B; got %v", dB)
	}
	// The floor is never subtracted from any per-loop entry.
	for _, floor := range source.AbsoluteNeverDenyKeys() {
		if !slices.Contains(dA, floor) {
			t.Errorf("floor key %q must remain in loop A's denylist", floor)
		}
	}
}

// minimalConfig: one enabled portkey source, analytics loop, file checkpoint, no coordinator.
func minimalConfig(baseURL string) *config.Config {
	return &config.Config{
		Emit:     config.EmitConfig{Telemetry: config.OTLPTarget{OTLP: config.OTLPConn{Endpoint: "https://otlp.example", InstanceID: "1", Token: "t"}}},
		Identity: config.IdentityConfig{ServiceNamespace: "genai-otel-bridge", DeploymentEnvironment: "dev"},
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
	em := otlp.New(otlp.Config{Endpoint: gw.URL, InstanceID: "1", Token: "t", Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20})
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

// TestBuildRejectsUnknownLoopName (#40): a typo'd enabled loop name (e.g. `log_export` for
// `logs_export`) passes cfg.Validate (its cadence/window are even checked) but the source never builds
// it. Build must reconcile configured-enabled loop names against what the source actually constructed
// and fail fast, so the typo can't silently leave a whole plane never collecting.
func TestBuildRejectsUnknownLoopName(t *testing.T) {
	cfg := minimalConfig("http://127.0.0.1:1")
	cfg.Sources[0].Loops["log_export"] = config.LoopConfig{ // typo of logs_export
		Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
		BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
		MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api", Graphs: []string{"requests"},
	}
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	_, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err == nil || !strings.Contains(err.Error(), "log_export") {
		t.Fatalf("expected Build to reject the typo'd loop name log_export, got %v", err)
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
