// SPDX-License-Identifier: Apache-2.0

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
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// splitServer records every gunzipped request body. A request whose body contains rejectMarker gets
// rejectStatus/rejectBody; a request containing failMarker gets a 500; everything else gets 200.
type splitServer struct {
	mu           sync.Mutex
	seen         []byte
	reqs         int
	rejectMarker string
	rejectBody   string
	failMarker   string
}

func (s *splitServer) handler(w http.ResponseWriter, r *http.Request) {
	zr, _ := gzip.NewReader(r.Body)
	b, _ := io.ReadAll(zr)
	s.mu.Lock()
	s.seen = append(s.seen, b...)
	s.reqs++
	s.mu.Unlock()
	switch {
	case s.rejectMarker != "" && bytes.Contains(b, []byte(s.rejectMarker)):
		w.WriteHeader(400)
		_, _ = w.Write([]byte(s.rejectBody))
	case s.failMarker != "" && bytes.Contains(b, []byte(s.failMarker)):
		w.WriteHeader(500)
	default:
		w.WriteHeader(200)
	}
}

func (s *splitServer) sent(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Contains(s.seen, []byte(name))
}

func uniqueLogs(n int) []model.LogRecord {
	out := make([]model.LogRecord, n)
	for i := range out {
		out[i] = model.LogRecord{
			Timestamp:         time.Unix(1_700_000_000+int64(i), 0).UTC(),
			Severity:          "INFO",
			IndexedAttributes: map[string]string{"loop": "split-test"},
			RecordAttributes:  map[string]string{"marker": fmt.Sprintf("log_rec_%04d", i)},
		}
	}
	return out
}

func logMarkers(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("log_rec_%04d", i)
	}
	return out
}

func requireAdvancePast(t *testing.T, err error) {
	t.Helper()
	var re *emit.RejectError
	if !errors.As(err, &re) || !re.AdvancesPast() {
		t.Fatalf("want an advance-past reject, got %v", err)
	}
}

func TestSplitContinuesAfterAdvancePastReject(t *testing.T) {
	t.Run("samples", func(t *testing.T) {
		s := &splitServer{rejectMarker: "portkey_api_m_0000", rejectBody: "err-mimir-sample-duplicate-timestamp"}
		srv := httptest.NewServer(http.HandlerFunc(s.handler))
		defer srv.Close()
		samples := uniqueSamples(4)
		err := emitterWithMaxBytes(srv.URL, 1).Emit(context.Background(), model.Batch{Samples: samples})
		requireAdvancePast(t, err)
		for _, n := range sampleNames(samples)[1:] {
			if !s.sent(n) {
				t.Errorf("sample %s was never sent to the gateway, but the bucket is advanced past", n)
			}
		}
	})
	t.Run("logs", func(t *testing.T) {
		s := &splitServer{rejectMarker: "log_rec_0000", rejectBody: "entry too far behind"}
		srv := httptest.NewServer(http.HandlerFunc(s.handler))
		defer srv.Close()
		err := emitterWithMaxBytes(srv.URL, 1).Emit(context.Background(), model.Batch{Logs: uniqueLogs(4)})
		requireAdvancePast(t, err)
		for _, n := range logMarkers(4)[1:] {
			if !s.sent(n) {
				t.Errorf("log %s was never sent to the gateway, but the chunk is advanced past", n)
			}
		}
	})
}

// A later chunk failing retryably must win over the earlier advance-past reject, so the runner does
// not advance past data that still needs a retry.
func TestSplitLaterRetryableBeatsEarlierAdvancePast(t *testing.T) {
	t.Run("samples", func(t *testing.T) {
		s := &splitServer{rejectMarker: "portkey_api_m_0000", rejectBody: "err-mimir-sample-duplicate-timestamp", failMarker: "portkey_api_m_0003"}
		srv := httptest.NewServer(http.HandlerFunc(s.handler))
		defer srv.Close()
		err := emitterWithMaxBytes(srv.URL, 1).Emit(context.Background(), model.Batch{Samples: uniqueSamples(4)})
		var rt *emit.RetryableError
		if !errors.As(err, &rt) {
			t.Fatalf("want RetryableError, got %v", err)
		}
	})
	t.Run("logs", func(t *testing.T) {
		s := &splitServer{rejectMarker: "log_rec_0000", rejectBody: "entry too far behind", failMarker: "log_rec_0003"}
		srv := httptest.NewServer(http.HandlerFunc(s.handler))
		defer srv.Close()
		err := emitterWithMaxBytes(srv.URL, 1).Emit(context.Background(), model.Batch{Logs: uniqueLogs(4)})
		var rt *emit.RetryableError
		if !errors.As(err, &rt) {
			t.Fatalf("want RetryableError, got %v", err)
		}
	})
}

// A retryable first chunk stops the emit at once: no later chunk is sent.
func TestSplitRetryableFirstChunkSendsNoMore(t *testing.T) {
	s := &splitServer{failMarker: "portkey_api_m_0000"}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	samples := uniqueSamples(4)
	err := emitterWithMaxBytes(srv.URL, 1).Emit(context.Background(), model.Batch{Samples: samples})
	var rt *emit.RetryableError
	if !errors.As(err, &rt) {
		t.Fatalf("want RetryableError, got %v", err)
	}
	for _, n := range sampleNames(samples)[1:] {
		if s.sent(n) {
			t.Errorf("sample %s was sent after the first chunk failed retryably", n)
		}
	}
}
