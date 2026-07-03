// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// rangeServer serves a fixed JSONL body and (unless ignoreRange) honours HTTP `Range: bytes=N-` with a
// 206 + Content-Range. It records the Range header of every request and counts total bytes served, so a
// test can prove a multi-chunk page resumes from the byte offset (Range sent, consumed PREFIX never
// re-served) instead of re-downloading the whole object per chunk (#61). ignoreRange models a server/CDN
// that drops the Range header and returns 200 + the full body — the correctness fallback path.
type rangeServer struct {
	srv         *httptest.Server
	mu          sync.Mutex
	body        []byte
	ranges      []string // the Range header of each request ("" = none)
	served      int64    // total response bytes written
	ignoreRange bool
}

func newRangeServer(t *testing.T, body string, ignoreRange bool) *rangeServer {
	rs := &rangeServer{body: []byte(body), ignoreRange: ignoreRange}
	rs.srv = httptest.NewServer(http.HandlerFunc(rs.handle))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (s *rangeServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hdr := r.Header.Get("Range")
	s.ranges = append(s.ranges, hdr)
	if hdr != "" && !s.ignoreRange {
		var start int64
		_, _ = fmt.Sscanf(hdr, "bytes=%d-", &start)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(s.body)-1, len(s.body)))
		w.WriteHeader(http.StatusPartialContent)
		n, _ := w.Write(s.body[start:]) // serve only the tail — the prefix is not re-transferred
		s.served += int64(n)
		return
	}
	n, _ := w.Write(s.body) // 200 + full body (first chunk, or a server that ignored Range)
	s.served += int64(n)
}

// TestDownloadChunkRangeResume (#61): a multi-chunk page resumes via an HTTP Range from the persisted byte
// offset — the SECOND chunk sends `Range: bytes=<offset>-` and the server serves ONLY the un-consumed tail,
// so the consumed prefix is never re-downloaded/re-scanned (the 10× S3 egress on a full page). Records are
// correct and non-duplicated across chunks. This is also the failover path: a resume with a saved byte
// offset (as a new leader would replay from the cursor) Ranges from exactly that offset.
func TestDownloadChunkRangeResume(t *testing.T) {
	srv := newRangeServer(t, nLines(10, "m"), false)
	l := mkLogsLoop(t, logsCfg(srv.srv, map[string]string{"window": "1h", "settle": "10m"}), time.Now().UTC())
	full := int64(len(srv.body))

	// Chunk 1: byteOffset 0 ⇒ no Range, full body (200), read 6 lines r0..r5.
	recs1, lines1, off1, eof1, err := l.downloadChunk(context.Background(), srv.srv.URL+"/f.jsonl", 0, 0, 6, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lines1 != 6 || eof1 || len(recs1) != 6 || recs1[0].RecordAttributes["id"] != "r0" || recs1[5].RecordAttributes["id"] != "r5" {
		t.Fatalf("chunk1: want 6 lines r0..r5 eof=false, got %d lines eof=%v ids %q..%q", lines1, eof1, recs1[0].RecordAttributes["id"], recs1[len(recs1)-1].RecordAttributes["id"])
	}
	if off1 <= 0 || off1 >= full {
		t.Fatalf("chunk1 byte offset %d must be a mid-object line boundary (0<off<%d)", off1, full)
	}
	servedAfter1 := srv.served

	// Chunk 2: resume with the saved byte offset (line offset 6 kept as the fallback) ⇒ Range bytes=off1-,
	// server serves ONLY body[off1:], read the remaining r6..r9.
	recs2, lines2, off2, eof2, err := l.downloadChunk(context.Background(), srv.srv.URL+"/f.jsonl", lines1, off1, 6, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lines2 != 4 || !eof2 || len(recs2) != 4 || recs2[0].RecordAttributes["id"] != "r6" || recs2[3].RecordAttributes["id"] != "r9" {
		t.Fatalf("chunk2: want 4 lines r6..r9 eof=true, got %d lines eof=%v", lines2, eof2)
	}
	if off2 != full {
		t.Fatalf("chunk2 must consume to end-of-object; byte offset %d want %d", off2, full)
	}
	// The resume request must carry the Range header at exactly off1.
	if len(srv.ranges) != 2 || srv.ranges[0] != "" || srv.ranges[1] != fmt.Sprintf("bytes=%d-", off1) {
		t.Fatalf("resume must send Range bytes=%d-, got request ranges %v", off1, srv.ranges)
	}
	// The prefix was NOT re-served: chunk2 transferred only the tail (full-off1), so total served is well
	// under two full objects (the old behaviour re-downloaded the whole object every chunk).
	tail := full - off1
	if got := srv.served - servedAfter1; got != tail {
		t.Fatalf("chunk2 served %d bytes, want only the tail %d (prefix must not be re-transferred)", got, tail)
	}
	if srv.served >= 2*full {
		t.Fatalf("total served %d ≥ 2×object (%d) — the consumed prefix was re-downloaded, Range resume ineffective", srv.served, 2*full)
	}
}

// TestDownloadChunkRangeIgnoredFallback (#61): a server that IGNORES the Range header (returns 200 + the
// full body) must still yield correct records via the existing line-skip path — correctness never depends
// on Range being honoured.
func TestDownloadChunkRangeIgnoredFallback(t *testing.T) {
	srv := newRangeServer(t, nLines(10, "m"), true) // ignoreRange ⇒ always 200 + full body
	l := mkLogsLoop(t, logsCfg(srv.srv, map[string]string{"window": "1h", "settle": "10m"}), time.Now().UTC())

	// Chunk 1: 6 lines from the start.
	recs1, lines1, off1, _, err := l.downloadChunk(context.Background(), srv.srv.URL+"/f.jsonl", 0, 0, 6, time.Unix(0, 0).UTC())
	if err != nil || lines1 != 6 || recs1[0].RecordAttributes["id"] != "r0" {
		t.Fatalf("chunk1: err=%v lines=%d", err, lines1)
	}
	// Chunk 2: pass the byte offset, but the server ignores Range (200) ⇒ line-skip 6 lines still resumes
	// correctly at r6..r9 (the correctness fallback).
	recs2, lines2, _, eof2, err := l.downloadChunk(context.Background(), srv.srv.URL+"/f.jsonl", lines1, off1, 6, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lines2 != 4 || !eof2 || len(recs2) != 4 || recs2[0].RecordAttributes["id"] != "r6" || recs2[3].RecordAttributes["id"] != "r9" {
		t.Fatalf("Range-ignored fallback: want 4 lines r6..r9 via line-skip, got %d lines (ids %v)", lines2, idsOf(recs2))
	}
}

// idsOf extracts the record-attr id from each record for a readable failure message.
func idsOf(recs []model.LogRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.RecordAttributes["id"]
	}
	return out
}

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
	recs, lines, _, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 3, 0, 4, time.Unix(0, 0).UTC())
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
	recs, lines, _, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 8, 0, 4, time.Unix(0, 0).UTC())
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
	_, lines, _, eof, _ := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 0, 5, time.Unix(0, 0).UTC())
	if lines != 5 || eof {
		t.Fatalf("full chunk: want 5 lines / eof=false, got %d/%v", lines, eof)
	}
	recs, lines, _, eof, _ := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 5, 0, 5, time.Unix(0, 0).UTC())
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
	var skipped []string
	l.onGraphSkipped = func(loop, graph string) { skipped = append(skipped, loop+"/"+graph) }
	recs, lines, _, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 0, 10, time.Unix(0, 0).UTC())
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
	// #66: the skipped unparseable line must fire exactly one alertable self-metric count (a Warn log
	// alone is invisible to the metrics-based self-obs stack — a systematic format change would otherwise
	// drop 100% of records while the window completes silently).
	if len(skipped) != 1 || skipped[0] != "logs_export/line_unparseable" {
		t.Fatalf("unparseable line must fire exactly one logs_export/line_unparseable count, got %v", skipped)
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
	recs, _, _, _, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 0, 10, time.Unix(0, 0).UTC())
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
	recs, _, _, _, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 0, 10, time.Unix(0, 0).UTC())
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
	if _, _, _, _, err := l.downloadChunk(context.Background(), srv.URL+"/500", 0, 0, 4, time.Unix(0, 0).UTC()); err == nil {
		t.Fatal("a non-200 download must error (no silent empty page)")
	}
}

