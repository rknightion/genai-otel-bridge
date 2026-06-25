// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// bodyServer serves a fixed JSONL body at any path except "/500" (which returns 500). Used to unit-test
// the streaming chunker in isolation from the lifecycle.
func bodyServer(t *testing.T, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/500" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dlLoop builds a loop whose dlClient allows srv's host (loopback), for downloadChunk unit tests.
func dlLoop(t *testing.T, srv *httptest.Server) *logsExportLoop {
	return mkLogsLoop(t, logsCfg(srv, map[string]string{"window": "1h", "settle": "10m"}), time.Now().UTC())
}

func TestDownloadChunkSkipAndBound(t *testing.T) {
	srv := bodyServer(t, nLines(10, "m"))
	l := dlLoop(t, srv)
	recs, lines, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 3, 4, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || lines != 4 || eof {
		t.Fatalf("want 4 recs / 4 lines / eof=false, got %d/%d/%v", len(recs), lines, eof)
	}
	// After skipping 3 (r0,r1,r2), the first emitted record is r3.
	if recs[0].RecordAttributes["id"] != "r3" {
		t.Fatalf("first record id=%q want r3 (offset skip)", recs[0].RecordAttributes["id"])
	}
}

func TestDownloadChunkEOFShortChunk(t *testing.T) {
	srv := bodyServer(t, nLines(10, "m"))
	l := dlLoop(t, srv)
	recs, lines, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 8, 4, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || lines != 2 || !eof {
		t.Fatalf("want 2 recs / 2 lines / eof=true, got %d/%d/%v", len(recs), lines, eof)
	}
}

// An exact-multiple page: a full chunk reports eof=false; the next (past-the-end) chunk reads 0 → eof.
func TestDownloadChunkExactMultiple(t *testing.T) {
	srv := bodyServer(t, nLines(5, "m"))
	l := dlLoop(t, srv)
	_, lines, eof, _ := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 5, time.Unix(0, 0).UTC())
	if lines != 5 || eof {
		t.Fatalf("full chunk: want 5 lines / eof=false, got %d/%v", lines, eof)
	}
	recs, lines, eof, _ := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 5, 5, time.Unix(0, 0).UTC())
	if len(recs) != 0 || lines != 0 || !eof {
		t.Fatalf("past-end chunk: want 0 recs / eof=true, got %d/%d/%v", len(recs), lines, eof)
	}
}

// A malformed line is SKIPPED (not parsed) but still advances the line offset, so the chunk's record
// count is < lines consumed and the next chunk resumes past it.
func TestDownloadChunkSkipsMalformedLine(t *testing.T) {
	body := exportLine(0, "m") + "{not json\n" + exportLine(2, "m")
	srv := bodyServer(t, body)
	l := dlLoop(t, srv)
	recs, lines, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 10, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lines != 3 {
		t.Fatalf("lines consumed=%d want 3 (incl. the malformed one)", lines)
	}
	if len(recs) != 2 {
		t.Fatalf("emitted records=%d want 2 (malformed dropped)", len(recs))
	}
	if !eof {
		t.Fatal("want eof=true (3 < chunk max 10)")
	}
}

