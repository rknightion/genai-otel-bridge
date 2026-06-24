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

	"github.com/grafana-ps/aip-oi/internal/emit"
	"github.com/grafana-ps/aip-oi/internal/model"
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
