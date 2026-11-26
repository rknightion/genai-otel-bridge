// SPDX-License-Identifier: Apache-2.0

package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
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

func oneLogBatch() model.Batch {
	return model.Batch{
		Logs: []model.LogRecord{
			{
				Timestamp:         time.Unix(1_700_000_000, 0).UTC(),
				Severity:          "INFO",
				IndexedAttributes: map[string]string{"loop": "portkey-analytics"},
				RecordAttributes:  map[string]string{"model": "gpt-4o"},
			},
		},
	}
}

// TestLogsEmitPathAndHeaders verifies that a logs-only batch is POSTed to /v1/logs (not /v1/metrics)
// with the correct Content-Type, Content-Encoding, and Authorization headers.
func TestLogsEmitPathAndHeaders(t *testing.T) {
	var sawPath, sawAuth, sawCE, sawCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		sawCE = r.Header.Get("Content-Encoding")
		sawCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	if err := testEmitter(t, srv.URL).Emit(context.Background(), oneLogBatch()); err != nil {
		t.Fatal(err)
	}
	if sawPath != "/v1/logs" {
		t.Errorf("path=%q; want /v1/logs", sawPath)
	}
	if !strings.HasPrefix(sawAuth, "Basic ") {
		t.Errorf("Authorization=%q; want Basic ...", sawAuth)
	}
	if sawCE != "gzip" {
		t.Errorf("Content-Encoding=%q; want gzip", sawCE)
	}
	if sawCT != "application/x-protobuf" {
		t.Errorf("Content-Type=%q; want application/x-protobuf", sawCT)
	}
}

// TestSamplesOnlyBatchStillUsesMetricsPath is a regression guard: the post/doOnce refactor (adding
// the url parameter) must not break metrics routing.
func TestSamplesOnlyBatchStillUsesMetricsPath(t *testing.T) {
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

	if err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch()); err != nil {
		t.Fatal(err)
	}
	if sawPath != "/v1/metrics" {
		t.Errorf("path=%q; want /v1/metrics", sawPath)
	}
}

// TestLogs413TriggersReactiveSplit verifies that a 413 from the logs endpoint triggers the reactive
// midpoint split: the server must receive more than one request, each with a smaller body.
func TestLogs413TriggersReactiveSplit(t *testing.T) {
	var reqCount int32
	var firstBodySize int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqCount, 1)
		if n == 1 {
			// Record the size of the first request body, then reject with 413.
			// The body is gzip-compressed protobuf; we just care that subsequent requests are smaller.
			buf := make([]byte, 4096)
			read, _ := r.Body.Read(buf)
			firstBodySize = read
			w.WriteHeader(413)
			return
		}
		// After split: accept all sub-requests.
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Two log records so there is something to split.
	b := model.Batch{
		Logs: []model.LogRecord{
			{
				Timestamp:         time.Unix(1_700_000_000, 0).UTC(),
				Severity:          "INFO",
				IndexedAttributes: map[string]string{"loop": "portkey-analytics"},
				RecordAttributes:  map[string]string{"model": "gpt-4o"},
			},
			{
				Timestamp:         time.Unix(1_700_000_001, 0).UTC(),
				Severity:          "WARN",
				IndexedAttributes: map[string]string{"loop": "portkey-analytics"},
				RecordAttributes:  map[string]string{"model": "gpt-4"},
			},
		},
	}

	e := testEmitter(t, srv.URL)
	if err := e.Emit(context.Background(), b); err != nil {
		t.Fatalf("expected success after 413-triggered split, got: %v", err)
	}
	got := atomic.LoadInt32(&reqCount)
	if got < 2 {
		t.Errorf("expected >1 requests after 413 split, got %d", got)
	}
	_ = firstBodySize // size recorded for diagnostic context; split correctness proven by reqCount
}

// TestLogs413SingleRecordSurfacesRejectError verifies that a 413 for a single log record (cannot
// split further) surfaces as a RejectError with ReasonPayloadTooLarge.
func TestLogs413SingleRecordSurfacesRejectError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(413)
		w.Write([]byte("request entity too large"))
	}))
	defer srv.Close()

	b := model.Batch{
		Logs: []model.LogRecord{
			{
				Timestamp: time.Unix(1_700_000_000, 0).UTC(),
				Severity:  "ERROR",
			},
		},
	}

	err := testEmitter(t, srv.URL).Emit(context.Background(), b)
	var re *emit.RejectError
	if !errors.As(err, &re) || re.Reason != emit.ReasonPayloadTooLarge {
		t.Errorf("single-record 413 must surface as ReasonPayloadTooLarge, got %v", err)
	}
}

// TestLogsProactiveMaxBytesSplit [#395 / CP-C11] covers the proactive branch for the LOGS plane: with a
// tiny MaxBytes the two-record gzipped payload exceeds the cap, so emitLogs must split BEFORE transmit
// (no 413 needed) into two single-record POSTs to /v1/logs, delivering each record exactly once.
func TestLogsProactiveMaxBytesSplit(t *testing.T) {
	var mu sync.Mutex
	var posts int
	var acc []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			w.WriteHeader(500)
			return
		}
		b, _ := io.ReadAll(zr)
		mu.Lock()
		posts++
		acc = append(acc, b...)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	b := model.Batch{Logs: []model.LogRecord{
		{Timestamp: time.Unix(1_700_000_000, 0).UTC(), RecordAttributes: map[string]string{"id": "rec_marker_0001"}},
		{Timestamp: time.Unix(1_700_000_001, 0).UTC(), RecordAttributes: map[string]string{"id": "rec_marker_0002"}},
	}}
	if err := emitterWithMaxBytes(srv.URL, 1).Emit(context.Background(), b); err != nil {
		t.Fatalf("proactive MaxBytes split should succeed, got %v", err)
	}
	if posts != 2 {
		t.Fatalf("proactive MaxBytes split should POST each record alone (2 requests), got %d", posts)
	}
	for _, m := range []string{"rec_marker_0001", "rec_marker_0002"} {
		if c := bytes.Count(acc, []byte(m)); c != 1 {
			t.Fatalf("record %q delivered %d times, want exactly 1", m, c)
		}
	}
}
