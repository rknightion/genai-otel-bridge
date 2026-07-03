// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// defaultDownloadMaxBytes caps a single signed-URL object read (defence against a server returning an
// unbounded body). A page is ≤page_size operational records (small, content-free), so this is generous
// headroom; a real page is well under it. Stored as a loop field (overridable in tests) so the
// truncation-error path is exercisable without a multi-hundred-MiB fixture.
const defaultDownloadMaxBytes = 512 << 20 // 512 MiB

// maxLogLineBytes caps one JSONL line. Operational export records are small; a line beyond this is
// treated as malformed (corrupt or content-bearing) and is SKIPPED loudly — its bytes are drained and
// discarded (never parsed/stringified), the line offset still advances so the loop resumes PAST it (it
// never wedges re-reading the same over-long line), and it is counted via OnGraphSkipped(graph=
// "line_oversize"). Overridable per-loop via logsExportLoop.maxLineBytes so the skip path is testable
// without a multi-MiB fixture. See readLine / downloadChunk.
const maxLogLineBytes = 1 << 20 // 1 MiB

// lineReadBufBytes is the bufio.Reader working-buffer size for the JSONL line reader — the chunk of a
// long line held in memory per underlying read. A normal line is accumulated up to the line cap; an
// over-long line is drained through this buffer without retaining it, so peak memory is bounded to
// ~lineCap regardless of how large the bad line is (DESIGN §5 memory bound: the file is never buffered
// whole).
const lineReadBufBytes = 64 << 10 // 64 KiB

// countingReader tracks bytes read so downloadChunk can detect a download truncated at the byte cap
// (a silent tail-drop) and turn it into a loud error.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// validateSignedURLHost enforces the signed-URL host allow-list (spec §7 / DESIGN §4.7:270): the
// download URL is a SERVER-CONTROLLED input (Portkey returns it), so its host must be explicitly
// allow-listed before any fetch — independent of the httpx egress guard on dlClient (which is the
// defence-in-depth backstop + the IP-level metadata/RFC-1918 block). A self-hosted / in-VPC Portkey
// changes this host, so it is config, never a constant.
func (l *logsExportLoop) validateSignedURLHost(signedURL string) error {
	u, err := url.Parse(signedURL)
	if err != nil {
		return fmt.Errorf("parse signed url: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("signed url has no host")
	}
	for _, h := range l.signedURLAllowHosts {
		if strings.ToLower(h) == host {
			return nil
		}
	}
	return fmt.Errorf("signed-url host %q not in signed_url_allow_hosts", u.Hostname())
}

// downloadChunk GETs the signed-URL JSONL object, skips `skip` LINES (already-consumed lines of this
// page, for re-download resume), and returns up to `max` stripped LogRecords from the next lines, the
// number of LINES consumed THIS chunk (advances page_offset_done), and eof (the page is exhausted — it
// returned fewer than a full chunk). It STREAMS line-by-line: at most one raw line plus `max` stripped
// records are held — the ≤page_size file is NEVER buffered whole (DESIGN §5 memory bound). A fresh
// signed URL is minted per call (PoC §4: GET-only, 6h expiry), so this is also the expiry-recovery +
// idempotent-resume path; the S3 object is stable, so re-download never re-runs the export.
//
// page_offset_done counts LINES (file positions), not emitted records: a malformed line is skipped but
// still advances the offset, so the next chunk resumes past it and we never re-attempt the bad line.
func (l *logsExportLoop) downloadChunk(ctx context.Context, signedURL string, skip, max int, fallbackTS time.Time) (recs []model.LogRecord, linesRead int, eof bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		// A NewRequest URL-parse error stringifies the raw signed URL (incl. its credential-bearing query),
		// so redact the query before it can reach a log line (issue #34).
		return nil, 0, false, httpx.RedactURLError(err)
	}
	resp, err := l.dlClient.Do(req)
	if err != nil {
		// SECURITY (issue #34): a transport error (timeout / reset / TLS / EOF) is a *url.Error whose string
		// embeds the FULL signed URL, and the signed query IS a live bearer credential (X-Amz-Signature /
		// X-Goog-Signature / SAS token) granting read of the raw, un-stripped export object. This error
		// propagates to the scheduler's slog.Warn → stdout → Loki, so the query MUST be stripped before it
		// can ever be rendered. The non-2xx path below uses ErrSnippet (bounded body, no URL) — already safe.
		return nil, 0, false, fmt.Errorf("portkey logs_export: signed-url download: %w", httpx.RedactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, false, fmt.Errorf("portkey logs_export: signed-url download status %d%s", resp.StatusCode, errBodySuffix(httpx.ErrSnippet(resp)))
	}

	// Read at most downloadMaxBytes+1 through a counting reader: if the count reaches downloadMaxBytes+1
	// the object exceeded the cap and the scan was TRUNCATED — we must error loudly rather than treat the
	// truncation as a natural end-of-page (which would silently drop the tail, violating "every gap is
	// alertable"). The +1 is what lets us distinguish "hit the cap" from "exactly filled it".
	maxBytes := l.downloadMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultDownloadMaxBytes
	}
	lineCap := l.maxLineBytes
	if lineCap <= 0 {
		lineCap = maxLogLineBytes
	}
	cr := &countingReader{r: io.LimitReader(resp.Body, maxBytes+1)}
	// A bounded line reader (NOT bufio.Scanner): the Scanner aborts the WHOLE scan with ErrTooLong on a
	// single over-long line and cannot advance past it — so one >lineCap line wedged the loop forever
	// (issue #35). readLine instead DRAINS + skips an over-long line while advancing the offset, so the
	// page always completes and the bad line is counted, not fatal.
	br := bufio.NewReaderSize(cr, lineReadBufBytes)

	// Skip the already-consumed lines of this page (re-download resume). An over-long line in the skip
	// region is drained like any other (its tooLong flag is ignored — it was counted when first read).
	// One line at a time ⇒ bounded memory even across the skip.
	for range skip {
		_, _, rerr := readLine(br, lineCap)
		if rerr == io.EOF {
			// Fewer lines than the saved offset — the object ended early (shouldn't happen for a stable
			// S3 object). Treat the page as exhausted rather than crash; the window's frontier won't
			// advance unless this was the last page, so a genuine shortfall stays loud via window_lag.
			return nil, skip, true, nil
		}
		if rerr != nil {
			return nil, skip, false, fmt.Errorf("portkey logs_export: stream signed-url body: %w", rerr)
		}
	}

	recs = make([]model.LogRecord, 0, max)
	for linesRead < max {
		line, tooLong, rerr := readLine(br, lineCap)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return recs, linesRead, false, fmt.Errorf("portkey logs_export: stream signed-url body: %w", rerr)
		}
		linesRead++ // count the line whether emitted or skipped — page_offset_done must advance PAST it
		if tooLong {
			// Over-long line (issue #35): SKIP it loudly and keep the page progressing. The offset advanced
			// above, so the loop never re-attempts these bytes (no wedge); the bytes were drained + discarded,
			// never parsed/stringified (they may be content-bearing). Counted so it is alertable, not silent.
			slog.Warn("portkey logs_export: skipping over-long export line (exceeds max_log_line_bytes)",
				"source", l.sourceInstance, "line", skip+linesRead, "cap_bytes", lineCap)
			l.lineOversize()
			continue
		}
		var raw map[string]json.RawMessage
		if uerr := json.Unmarshal(line, &raw); uerr != nil {
			// Malformed line: re-download yields the same bytes (stable object), so retrying is futile.
			// Skip it, loudly (alertable via the error log), and keep the page progressing — the line
			// offset still advances so we never re-attempt this line.
			slog.Warn("portkey logs_export: skipping unparseable export line",
				"source", l.sourceInstance, "line", skip+linesRead)
			continue
		}
		rec := l.policy.strip(raw, fallbackTS)
		// Operationally honest: if a trace-id field is configured and its value was present (shipped as an
		// attr) but did not parse to a UUID, the OTLP trace_id mapping was lost — count it so a systematically
		// broken upstream format is alertable (the log record itself is unaffected).
		if f := l.policy.traceIDAttrKey(); f != "" && len(rec.TraceID) == 0 && rec.RecordAttributes[f] != "" {
			l.traceIDUnparsed()
		}
		recs = append(recs, rec)
	}
	if cr.n > maxBytes {
		// The reader delivered more than the cap → the scan was (or was about to be) truncated. Error
		// loudly (retryable) instead of reporting a false eof — a truncated page must never advance the
		// window over the dropped tail. (Edge: a non-final chunk that stops at chunk_max_records while the
		// object is within ~one read of the cap could trip this slightly early; harmless — it's retryable,
		// and a page legitimately near 512 MiB is itself a mis-sized window. Raise the cap / shrink window.)
		return nil, 0, false, fmt.Errorf("portkey logs_export: signed-url object exceeds %d bytes (truncated) — raise download cap / shrink window", maxBytes)
	}
	// Fewer than a full chunk ⇒ end of page. (When a page is an exact multiple of `max`, the final
	// non-empty chunk reports eof=false and the NEXT chunk reads zero lines ⇒ eof=true — one extra,
	// cheap, idempotent re-download. Correctness over micro-optimisation.)
	eof = linesRead < max
	return recs, linesRead, eof, nil
}

