// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"google.golang.org/protobuf/encoding/protowire"
)

// partialSuccessBody builds an ExportMetricsServiceResponse / ExportLogsServiceResponse protobuf whose
// partial_success (field 1) carries rejected_* (field 1, varint) and an error_message (field 2, string) —
// the identical wire shape for both planes.
func partialSuccessBody(rejected int64, msg string) []byte {
	var ps []byte
	ps = protowire.AppendTag(ps, 1, protowire.VarintType)
	ps = protowire.AppendVarint(ps, uint64(rejected))
	if msg != "" {
		ps = protowire.AppendTag(ps, 2, protowire.BytesType)
		ps = protowire.AppendBytes(ps, []byte(msg))
	}
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, ps)
	return body
}

// TestEmitCountsPartialSuccessReject (#80) — a 200 whose body carries partial_success.rejected_* > 0
// must fire OnPartialReject with the plane, count, and message for BOTH the metrics and logs planes.
// The 4xx form GC actually uses is handled by classify(); this covers the spec-canonical 200-partial form
// that otherwise bypasses the entire reject taxonomy and counts rejected items as delivered.
func TestEmitCountsPartialSuccessReject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		switch r.URL.Path {
		case "/v1/metrics":
			w.Write(partialSuccessBody(3, "3 data points rejected: out of order"))
		case "/v1/logs":
			w.Write(partialSuccessBody(2, "2 log records rejected"))
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	got := map[string]int64{}
	msgs := map[string]string{}
	em := New(Config{
		Endpoint: srv.URL, InstanceID: "123", Token: "secret-token",
		Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20,
		OnPartialReject: func(plane string, rejected int64, msg string) {
			mu.Lock()
			defer mu.Unlock()
			got[plane] += rejected
			msgs[plane] = msg
		},
	})

	if err := em.Emit(context.Background(), oneBatch()); err != nil {
		t.Fatalf("metrics emit (2xx) must still return success, got %v", err)
	}
	logs := model.Batch{Logs: []model.LogRecord{{Timestamp: time.Unix(1_700_000_000, 0).UTC(), IndexedAttributes: map[string]string{"ai_model": "m"}}}}
	if err := em.Emit(context.Background(), logs); err != nil {
		t.Fatalf("logs emit (2xx) must still return success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got["metrics"] != 3 {
		t.Errorf("metrics plane: rejected_data_points want 3, got %d", got["metrics"])
	}
	if got["logs"] != 2 {
		t.Errorf("logs plane: rejected_log_records want 2, got %d", got["logs"])
	}
	if msgs["metrics"] == "" || msgs["logs"] == "" {
		t.Errorf("gateway error_message must be surfaced to the callback, got metrics=%q logs=%q", msgs["metrics"], msgs["logs"])
	}
}

// TestEmitNoPartialRejectOnCleanSuccess (#80) — a bare 2xx with no body (the normal GC success, incl. the
// /v1/logs 204) and a partial_success with rejected==0 (a spec "warning") must NOT fire OnPartialReject:
// a phantom reject would false-alert. Guards against manufacturing a reject from an empty/zero body.
func TestEmitNoPartialRejectOnCleanSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		code int
	}{
		{"empty-200", nil, 200},
		{"empty-204", nil, 204},
		{"warning-zero-rejected", partialSuccessBody(0, "accepted with a warning"), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				if tc.body != nil {
					w.Write(tc.body)
				}
			}))
			defer srv.Close()
			var fired int
			em := New(Config{
				Endpoint: srv.URL, Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20,
				OnPartialReject: func(string, int64, string) { fired++ },
			})
			if err := em.Emit(context.Background(), oneBatch()); err != nil {
				t.Fatalf("emit must succeed, got %v", err)
			}
			if fired != 0 {
				t.Errorf("OnPartialReject must not fire on a clean/zero-reject 2xx, fired %d times", fired)
			}
		})
	}
}