// TestDownloadChunkCountsUnparsedTraceID: when a trace-id metadata field is configured, a record whose
// value is present-but-not-a-UUID fires the alertable trace_id_unparsed self-metric (so a fleet-wide
// broken mapping is visible, not silent), while the raw value still ships as a record attr. A valid
// UUID and an absent value are NOT counted.
func TestDownloadChunkCountsUnparsedTraceID(t *testing.T) {
	body := `{"id":"r0","metadata":{"correlation_id":"00112233-4455-6677-8899-aabbccddeeff"}}` + "\n" +
		`{"id":"r1","metadata":{"correlation_id":"not-a-uuid"}}` + "\n" +
		`{"id":"r2"}` + "\n"
	srv := bodyServer(t, body)
	l := dlLoop(t, srv)
	l.policy = defaultLogFieldPolicy().withMetadataFields(nil, "correlation_id")
	var skipped []string
	l.onGraphSkipped = func(loop, graph string) { skipped = append(skipped, loop+"/"+graph) }
	recs, _, _, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 10, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("recs=%d want 3", len(recs))
	}
	if len(recs[0].TraceID) != 16 {
		t.Fatalf("r0 valid UUID should yield a 16-byte TraceID, got %x", recs[0].TraceID)
	}
	if recs[1].TraceID != nil {
		t.Fatalf("r1 non-UUID TraceID must be unset, got %x", recs[1].TraceID)
	}
	if recs[1].RecordAttributes["correlation_id"] != "not-a-uuid" {
		t.Fatal("r1 raw value should still ship as a record attr")
	}
	if len(skipped) != 1 || skipped[0] != "logs_export/trace_id_unparsed" {
		t.Fatalf("want exactly one logs_export/trace_id_unparsed count, got %v", skipped)
	}
}

// TestDownloadChunkCountsUnparsedTopLevelTraceID mirrors the metadata-path counter test for the
// Portkey-native trace_id path (settings.trace_id_field): a present-but-non-UUID top-level value fires the
// alertable trace_id_unparsed self-metric while the raw value still ships as a record attr; a valid UUID
// and an absent value are NOT counted.
func TestDownloadChunkCountsUnparsedTopLevelTraceID(t *testing.T) {
	body := `{"id":"r0","trace_id":"00112233-4455-6677-8899-aabbccddeeff"}` + "\n" +
		`{"id":"r1","trace_id":"not-a-uuid"}` + "\n" +
		`{"id":"r2"}` + "\n"
	srv := bodyServer(t, body)
	l := dlLoop(t, srv)
	l.policy = defaultLogFieldPolicy().withTraceIDField("trace_id")
	var skipped []string
	l.onGraphSkipped = func(loop, graph string) { skipped = append(skipped, loop+"/"+graph) }
	recs, _, _, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 10, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("recs=%d want 3", len(recs))
	}
	if len(recs[0].TraceID) != 16 {
		t.Fatalf("r0 valid UUID should yield a 16-byte TraceID, got %x", recs[0].TraceID)
	}
	if recs[1].TraceID != nil {
		t.Fatalf("r1 non-UUID TraceID must be unset, got %x", recs[1].TraceID)
	}
	if recs[1].RecordAttributes["trace_id"] != "not-a-uuid" {
		t.Fatal("r1 raw value should still ship as a record attr")
	}
	if len(skipped) != 1 || skipped[0] != "logs_export/trace_id_unparsed" {
		t.Fatalf("want exactly one logs_export/trace_id_unparsed count, got %v", skipped)
	}
}

func TestDownloadChunkNon200Errors(t *testing.T) {
	srv := bodyServer(t, "")
	l := dlLoop(t, srv)
	if _, _, _, err := l.downloadChunk(context.Background(), srv.URL+"/500", 0, 4, time.Unix(0, 0).UTC()); err == nil {
		t.Fatal("a non-200 download must error (no silent empty page)")
	}
}

func TestValidateSignedURLHost(t *testing.T) {
	l := &logsExportLoop{signedURLAllowHosts: []string{"ai-gateway-dataservice-us-prod.s3.us-west-2.amazonaws.com"}}
	if err := l.validateSignedURLHost("https://ai-gateway-dataservice-us-prod.s3.us-west-2.amazonaws.com/x.jsonl?sig=1"); err != nil {
		t.Fatalf("allow-listed host rejected: %v", err)
	}
	for _, bad := range []string{
		"https://evil.example.com/x.jsonl",
		"https://169.254.169.254/latest/meta-data/",
		"http://ai-gateway-dataservice-us-prod.s3.us-west-2.amazonaws.com.evil.com/x",
	} {
		if err := l.validateSignedURLHost(bad); err == nil {
			t.Fatalf("host %q must be rejected (not in allow-list)", bad)
		}
	}
}