// TestDownloadChunkRedactsSignedURLInTransportError (issue #34, SECURITY): a transport error on the
// signed-URL fetch must NOT leak the credential-bearing query (X-Amz-Signature / SAS token) into the
// returned error — which would otherwise be logged to stdout → Loki. We point the download at an
// allow-listed but unreachable host (server closed) so dlClient.Do returns a *url.Error embedding the
// full signed URL, and assert no signature/credential survives.
func TestDownloadChunkRedactsSignedURLInTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	l := mkLogsLoop(t, logsCfg(srv, map[string]string{"window": "1h", "settle": "10m"}), time.Now().UTC())
	signed := srv.URL + "/obj.jsonl?X-Amz-Signature=SUPERSECRETSIG&X-Amz-Credential=AKIAEXAMPLE&X-Amz-Expires=21600"
	srv.Close() // connections now refused ⇒ the transport error is a *url.Error embedding the full signed URL
	_, _, _, _, err := l.downloadChunk(context.Background(), signed, 0, 0, 4, time.Unix(0, 0).UTC())
	if err == nil {
		t.Fatal("expected a transport error from the unreachable signed URL")
	}
	for _, leak := range []string{"SUPERSECRETSIG", "X-Amz-Signature", "X-Amz-Credential", "AKIAEXAMPLE", "X-Amz-Expires", "?"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("signed-URL credential leaked into download error: %q", err.Error())
		}
	}
}

