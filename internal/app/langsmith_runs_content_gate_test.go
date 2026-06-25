// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// runsSecretMarkers are DISTINCTIVE values placed in the content fields LangSmith returns on a run
// record regardless of `select` (PROBED: select is not an egress filter on 0.13.5). The release gate
// asserts NONE of these bytes survive into the emitted OTLP logs — content cannot leak via any field
// (indexed attr, record attr, body) or the query projection.
var runsSecretMarkers = []string{
	"ZZINPUTSZZ", "ZZOUTPUTSZZ", "ZZPREVIEWZZ", "ZZMSGZZ", "ZZEVENTZZ",
	"ZZEXTRAZZ", "ZZSERIALZZ", "ZZERRORTXTZZ", "ZZRUNNAMEZZ",
}

// injectedRunsResponse is one realistic runs/query page: content-free operational fields PLUS the
// content blobs (inputs/outputs/messages/events/extra/serialized/error/name) each carrying a marker.
// cursors.next is null ⇒ the window completes in one Collect.
func injectedRunsResponse(t *testing.T) string {
	t.Helper()
	run := map[string]any{
		"id": "r1", "trace_id": "t1", "session_id": "s1",
		"run_type": "llm", "status": "success", "trace_tier": "longlived",
		"start_time": "2026-06-18T14:23:45.123456", "total_tokens": 1312, "total_cost": 0.0194,
		"tags":                 []any{"prod", "eu"}, // operational scalar array — opt-in-able as a csv record attr
		"reference_dataset_id": "ds-eval-42",        // content-free low-card op id — opt-in-able to the INDEXED tier
		"inputs":               map[string]any{"messages": []any{"ZZINPUTSZZ"}},
		"outputs":              map[string]any{"choices": []any{"ZZOUTPUTSZZ"}},
		"inputs_preview":       "ZZPREVIEWZZ",
		"messages":             []any{"ZZMSGZZ"},
		"events":               []any{map[string]any{"x": "ZZEVENTZZ"}},
		"extra":                map[string]any{"k": "ZZEXTRAZZ"},
		"serialized":           map[string]any{"k": "ZZSERIALZZ"},
		"error":                "ZZERRORTXTZZ",
		"name":                 "ZZRUNNAMEZZ",
	}
	b, err := json.Marshal(map[string]any{"runs": []any{run}, "cursors": map[string]any{"next": nil}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type fakeRuns struct {
	*httptest.Server
	mu      sync.Mutex
	queries []map[string]json.RawMessage
}

func newFakeRuns(t *testing.T) *fakeRuns {
	f := &fakeRuns{}
	body := injectedRunsResponse(t)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/runs/query" {
			var q map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&q)
			f.mu.Lock()
			f.queries = append(f.queries, q)
			f.mu.Unlock()
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(f.Close)
	return f
}

func langsmithRunsConfig(baseURL string) *config.Config {
	return &config.Config{
		Emit:     config.EmitConfig{Telemetry: config.OTLPTarget{OTLP: config.OTLPConn{Endpoint: "https://otlp.example", InstanceID: "1", Token: "t"}}},
		Identity: config.IdentityConfig{ServiceNamespace: "decant", DeploymentEnvironment: "dev"},
		HA:       config.HAConfig{Coordinator: "none", Checkpoint: "file"},
		Queue:    config.QueueConfig{MaxBatches: 4, MaxBatchBytes: 1 << 20, EmitWorkers: 1},
		Sources: []config.SourceConfig{{
			Type: "langsmith", Enabled: true, BaseURL: baseURL, SourceInstance: "ls-test",
			Auth:      config.AuthConfig{Header: "x-api-key", Value: "k"},
			RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
			HTTP:      config.HTTPConfig{AllowPrivate: true},
			Loops: map[string]config.LoopConfig{"runs": {
				Enabled: true, Cadence: config.Duration(time.Minute),
				Settings: map[string]string{"session_ids": "s1", "window": "1h", "settle": "10m"},
			}},
		}},
	}
}

// TestLangsmithRunsContentLeakConformanceGate is the RELEASE GATE for the runs logs loop (Cdx-C7, §7a).
// It drives the WHOLE production path — Build (real guard config) → loop strip → source.Guard.SanitizeLogs
// → otlp.EncodeLogs → emitter routing to /v1/logs — for a run record carrying injected content, and
// asserts NONE of the markers survive into the emitted OTLP bytes, a content-free value (run_type) DID
// survive, and the query projection named no content field. If this fails, the runs loop must NOT ship.
func TestLangsmithRunsContentLeakConformanceGate(t *testing.T) {
	f := newFakeRuns(t)

	var mu sync.Mutex
	var logBodies [][]byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			raw, _ := io.ReadAll(r.Body)
			body := raw
			if r.Header.Get("Content-Encoding") == "gzip" {
				if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
					body, _ = io.ReadAll(zr)
				}
			}
			mu.Lock()
			logBodies = append(logBodies, body)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer gw.Close()

	cfg := langsmithRunsConfig(f.URL)
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	em := otlp.New(otlp.Config{Endpoint: gw.URL, InstanceID: "1", Token: "t",
		Identity: map[string]string{"service.namespace": "decant"}, MaxBytes: 1 << 20})
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, em, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	specs := a.Specs()
	if len(specs) != 1 {
		t.Fatalf("specs=%d want 1 (runs)", len(specs))
	}
	sp := specs[0]

	leaderCtx := coordinate.WithEpoch(context.Background(), 1)
	since := model.Watermark{}
	emitted := false
	for step := range 8 {
		b, cerr := sp.Loop.Collect(leaderCtx, since)
		if cerr != nil {
			t.Fatalf("step %d: Collect: %v", step, cerr)
		}
		if len(b.Logs) > 0 {
			emitted = true
		}
		sp.Runner.ProcessBatch(leaderCtx, b)
		next, _ := sp.Runner.Since(leaderCtx)
		since = next
		mu.Lock()
		got := len(logBodies)
		mu.Unlock()
		if got > 0 {
			break
		}
	}
	if !emitted {
		t.Fatal("no log records were produced by the loop")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logBodies) == 0 {
		t.Fatal("no /v1/logs payload reached the gateway through the wired path (guard dropped every record?)")
	}
	for _, body := range logBodies {
		for _, marker := range runsSecretMarkers {
			if bytes.Contains(body, []byte(marker)) {
				t.Fatalf("CONTENT LEAK: secret marker %q found in emitted /v1/logs payload (governance failed)", marker)
			}
		}
		if !bytes.Contains(body, []byte("llm")) {
			t.Fatalf("expected the content-free run_type value to survive in the payload; got none")
		}
	}

	// The query projection must never NAME a content field.
	if len(f.queries) == 0 {
		t.Fatal("no runs/query request captured")
	}
	sel, _ := json.Marshal(f.queries[0]["select"])
	for _, banned := range []string{"inputs", "outputs", "messages", "events", "extra", "serialized",
		"inputs_preview", "outputs_preview", "error", "name", "manifest", "s3_urls"} {
		if strings.Contains(string(sel), `"`+banned+`"`) {
			t.Fatalf("runs/query select named content field %q: %s", banned, sel)
		}
	}
}

// TestLangsmithRunsExtraRecordFieldFlows proves content-governance is CONFIGURABLE: an operator can opt
// extra fields into the record allow-list (settings.extra_record_fields) and they flow end-to-end —
// including a scalar array as a csv (tags → "prod,eu") and a formerly-hard-denied free-text field
// (error), which only flows because fork-1 shrank the app denylist to message-bodies only. The hard
// backstop still holds: message bodies (inputs/outputs/messages) that were NOT opted in never leak, and
// `name` (not opted in) is dropped by the default-deny strip.
func TestLangsmithRunsExtraRecordFieldFlows(t *testing.T) {
	f := newFakeRuns(t)

	var mu sync.Mutex
	var logBodies [][]byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			raw, _ := io.ReadAll(r.Body)
			body := raw
			if r.Header.Get("Content-Encoding") == "gzip" {
				if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
					body, _ = io.ReadAll(zr)
				}
			}
			mu.Lock()
			logBodies = append(logBodies, body)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer gw.Close()

	cfg := langsmithRunsConfig(f.URL)
	// Opt in: tags (scalar array → csv) + error (free text, formerly hard-denied — now governed by the strip).
	cfg.Sources[0].Loops["runs"].Settings["extra_record_fields"] = "tags, error"

	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	em := otlp.New(otlp.Config{Endpoint: gw.URL, InstanceID: "1", Token: "t",
		Identity: map[string]string{"service.namespace": "decant"}, MaxBytes: 1 << 20})
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, em, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	sp := a.Specs()[0]

	leaderCtx := coordinate.WithEpoch(context.Background(), 1)
	since := model.Watermark{}
	for step := range 8 {
		b, cerr := sp.Loop.Collect(leaderCtx, since)
		if cerr != nil {
			t.Fatalf("step %d: Collect: %v", step, cerr)
		}
		sp.Runner.ProcessBatch(leaderCtx, b)
		next, _ := sp.Runner.Since(leaderCtx)
		since = next
		mu.Lock()
		got := len(logBodies)
		mu.Unlock()
		if got > 0 {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logBodies) == 0 {
		t.Fatal("no /v1/logs payload reached the gateway (opted-in fields caused the record to be dropped?)")
	}
	joined := bytes.Join(logBodies, []byte{})
	// Opted-in fields flow:
	if !bytes.Contains(joined, []byte("prod,eu")) {
		t.Fatal("expected opted-in tags to flow as a csv record attr (prod,eu)")
	}
	if !bytes.Contains(joined, []byte("ZZERRORTXTZZ")) {
		t.Fatal("expected opted-in free-text error to flow (proves the denylist shrink to message-bodies only)")
	}
	// Hard backstop still holds — message bodies NOT opted in never leak; name (not opted in) is dropped:
	for _, marker := range []string{"ZZINPUTSZZ", "ZZOUTPUTSZZ", "ZZMSGZZ", "ZZRUNNAMEZZ", "ZZEVENTZZ"} {
		if bytes.Contains(joined, []byte(marker)) {
			t.Fatalf("CONTENT LEAK: %q leaked despite not being opted in", marker)
		}
	}
}

// TestLangsmithRunsExtraIndexedFieldFlows proves the INDEXED-tier opt-in composes end-to-end: a
// content-free field promoted via settings.extra_indexed_fields is emitted as an indexed attr AND the
// record is NOT dropped — i.e. the composition root auto-allow-listed the promoted key in the guard (a
// regression here would make the guard's okLog drop the WHOLE record on an un-allow-listed indexed key).
// The content floor still holds: message bodies never leak.
func TestLangsmithRunsExtraIndexedFieldFlows(t *testing.T) {
	f := newFakeRuns(t)

	var mu sync.Mutex
	var logBodies [][]byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			raw, _ := io.ReadAll(r.Body)
			body := raw
			if r.Header.Get("Content-Encoding") == "gzip" {
				if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
					body, _ = io.ReadAll(zr)
				}
			}
			mu.Lock()
			logBodies = append(logBodies, body)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer gw.Close()

	cfg := langsmithRunsConfig(f.URL)
	// Promote a content-free low-card op id into the INDEXED (Loki stream-label) tier.
	cfg.Sources[0].Loops["runs"].Settings["extra_indexed_fields"] = "reference_dataset_id"

	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	em := otlp.New(otlp.Config{Endpoint: gw.URL, InstanceID: "1", Token: "t",
		Identity: map[string]string{"service.namespace": "decant"}, MaxBytes: 1 << 20})
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, em, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	sp := a.Specs()[0]

	leaderCtx := coordinate.WithEpoch(context.Background(), 1)
	since := model.Watermark{}
	for step := range 8 {
		b, cerr := sp.Loop.Collect(leaderCtx, since)
		if cerr != nil {
			t.Fatalf("step %d: Collect: %v", step, cerr)
		}
		sp.Runner.ProcessBatch(leaderCtx, b)
		next, _ := sp.Runner.Since(leaderCtx)
		since = next
		mu.Lock()
		got := len(logBodies)
		mu.Unlock()
		if got > 0 {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logBodies) == 0 {
		t.Fatal("no /v1/logs payload reached the gateway — the record was DROPPED (the promoted indexed key was not auto-allow-listed in the guard?)")
	}
	joined := bytes.Join(logBodies, []byte{})
	if !bytes.Contains(joined, []byte("ds-eval-42")) {
		t.Fatal("expected the promoted reference_dataset_id value to flow as an indexed attr")
	}
	for _, marker := range []string{"ZZINPUTSZZ", "ZZOUTPUTSZZ", "ZZMSGZZ", "ZZRUNNAMEZZ"} {
		if bytes.Contains(joined, []byte(marker)) {
			t.Fatalf("CONTENT LEAK: %q leaked", marker)
		}
	}
}
