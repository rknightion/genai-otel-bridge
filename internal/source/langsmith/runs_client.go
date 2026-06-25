// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/httpx"
)

// defaultRunsResponseBytes is the per-page decode cap (DoS backstop). [adversarial-review M3] kept
// modest (matches the sessions loop's 32MiB) for an o11y-critical-path service: the whole page is held
// in RAM before the strip drops content (select does NOT trim content on 0.13.5 — probed), so the cap
// bounds transient memory. `page_size` (and root_only/run_type/session scoping) is the operator lever
// for content-heavy projects; an oversize page surfaces as a LOUD decode error, never a silent truncation.
const defaultRunsResponseBytes = 32 << 20

type runsQueryResp struct {
	Runs    []map[string]json.RawMessage `json:"runs"`
	Cursors struct {
		Next string `json:"next"`
	} `json:"cursors"`
}

// queryRuns POSTs one page of /runs/query for the session-scoped [winMin,winMax] window. Returns the raw
// run records, the next cursor ("" when exhausted), the HTTP status (caller maps the taxonomy), and a
// transport error. The body NEVER names a content field (the select is content-free). `sessions` is the
// resolved project-UUID scope (static or discovered) — runs/query 400s without one.
func (l *runsLoop) queryRuns(ctx context.Context, sessions []string, winMin, winMax time.Time, cursor string) ([]map[string]json.RawMessage, string, int, string, error) {
	body := map[string]any{
		"session":    sessions,
		"select":     l.selectFields,
		"start_time": winMin.UTC().Format(time.RFC3339),
		"end_time":   winMax.UTC().Format(time.RFC3339),
		"order":      "asc",
		"limit":      l.pageSize,
	}
	if cursor != "" {
		body["cursor"] = cursor
	}
	if l.rootOnly { // volume bound: only root runs (one log per trace)
		body["is_root"] = true
	}
	if l.runType != "" { // optional single run_type filter
		body["run_type"] = l.runType
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, "", 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/runs/query", bytes.NewReader(b))
	if err != nil {
		return nil, "", 0, "", err
	}
	req.Header.Set(l.authHdr, l.authVal)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.hc.Do(req)
	if err != nil {
		return nil, "", 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Capture a bounded prefix of the error body so a non-2xx self-diagnoses (the 422 select outage):
		// the caller folds it into the Collect error → the loop.tick trace exception + the warn log.
		return nil, "", resp.StatusCode, httpx.ErrSnippet(resp), nil
	}
	var out runsQueryResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, l.respCap())).Decode(&out); err != nil {
		return nil, "", resp.StatusCode, "", fmt.Errorf("langsmith runs: decode page (lower page_size if oversize): %w", err)
	}
	return out.Runs, out.Cursors.Next, resp.StatusCode, "", nil
}

// errBodySuffix appends ": <snippet>" when the captured upstream error body is non-empty, else "" —
// so a "status N" error reads "status 422: {detail...}" without a dangling ": " when the body was empty.
func errBodySuffix(snippet string) string {
	if snippet == "" {
		return ""
	}
	return ": " + snippet
}

// respCap is the per-response decode cap (settings.max_response_bytes, or the generous default).
func (l *runsLoop) respCap() int64 {
	if l.maxResponseBytes > 0 {
		return l.maxResponseBytes
	}
	return defaultRunsResponseBytes
}
