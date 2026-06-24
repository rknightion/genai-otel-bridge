// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grafana-ps/aip-oi/internal/httpx"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// errBodySuffix appends ": <snippet>" when the captured upstream error body is non-empty, else "" — so a
// "status N" error reads "status 4xx: {detail...}" without a dangling ": " when the body was empty. The
// upstream error body is operational (a vendor error message), not user content. Shared across portkey.
func errBodySuffix(snippet string) string {
	if snippet == "" {
		return ""
	}
	return ": " + snippet
}

// Export job status values (PoC §2 observed state machine: draft → queued → in_progress → success;
// terminal alternatives failed, stopped). Anything unrecognised is treated as a terminal failure.
const (
	statusDraft      = "draft"
	statusQueued     = "queued"
	statusInProgress = "in_progress"
	statusSuccess    = "success"
)

// Export lifecycle wire shapes (PoC §1/§2 — measured). Only the fields we use are decoded.
type createExportResp struct {
	ID    string `json:"id"`
	Total int    `json:"total"` // matched count at create, fixed for the (settled) window
}
type exportDetailResp struct {
	Status string `json:"status"`
}
type downloadResp struct {
	SignedURL string `json:"signed_url"`
}

// createExport registers a content-free export DEFINITION for [winMin, winMax] at the given page and
// returns (job id, matched-record total). requested_data lists only operational fields, but that is NOT
// an egress filter (PoC §3: Portkey injects metadata/portkeyHeaders regardless), so the downloaded
// payload is ALWAYS stripped our side. The description is a stable, content-free trace tag (there is no
// API delete; terminal jobs persist — §6 — so the tag aids manual audit, never carries content).
func (l *logsExportLoop) createExport(ctx context.Context, winMin, winMax time.Time, page int) (string, int, error) {
	filters := map[string]any{
		"time_of_generation_min": winMin.UTC().Format(time.RFC3339),
		"time_of_generation_max": winMax.UTC().Format(time.RFC3339),
		"page_size":              l.pageSize,
		"current_page":           page,
	}
	// api_key_ids scopes the export to a specific set of API keys (the per-use-case filter). Confirmed
	// accepted by the 2026-06-24 live probe (605 vs 1726 records with/without the filter).
	if l.apiKeyIDs != "" {
		filters["api_key_ids"] = strings.Split(l.apiKeyIDs, ",")
	}
	body := map[string]any{
		"workspace_id":   l.workspaceID,
		"filters":        filters,
		"requested_data": l.requestedData,
		"description": fmt.Sprintf("aip-oi/%s/logs/%s..%s", l.sourceInstance,
			winMin.UTC().Format(time.RFC3339), winMax.UTC().Format(time.RFC3339)),
	}
	var out createExportResp
	if err := l.lifecycle(ctx, http.MethodPost, "/logs/exports", body, &out); err != nil {
		return "", 0, err
	}
	return out.ID, out.Total, nil
}

func (l *logsExportLoop) startExport(ctx context.Context, jobID string) error {
	return l.lifecycle(ctx, http.MethodPost, "/logs/exports/"+url.PathEscape(jobID)+"/start", nil, nil)
}

func (l *logsExportLoop) pollExport(ctx context.Context, jobID string) (string, error) {
	var out exportDetailResp
	if err := l.lifecycle(ctx, http.MethodGet, "/logs/exports/"+url.PathEscape(jobID), nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

func (l *logsExportLoop) getDownloadURL(ctx context.Context, jobID string) (string, error) {
	var out downloadResp
	if err := l.lifecycle(ctx, http.MethodGet, "/logs/exports/"+url.PathEscape(jobID)+"/download", nil, &out); err != nil {
		return "", err
	}
	if out.SignedURL == "" {
		return "", fmt.Errorf("portkey logs_export: download returned an empty signed_url")
	}
	return out.SignedURL, nil
}

// cancelExport is best-effort (PoC §1: valid only on queued/in_progress; a terminal job returns 400
// AB01). The caller ignores its error — cancel is a courtesy to stop a runaway job, never load-bearing.
func (l *logsExportLoop) cancelExport(ctx context.Context, jobID string) error {
	return l.lifecycle(ctx, http.MethodPost, "/logs/exports/"+url.PathEscape(jobID)+"/cancel", nil, nil)
}

// lifecycle performs one control-plane call via the SHARED httpx client (rate-limited + egress-guarded),
// sets the vendor auth header, and decodes a 2xx JSON body into out (nil ⇒ discard). A non-2xx is a
// plain error carrying the status — these calls drive the step machine, where any error means "no
// advance, re-pull next tick" (the loud, idempotent path), so they need no reject taxonomy of their own.
func (l *logsExportLoop) lifecycle(ctx context.Context, method, path string, reqBody, out any) error {
	var rdr io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set(l.authHdr, l.authVal)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// followup §9: a 401/403 on ANY lifecycle call (create/start/poll/download/cancel) is a credential
		// failure → its own alertable signal. l.Key().Loop == "logs_export" (matches groups.go:295).
		if source.IsAuthStatus(resp.StatusCode) && l.onAuthError != nil {
			l.onAuthError(l.Key().Loop, l.sourceInstance)
		}
		return fmt.Errorf("portkey logs_export: %s %s status %d%s", method, path, resp.StatusCode, errBodySuffix(httpx.ErrSnippet(resp)))
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
			return fmt.Errorf("portkey logs_export: decode %s: %w", path, err)
		}
	}
	return nil
}
