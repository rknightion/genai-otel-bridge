// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20,
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
	em := New(Config{Endpoint: srv.URL, Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20})
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

// TestEmitDoesNotFollowRedirects [#29] — the emit leg carries the actual data, so it must NOT follow
// redirects: Go's default policy rewrites a 301/302/303 POST into a body-less GET, and a 2xx from the
// redirect target would otherwise be recorded as a successful emit → the watermark advances past a
// window whose payload never reached the gateway (permanent silent data loss). The emitter must surface
// the 3xx as a loud, non-nil, non-advancing error, and the redirect target must never receive a request.
func TestEmitDoesNotFollowRedirects(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var methods []string
			var followed atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method)
				if r.URL.Path == "/final" {
					followed.Store(true) // the redirect was followed — must never happen
					w.WriteHeader(200)
					return
				}
				w.Header().Set("Location", "/final")
				w.WriteHeader(code)
			}))
			defer srv.Close()
			err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch())
			if err == nil {
				t.Fatalf("status %d: Emit returned nil — a redirect was silently treated as success", code)
			}
			if followed.Load() {
				t.Fatalf("status %d: emit client followed the redirect to /final (methods=%v)", code, methods)
			}
		})
	}
}

// emitterWithMaxBytes builds a test emitter with an explicit MaxBytes (testEmitter hard-codes 1<<20,
// which never triggers a proactive split for small batches).
func emitterWithMaxBytes(url string, maxBytes int) *Emitter {
	return New(Config{
		Endpoint: url, InstanceID: "123", Token: "secret-token",
		Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: maxBytes,
		Retry: RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 30 * time.Millisecond, Jitter: 0},
	})
}

// uniqueSamples builds n samples with distinct, fixed-width metric names (m_0000, m_0001, …) so each
// name is an unambiguous marker in the encoded protobuf — no name is a substring of another — letting a
// server count exactly-once delivery by substring match after gunzip.
func uniqueSamples(n int) []model.Sample {
	out := make([]model.Sample, n)
	for i := range out {
		out[i] = model.Sample{
			Name:      fmt.Sprintf("portkey_api_m_%04d", i),
			Kind:      model.Gauge,
			Value:     float64(i),
			Timestamp: time.Unix(1_700_000_000+int64(i), 0).UTC(),
		}
	}
	return out
}

func sampleNames(s []model.Sample) []string {
	out := make([]string, len(s))
	for i := range s {
		out[i] = s[i].Name
	}
	return out
}

// assertEachDeliveredOnce fails unless every name appears EXACTLY once across the accumulated
// (gunzipped) request bodies — i.e. the split delivered the full sample set with no drops or dupes.
func assertEachDeliveredOnce(t *testing.T, body []byte, names []string) {
	t.Helper()
	for _, n := range names {
		if c := bytes.Count(body, []byte(n)); c != 1 {
			t.Fatalf("sample %q delivered %d times across all requests, want exactly 1", n, c)
		}
	}
}

// TestSamples413TriggersReactiveSplit [CP-C11 / #137] mirrors TestLogs413TriggersReactiveSplit for the
// METRICS plane: emitSamples' reactive 413 midpoint recursion had no test (TestEmitClassifies400's 413
// case uses a single sample, short-circuiting the len(samples)>1 split). A multi-sample batch whose first
// whole POST 413s must recursively split and re-deliver every sample EXACTLY once across the accepted
// (2xx) requests — never dropping a bucket and never double-emitting one.
func TestSamples413TriggersReactiveSplit(t *testing.T) {
	var reqCount int32
	var mu sync.Mutex
	var acc []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&reqCount, 1) == 1 {
			w.WriteHeader(413) // reject the whole batch once → force the reactive midpoint split
			return
		}
		// Record only ACCEPTED requests, so the rejected (re-split) batch is not double-counted.
		if zr, err := gzip.NewReader(r.Body); err == nil {
			if b, err := io.ReadAll(zr); err == nil {
				mu.Lock()
				acc = append(acc, b...)
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	samples := uniqueSamples(4)
	if err := testEmitter(t, srv.URL).Emit(context.Background(), model.Batch{Samples: samples}); err != nil {
		t.Fatalf("expected success after 413-triggered split, got %v", err)
	}
	if got := atomic.LoadInt32(&reqCount); got < 3 {
		t.Fatalf("reactive split should produce the rejected request + ≥2 smaller ones, got %d", got)
	}
	assertEachDeliveredOnce(t, acc, sampleNames(samples))
}

// TestSamplesProactiveMaxBytesSplit [CP-C11 / #137] covers the proactive branch for the METRICS plane:
// with a tiny MaxBytes every multi-sample gzipped payload exceeds the cap, so emitSamples must split
// BEFORE transmit (no 413 needed), recursing to singletons, and still deliver every sample exactly once.
func TestSamplesProactiveMaxBytesSplit(t *testing.T) {
	var reqCount int32
	var mu sync.Mutex
	var acc []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		if zr, err := gzip.NewReader(r.Body); err == nil {
			if b, err := io.ReadAll(zr); err == nil {
				mu.Lock()
				acc = append(acc, b...)
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	samples := uniqueSamples(4)
	if err := emitterWithMaxBytes(srv.URL, 1).Emit(context.Background(), model.Batch{Samples: samples}); err != nil {
		t.Fatalf("proactive MaxBytes split should succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&reqCount); got < 2 {
		t.Fatalf("proactive MaxBytes split should produce >1 requests, got %d", got)
	}
	assertEachDeliveredOnce(t, acc, sampleNames(samples))
}

func TestEmitNeverLeaksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); w.Write([]byte("bad")) }))
	defer srv.Close()
	err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch())
	if err != nil && strings.Contains(err.Error(), "secret-token") {
		t.Fatal("token leaked into error string")
	}
}