// TestDownloadChunkSkipsOversizeLine (issue #35): a single JSONL line exceeding the line cap is SKIPPED
// (drained, never parsed) while the good lines around it are emitted, the line offset advances PAST the
// bad line (so the loop can never wedge re-reading it), and it fires exactly one line_oversize count.
func TestDownloadChunkSkipsOversizeLine(t *testing.T) {
	big := `{"id":"big","pad":"` + strings.Repeat("x", 4096) + `"}` + "\n"
	body := exportLine(0, "m") + big + exportLine(2, "m")
	srv := bodyServer(t, body)
	l := dlLoop(t, srv)
	l.maxLineBytes = 1024 // a normal exportLine (~few hundred B) fits; the padded line (>4 KiB) is over-long
	var skipped []string
	l.onGraphSkipped = func(loop, graph string) { skipped = append(skipped, loop+"/"+graph) }
	recs, lines, _, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 0, 10, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lines != 3 {
		t.Fatalf("lines consumed=%d want 3 (offset must advance PAST the over-long line)", lines)
	}
	if len(recs) != 2 {
		t.Fatalf("emitted records=%d want 2 (over-long line dropped, good lines around it kept)", len(recs))
	}
	if recs[0].RecordAttributes["id"] != "r0" || recs[1].RecordAttributes["id"] != "r2" {
		t.Fatalf("wrong records emitted: %q,%q want r0,r2", recs[0].RecordAttributes["id"], recs[1].RecordAttributes["id"])
	}
	if !eof {
		t.Fatal("want eof=true (3 < chunk max 10)")
	}
	if len(skipped) != 1 || skipped[0] != "logs_export/line_oversize" {
		t.Fatalf("over-long line must fire exactly one logs_export/line_oversize count, got %v", skipped)
	}
}

// TestDownloadChunkResumesPastOversizeLine (issue #35): a chunked page whose FIRST line is over-long is
// consumed across two chunks — the first chunk skips the bad line, the second resumes at the advanced
// offset and never re-reads it (no wedge on identical bytes).
func TestDownloadChunkResumesPastOversizeLine(t *testing.T) {
	big := `{"id":"big","pad":"` + strings.Repeat("x", 4096) + `"}` + "\n"
	body := big + exportLine(1, "m") + exportLine(2, "m")
	srv := bodyServer(t, body)
	l := dlLoop(t, srv)
	l.maxLineBytes = 1024
	// chunk 1: max=1 ⇒ read exactly one line — the over-long one — skip it, offset advances to 1.
	recs, lines, _, eof, err := l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 0, 0, 1, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lines != 1 || len(recs) != 0 || eof {
		t.Fatalf("chunk1: want 1 line / 0 recs / eof=false, got %d/%d/%v", lines, len(recs), eof)
	}
	// chunk 2: resume at offset 1 (skip the bad line without re-parsing it), emit the two good lines.
	recs, lines, _, eof, err = l.downloadChunk(context.Background(), srv.URL+"/f.jsonl", 1, 0, 10, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lines != 2 || len(recs) != 2 || !eof {
		t.Fatalf("chunk2: want 2 lines / 2 recs / eof=true, got %d/%d/%v", lines, len(recs), eof)
	}
	if recs[0].RecordAttributes["id"] != "r1" || recs[1].RecordAttributes["id"] != "r2" {
		t.Fatalf("resume emitted wrong records: %q,%q want r1,r2", recs[0].RecordAttributes["id"], recs[1].RecordAttributes["id"])
	}
}

func TestValidateSignedURLHost(t *testing.T) {
	l := &logsExportLoop{signedURLAllowHosts: []string{"signed-url-host.example.com"}}
	if err := l.validateSignedURLHost("https://signed-url-host.example.com/x.jsonl?sig=1"); err != nil {
		t.Fatalf("allow-listed host rejected: %v", err)
	}
	for _, bad := range []string{
		"https://evil.example.com/x.jsonl",
		"https://169.254.169.254/latest/meta-data/",
		"http://signed-url-host.example.com.evil.com/x",
	} {
		if err := l.validateSignedURLHost(bad); err == nil {
			t.Fatalf("host %q must be rejected (not in allow-list)", bad)
		}
	}
}

// TestValidateSignedURLScheme (#139): an http:// download URL to an ALLOW-LISTED host is refused
// (cleartext transit of the object + its X-Amz-Signature credential), UNLESS the documented
// http.allow_private carve-out applies. https is unaffected.
func TestValidateSignedURLScheme(t *testing.T) {
	strict := &logsExportLoop{signedURLAllowHosts: []string{"signed-url-host.example.com"}} // allowPrivate=false
	httpAllowed := "http://signed-url-host.example.com/x.jsonl?X-Amz-Signature=SIG"
	if err := strict.validateSignedURLHost(httpAllowed); err == nil {
		t.Fatal("http:// signed URL to an allow-listed host must be refused when allow_private is off (cleartext credential leak)")
	}
	// https to the same host still passes.
	if err := strict.validateSignedURLHost("https://signed-url-host.example.com/x.jsonl?X-Amz-Signature=SIG"); err != nil {
		t.Fatalf("https signed URL must be unaffected: %v", err)
	}
	// The documented in-VPC/self-hosted carve-out permits http when allow_private is set.
	priv := &logsExportLoop{signedURLAllowHosts: []string{"signed-url-host.example.com"}, allowPrivate: true}
	if err := priv.validateSignedURLHost(httpAllowed); err != nil {
		t.Fatalf("http signed URL must be permitted under allow_private carve-out: %v", err)
	}
}
