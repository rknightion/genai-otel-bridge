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

	"github.com/grafana-ps/aip-oi/internal/httpx"
	"github.com/grafana-ps/aip-oi/internal/model"
)

// defaultDownloadMaxBytes caps a single signed-URL object read (defence against a server returning an
// unbounded body). A page is ≤page_size operational records (small, content-free), so this is generous
// headroom; a real page is well under it. Stored as a loop field (overridable in tests) so the
// truncation-error path is exercisable without a multi-hundred-MiB fixture.
const defaultDownloadMaxBytes = 512 << 20 // 512 MiB

// maxLogLineBytes caps one JSONL line. Operational export records are small; a line beyond this is
// treated as malformed (corrupt or content-bearing) and skipped loudly, never parsed/stringified.
const maxLogLineBytes = 1 << 20 // 1 MiB

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
		return nil, 0, false, err
	}
	resp, err := l.dlClient.Do(req)
	if err != nil {
		return nil, 0, false, err
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
	cr := &countingReader{r: io.LimitReader(resp.Body, maxBytes+1)}
	sc := bufio.NewScanner(cr)
	sc.Buffer(make([]byte, 0, 64<<10), maxLogLineBytes)

	// Skip the already-consumed lines of this page (re-download resume). One line at a time ⇒ bounded
	// memory even across the skip.
	for range skip {
		if !sc.Scan() {
			// Fewer lines than the saved offset — the object ended early (shouldn't happen for a stable
			// S3 object). Treat the page as exhausted rather than crash; the window's frontier won't
			// advance unless this was the last page, so a genuine shortfall stays loud via window_lag.
			return nil, skip, true, sc.Err()
		}
	}

	recs = make([]model.LogRecord, 0, max)
	for linesRead < max && sc.Scan() {
		linesRead++
		var raw map[string]json.RawMessage
		if uerr := json.Unmarshal(sc.Bytes(), &raw); uerr != nil {
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
		if f := l.policy.metadataTraceID; f != "" && len(rec.TraceID) == 0 && rec.RecordAttributes[f] != "" {
			l.traceIDUnparsed()
		}
		recs = append(recs, rec)
	}
	if serr := sc.Err(); serr != nil {
		return recs, linesRead, false, fmt.Errorf("portkey logs_export: stream signed-url body: %w", serr)
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
