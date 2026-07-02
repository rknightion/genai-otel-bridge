// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// runsStatsResp decodes ONLY run_count from POST /runs/stats. Every other aggregate (cost/token/latency
// stats, and especially feedback_stats' `values` maps which hold raw ids) is DELIBERATELY ABSENT so it
// never enters process memory — content-safety by construction (the usage loop emits counts only).
type runsStatsResp struct {
	RunCount *int `json:"run_count"`
}

// fetchSpanCount POSTs /runs/stats scoped to one project over [statsStart, now], returning the ALL-RUNS
// count = spans (the storage/"excessive spans" driver). NO `is_root` filter (that would count traces,
// which we already get free from /sessions). Returns (count, http status, transport error); the caller
// maps the taxonomy (429 → quota; other non-200 → counted skip). runs/stats 400s without a scope, so the
// session id is always sent. The request body NEVER names a content field.
func (l *usageLoop) fetchSpanCount(ctx context.Context, sessionID string, statsStart time.Time) (*int, int, error) {
	body, err := json.Marshal(map[string]any{
		"session":    []string{sessionID},
		"start_time": statsStart.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/runs/stats", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set(l.authHdr, l.authVal)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return nil, resp.StatusCode, nil
	}
	var out runsStatsResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out.RunCount, resp.StatusCode, nil
}
