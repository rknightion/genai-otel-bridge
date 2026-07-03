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
	"strconv"
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
	// [#139] REQUIRE https on the (server-controlled) download URL. The host allow-list + httpx egress
	// guard both validate host/IP but NOT the scheme, and the transport will happily issue plain http —
	// so a downgraded signed URL (a self-hosted/misconfigured Portkey, or an attacker influencing the
	// control-plane response when base_url is an http allow_private host) would transit the S3 object
	// AND the embedded X-Amz-Signature credential material in cleartext. Reject a non-https scheme
	// unless allow_private is set (the documented in-VPC/self-hosted carve-out, mirroring the base_url
	// rule) — belt-and-braces with the CP-M7 cleartext-emit posture.
	if !strings.EqualFold(u.Scheme, "https") && !l.allowPrivate {
		return fmt.Errorf("signed-url scheme %q is not https (refusing cleartext download of the signed object + its credential; set http.allow_private for an in-VPC/self-hosted http source)", u.Scheme)
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
func (l *logsExportLoop) downloadChunk(ctx context.Context, signedURL string, skip int, byteOffset int64, max int, fallbackTS time.Time) (recs []model.LogRecord, linesRead int, nextByteOffset int64, eof bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		// A NewRequest URL-parse error stringifies the raw signed URL (incl. its credential-bearing query),
		// so redact the query before it can reach a log line (issue #34).
		return nil, 0, 0, false, httpx.RedactURLError(err)
	}
	// [#61] Resume by BYTE offset: on a re-download (byteOffset>0) request only the un-consumed tail via an
	// HTTP Range header, so a multi-chunk page does not re-transfer AND re-scan (O(page²)) the already-
	// consumed prefix on every chunk (10× S3 egress on a full 50k page). The LINE offset (skip) is retained
	// as the correctness FALLBACK: a server/CDN that ignores Range returns 200 + the whole body, and we then
	// skip `skip` lines exactly as before — so correctness never depends on Range being honoured.
	if byteOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", byteOffset))
	}
	resp, err := l.dlClient.Do(req)
	if err != nil {
		// SECURITY (issue #34): a transport error (timeout / reset / TLS / EOF) is a *url.Error whose string
		// embeds the FULL signed URL, and the signed query IS a live bearer credential (X-Amz-Signature /
		// X-Goog-Signature / SAS token) granting read of the raw, un-stripped export object. This error
		// propagates to the scheduler's slog.Warn → stdout → Loki, so the query MUST be stripped before it
		// can ever be rendered. The non-2xx path below uses ErrSnippet (bounded body, no URL) — already safe.
		return nil, 0, 0, false, fmt.Errorf("portkey logs_export: signed-url download: %w", httpx.RedactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	// Decide whether the server honoured the Range. 206 + a Content-Range whose start == byteOffset ⇒ the
	// body IS the tail from byteOffset, so we must NOT line-skip (the prefix is already absent). A 200 ⇒ the
	// body starts at byte 0 (first chunk, or a server that ignored Range) ⇒ fall back to skipping `skip`
	// lines. A 206 we can't verify is refused loudly (never risk byte-misaligned page corruption).
	rangeHonored := false
	switch resp.StatusCode {
	case http.StatusOK:
		// full body from byte 0 — the line-skip fallback path below handles resume
	case http.StatusPartialContent:
		if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); byteOffset > 0 && ok && start == byteOffset {
			rangeHonored = true
		} else {
			return nil, 0, 0, false, fmt.Errorf("portkey logs_export: signed-url returned 206 with unexpected Content-Range %q (wanted start %d)", resp.Header.Get("Content-Range"), byteOffset)
		}
	default:
		return nil, 0, 0, false, fmt.Errorf("portkey logs_export: signed-url download status %d%s", resp.StatusCode, errBodySuffix(httpx.ErrSnippet(resp)))
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

	// consumed = raw bytes read from THIS response via readLine (advances the byte cursor). base is the
	// file offset at which this response's body starts: byteOffset when the Range was honoured, else 0.
	var consumed int64
	base := int64(0)
	skipLines := skip
	if rangeHonored {
		base = byteOffset // body starts at byteOffset ⇒ next offset = byteOffset + consumed
		skipLines = 0     // the prefix is already absent — never line-skip a honoured Range
	}

	// Skip the already-consumed lines of this page (200-fallback resume). An over-long line in the skip
	// region is drained like any other (its tooLong flag is ignored — it was counted when first read).
	// One line at a time ⇒ bounded memory even across the skip.
	for range skipLines {
		_, _, c, rerr := readLine(br, lineCap)
		consumed += int64(c)
		if rerr == io.EOF {
			// Fewer lines than the saved offset — the object ended early (shouldn't happen for a stable
			// S3 object). Treat the page as exhausted rather than crash; the window's frontier won't
			// advance unless this was the last page, so a genuine shortfall stays loud via window_lag.
			return nil, skip, base + consumed, true, nil
		}
		if rerr != nil {
			return nil, skip, 0, false, fmt.Errorf("portkey logs_export: stream signed-url body: %w", rerr)
		}
	}

	recs = make([]model.LogRecord, 0, max)
	for linesRead < max {
		line, tooLong, c, rerr := readLine(br, lineCap)
		if rerr == io.EOF {
			break
		}
		consumed += int64(c)
		if rerr != nil {
			return recs, linesRead, 0, false, fmt.Errorf("portkey logs_export: stream signed-url body: %w", rerr)
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
			// Skip it, loudly, and keep the page progressing — the line offset still advances so we never
			// re-attempt this line. Fire the alertable self-metric (#66): a Warn log alone is invisible to
			// the metrics-based self-obs stack, so a SYSTEMATIC format change dropping 100% of records would
			// otherwise complete the window silently. Counted → rate(line_unparseable)>0 is alertable.
			slog.Warn("portkey logs_export: skipping unparseable export line",
				"source", l.sourceInstance, "line", skip+linesRead)
			l.lineMalformed()
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
		return nil, 0, 0, false, fmt.Errorf("portkey logs_export: signed-url object exceeds %d bytes (truncated) — raise download cap / shrink window", maxBytes)
	}
	// Fewer than a full chunk ⇒ end of page. (When a page is an exact multiple of `max`, the final
	// non-empty chunk reports eof=false and the NEXT chunk reads zero lines ⇒ eof=true — one extra,
	// cheap, idempotent re-download. Correctness over micro-optimisation.)
	eof = linesRead < max
	return recs, linesRead, base + consumed, eof, nil
}

// contentRangeStart parses the START byte of a `Content-Range: bytes START-END/TOTAL` header (RFC 7233).
// Returns ok=false on any header we don't recognise, so an unverifiable 206 is refused rather than trusted.
func contentRangeStart(h string) (int64, bool) {
	h = strings.TrimSpace(h)
	const p = "bytes "
	if !strings.HasPrefix(h, p) {
		return 0, false
	}
	rest := h[len(p):]
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest[:dash]), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// readLine reads ONE newline-terminated logical line from br, bounded so a pathologically long line can
// never blow memory or wedge the loop. It returns the line WITHOUT the trailing '\n'. If the line's
// content exceeds `limit` bytes it DRAINS the remainder up to (and including) the next newline WITHOUT
// retaining it, and returns tooLong=true — so the reader always advances PAST the bad line rather than
// aborting the whole scan the way bufio.Scanner does on ErrTooLong (issue #35). At a clean EOF with no
// pending bytes it returns io.EOF; at EOF with a final unterminated line it returns that line. Any other
// read error is returned verbatim. Peak memory is ~limit + one bufio buffer, regardless of line size.
// It also returns `consumed`: the RAW bytes read from the stream for this line INCLUDING the trailing '\n'
// (and including all drained bytes of an over-long line) — the caller sums these to advance the byte cursor
// to the next line boundary for a Range-based resume (#61). At a clean EOF consumed is 0.
func readLine(br *bufio.Reader, limit int) (line []byte, tooLong bool, consumed int, err error) {
	var buf []byte
	var total int // content bytes seen for this line so far (may exceed len(buf) once truncated)
	for {
		frag, e := br.ReadSlice('\n')
		consumed += len(frag) // RAW bytes incl. the delimiter — advances the byte cursor
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
			return buf, tooLong, consumed, nil
		case io.EOF:
			if total == 0 {
				return nil, false, consumed, io.EOF // clean end, nothing pending (consumed==0)
			}
			return buf, tooLong, consumed, nil // final unterminated line
		default:
			return nil, false, consumed, e
		}
	}
}
