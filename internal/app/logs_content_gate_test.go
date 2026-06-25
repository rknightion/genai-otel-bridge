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

// secretMarkers are DISTINCTIVE values placed in the PII/content fields Portkey injects into an export
// record regardless of requested_data (PoC §3) plus the message-body fields. The release gate asserts
// NONE of these bytes survive into the emitted OTLP logs payload — proving content cannot leak via any
// field (indexed attr, record attr, body) or the create request.
var secretMarkers = []string{
	"ZZOWNERNAMEZZ", "ZZRITM0007ZZ", "ZZCLASSC3ZZ", // metadata.* (customer PII)
	"ZZPORTKEYCFGZZ",                                // portkeyHeaders.* (gateway config)
	"ZZPROMPTBODYZZ", "ZZREQBODYZZ", "ZZRESPBODYZZ", // prompt / request / response (message bodies)
}

// injectedExportRecord is one realistic export JSONL record: content-free operational fields PLUS the
// injected PII/config blobs and message bodies, each carrying a secretMarker.
func injectedExportRecord(t *testing.T) string {
	t.Helper()
	rec := map[string]any{
		"id": "r1", "trace_id": "t1", "created_at": "2026-06-18T14:23:45Z",
		"response_time": 1485, "response_status_code": 200,
		"ai_org": "openai", "ai_model": "gpt-4.1-2025-04-14",
		"cost": 0.3194, "total_units": 1312, "currency": "USD",
		"metadata":       map[string]any{"owner": "ZZOWNERNAMEZZ", "ritm_number": "ZZRITM0007ZZ", "data_classification": "ZZCLASSC3ZZ", "correlation_id": "00112233-4455-6677-8899-aabbccddeeff"},
		"portkeyHeaders": map[string]any{"x-portkey-config": "ZZPORTKEYCFGZZ"},
		"prompt":         "ZZPROMPTBODYZZ",
		"request":        map[string]any{"messages": []any{"ZZREQBODYZZ"}},
		"response":       map[string]any{"choices": []any{"ZZRESPBODYZZ"}},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// fakeLogsExport is a minimal export lifecycle (create→start→poll→download→S3) serving exactly one
// content-injected record on page 0. Capture the create bodies to assert requested_data is content-free.
type fakeLogsExport struct {
	*httptest.Server
	mu      sync.Mutex
	creates []map[string]json.RawMessage
	record  string
}

func newFakeLogsExport(t *testing.T) *fakeLogsExport {
	f := &fakeLogsExport{record: injectedExportRecord(t)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && p == "/logs/exports":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.creates = append(f.creates, body)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-1", "total": 1, "object": "export"})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/start"):
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "queued", "object": "export"})
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/download"):
			_ = json.NewEncoder(w).Encode(map[string]any{"signed_url": f.URL + "/s3/job-1.jsonl", "object": "export"})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/s3/"):
			_, _ = w.Write([]byte(f.record))
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/logs/exports/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-1", "status": "success", "object": "export"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func logsExportConfig(baseURL, gwHost string) *config.Config {
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
			Loops: map[string]config.LoopConfig{"logs_export": {
				Enabled: true, Cadence: config.Duration(time.Minute),
				Settings: map[string]string{
					"workspace_id": "ws-test", "signed_url_allow_hosts": gwHost,
					"window": "1h", "settle": "10m",
					// Lift the content-free correlation_id out of the (otherwise hard-denied) metadata blob,
					// as a record attr AND the OTLP trace_id — the rest of metadata (PII) must still be denied.
					"metadata_record_fields": "correlation_id", "metadata_trace_id_field": "correlation_id",
				},
			}},
		}},
	}
}

// TestLogsExportContentLeakConformanceGate is the RELEASE GATE (Cdx-C7, §7a). It drives the WHOLE
// production path — Build (real guard config) → loop strip → source.Guard.SanitizeLogs → otlp.EncodeLogs
// → emitter routing to /v1/logs — for an export record carrying injected PII/content, and asserts NONE
// of the secret markers survive into the emitted OTLP bytes, AND the create request named no content
// field. If this fails, the logs feature must NOT ship.
func TestLogsExportContentLeakConformanceGate(t *testing.T) {
	f := newFakeLogsExport(t)

	// Fake OTLP gateway: capture the decompressed /v1/logs payloads.
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

	// signed_url_allow_hosts must list the fake export server's host (it serves the S3 object too).
	exportHost := f.Listener.Addr().String()
	if i := strings.LastIndex(exportHost, ":"); i >= 0 {
		exportHost = exportHost[:i]
	}
	cfg := logsExportConfig(f.URL, exportHost)

	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	em := otlp.New(otlp.Config{Endpoint: gw.URL, InstanceID: "1", Token: "t",
		Identity: map[string]string{"service.namespace": "decant"}, MaxBytes: 1 << 20})
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, em, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	specs := a.Specs()
	if len(specs) != 1 {
		t.Fatalf("specs=%d want 1 (logs_export)", len(specs))
	}
	sp := specs[0]

	// Drive the step machine to completion through the REAL runner (guard + emit on every batch).
	leaderCtx := coordinate.WithEpoch(context.Background(), 1)
	since := model.Watermark{}
	emitted := false
	for step := 0; step < 12; step++ {
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
		t.Fatal("no /v1/logs payload reached the gateway through the wired path")
	}
	// THE GATE: no secret marker may appear ANYWHERE in any emitted OTLP logs payload.
	for _, body := range logBodies {
		for _, marker := range secretMarkers {
			if bytes.Contains(body, []byte(marker)) {
				t.Fatalf("CONTENT LEAK: secret marker %q found in emitted /v1/logs payload (governance failed)", marker)
			}
		}
		// Sanity: a content-free operational value DID survive (the strip is not just emitting nothing).
		if !bytes.Contains(body, []byte("gpt-4.1-2025-04-14")) {
			t.Fatalf("expected the content-free ai_model value to survive in the payload; got none")
		}
		// The named metadata sub-key (correlation_id) MUST lift through as a record attr — proving selective
		// extraction works (its PII siblings owner/ritm_number/data_classification are asserted absent above).
		if !bytes.Contains(body, []byte("00112233-4455-6677-8899-aabbccddeeff")) {
			t.Fatalf("expected metadata.correlation_id to lift through as a record attr; got none")
		}
		// ...and as the OTLP trace_id (the 16 raw bytes of the UUID).
		wantTID := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
		if !bytes.Contains(body, wantTID) {
			t.Fatalf("expected metadata.correlation_id to be encoded as the OTLP trace_id bytes; got none")
		}
	}

	// The create request must never NAME a content field in requested_data.
	if len(f.creates) == 0 {
		t.Fatal("no create request captured")
	}
	rd, _ := json.Marshal(f.creates[0]["requested_data"])
	for _, banned := range []string{"prompt", "request", "response", "metadata", "portkeyHeaders", "messages"} {
		if strings.Contains(string(rd), `"`+banned+`"`) {
			t.Fatalf("create requested_data named content field %q: %s", banned, rd)
		}
	}
}