// readLine reads ONE newline-terminated logical line from br, bounded so a pathologically long line can
// never blow memory or wedge the loop. It returns the line WITHOUT the trailing '\n'. If the line's
// content exceeds `limit` bytes it DRAINS the remainder up to (and including) the next newline WITHOUT
// retaining it, and returns tooLong=true — so the reader always advances PAST the bad line rather than
// aborting the whole scan the way bufio.Scanner does on ErrTooLong (issue #35). At a clean EOF with no
// pending bytes it returns io.EOF; at EOF with a final unterminated line it returns that line. Any other
// read error is returned verbatim. Peak memory is ~limit + one bufio buffer, regardless of line size.
func readLine(br *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	var buf []byte
	var total int // content bytes seen for this line so far (may exceed len(buf) once truncated)
	for {
		frag, e := br.ReadSlice('\n')
		chunk := frag
		if e == nil && len(chunk) > 0 && chunk[len(chunk)-1] == '\n' {
			chunk = chunk[:len(chunk)-1] // strip the delimiter from the returned line
		}
		total += len(chunk)
		if tooLong || total > limit {
			tooLong = true
			buf = nil // release: an over-long line is never emitted, so don't hold its bytes
		} else {
			buf = append(buf, chunk...) // ReadSlice's slice aliases the bufio buffer — copy it out now
		}
		switch e {
		case bufio.ErrBufferFull:
			continue // more of this line remains in the stream
		case nil:
			return buf, tooLong, nil
		case io.EOF:
			if total == 0 {
				return nil, false, io.EOF // clean end, nothing pending
			}
			return buf, tooLong, nil // final unterminated line
		default:
			return nil, false, e
		}
	}
}
