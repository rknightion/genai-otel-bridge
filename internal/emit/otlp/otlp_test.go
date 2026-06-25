// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func testEmitter(t *testing.T, url string) *Emitter {
	t.Helper()
	return New(Config{
		Endpoint: url, InstanceID: "123", Token: "secret-token",
		Identity: map[string]string{"service.namespace": "decant"}, MaxBytes: 1 << 20,
		Retry: RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 30 * time.Millisecond, Jitter: 0},
	})
}

func oneBatch() model.Batch {
	return model.Batch{Samples: []model.Sample{{Name: "portkey_api_requests", Kind: model.Gauge, Value: 1, Timestamp: time.Unix(1_700_000_000, 0).UTC()}}}
}

func TestEmitSuccessAndAuthAndGzip(t *testing.T) {
	var sawAuth, sawCE, sawCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawCE, sawCT = r.Header.Get("Authorization"), r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sawAuth, "Basic ") {
		t.Errorf("auth=%q", sawAuth)
	}
	if sawCE != "gzip" || sawCT != "application/x-protobuf" {
		t.Errorf("CE=%q CT=%q", sawCE, sawCT)
	}
}

// TestEmitNoAuthHeaderWhenCredsEmpty: for the [CP-M7] token-less in-cluster hop (emit to an Alloy that
// holds the real Grafana Cloud credentials), no credential rides the link — and an emitter built with
// empty instance_id+token must send NO Authorization header at all (not a useless "Basic Og==").
func TestEmitNoAuthHeaderWhenCredsEmpty(t *testing.T) {
	var sawAuth string
	var hadAuthHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuthHeader = r.Header["Authorization"]
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	em := New(Config{Endpoint: srv.URL, Identity: map[string]string{"service.namespace": "decant"}, MaxBytes: 1 << 20})
	if err := em.Emit(context.Background(), oneBatch()); err != nil {
		t.Fatal(err)
	}
	if hadAuthHeader || sawAuth != "" {
		t.Fatalf("token-less emitter must send no Authorization header, got %q (present=%v)", sawAuth, hadAuthHeader)
	}
}

// TestEmitAcceptsAny2xx: the OTLP/HTTP gateway signals success with ANY 2xx, not only 200 — Grafana
// Cloud returns 200 for /v1/metrics but 204 No Content for /v1/logs. A 204 must be treated as success,
// not misclassified as a retryable failure (the live soak caught this: every successful logs POST got
// 204 → was rejected → the logs loop never advanced). Covers both the samples and logs paths.
func TestEmitAcceptsAny2xx(t *testing.T) {
	for _, code := range []int{200, 201, 202, 204} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
			defer srv.Close()
			em := testEmitter(t, srv.URL)
			if err := em.Emit(context.Background(), oneBatch()); err != nil {
				t.Fatalf("samples: %d must be success, got %v", code, err)
			}
			logs := model.Batch{Logs: []model.LogRecord{{Timestamp: time.Unix(1_700_000_000, 0).UTC(), IndexedAttributes: map[string]string{"ai_model": "m"}}}}
			if err := em.Emit(context.Background(), logs); err != nil {
				t.Fatalf("logs: %d must be success, got %v", code, err)
			}
		})
	}
}

func TestEmitRetriesThenSucceeds(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch()); err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
}

func TestEmit500IsRetryableNotInline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch())
	var re *emit.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("500 must surface as RetryableError (retry next cadence), got %v", err)
	}
}

func TestEmitClassifies400(t *testing.T) {
	// Bodies use the ACTUAL Mimir err-mimir-* codes (the realistic gateway 400s), incl. an
	// unrecognised code that MUST classify as ReasonUnknown (on which the runner halts+degrades, CP-C7),
	// distinct from bad-encoding.
	cases := []struct {
		body   string
		status int
		reason emit.RejectReason
	}{
		{"err-mimir-sample-duplicate-timestamp for series {...}", 400, emit.ReasonDuplicateTimestamp},
		{"err-mimir-sample-out-of-order for series", 400, emit.ReasonTooOld},
		{"err-mimir-sample-timestamp-too-old: the sample has been rejected", 400, emit.ReasonTooOld},
		{"failed to parse: proto: cannot parse invalid wire-format data", 400, emit.ReasonBadEncoding},
		{"err-mimir-some-future-code we have never seen", 400, emit.ReasonUnknown}, // [CP-C7] → runner halts+degrades, not silent advance
		{"request entity too large", 413, emit.ReasonPayloadTooLarge},
		// Loki distributor age/order rejects (logs plane) must classify as ReasonTooOld (advance-past with a
		// counted gap), same as Mimir's — NOT ReasonUnknown (which would HALT the loop). Regression guard for
		// the long-outage logs path: a watermark inside max_backfill but beyond Loki's 7d horizon 400s here.
		{"loki: greater than max age", 400, emit.ReasonTooOld},
		{"loki: entry too far behind, oldest acceptable timestamp is 2026-06-13T00:00:00Z", 400, emit.ReasonTooOld},
		{"loki: entry out of order for stream", 400, emit.ReasonTooOld},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch())
		srv.Close()
		var re *emit.RejectError
		if !errors.As(err, &re) || re.Reason != c.reason {
			t.Errorf("body %q: got %v, want reason %v", c.body, err, c.reason)
		}
	}
}

func TestEmitNeverLeaksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); w.Write([]byte("bad")) }))
	defer srv.Close()
	err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch())
	if err != nil && strings.Contains(err.Error(), "secret-token") {
		t.Fatal("token leaked into error string")
	}
}
