// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// scopeProbeWindow is the lookback for the workspace-scope assertion. Wide (7d) so a correctly-scoped but
// low-traffic workspace still returns >=1 row — the /analytics/groups/workspace dimension only lists
// workspaces that had traffic in the window, so a short window risks a false "undeterminable".
const scopeProbeWindow = 7 * 24 * time.Hour

// workspaceScopeResult is the 3-state outcome of a scope probe.
type workspaceScopeResult int

const (
	scopeMatched        workspaceScopeResult = iota // key's analytics scope is EXACTLY the expected workspace
	scopeMismatch                                   // key sees a DIFFERENT or MORE-THAN-ONE workspace (too broad)
	scopeUndeterminable                             // no traffic in the probe window ⇒ cannot assert (don't block)
)

// checkWorkspaceScope asserts the API key's analytics DATA scope is exactly `expected` by reading the
// `workspace` analytics dimension (GET /analytics/groups/workspace). Portkey IGNORES per-request workspace
// targeting on the analytics/groups endpoints (scope is bound to the key — live-probed, followup §4), so
// this is the ONLY way to detect a too-broad (global/admin) key before it emits cross-workspace aggregates
// into one environment's metrics. Returns (result, detail, err): err != nil is a TRANSIENT probe failure
// (unreachable / non-200 / decode) — the caller must RETRY, never treat it as a mismatch (no false alarm
// on a Portkey blip). `detail` is the observed workspace set, for the mismatch log.
func checkWorkspaceScope(ctx context.Context, hc *httpx.Client, baseURL, authHdr, authVal, expected, loop, sourceInstance string, now time.Time, onAuthError func(loop, source string)) (workspaceScopeResult, string, error) {
	u, err := url.Parse(baseURL + "/analytics/groups/workspace")
	if err != nil {
		return scopeUndeterminable, "", err
	}
	q := url.Values{}
	q.Set("time_of_generation_min", now.Add(-scopeProbeWindow).UTC().Format(time.RFC3339))
	q.Set("time_of_generation_max", now.UTC().Format(time.RFC3339))
	q.Set("page_size", "100")
	q.Set("current_page", "0")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return scopeUndeterminable, "", err
	}
	req.Header.Set(authHdr, authVal)
	resp, err := hc.Do(req)
	if err != nil {
		return scopeUndeterminable, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		// A 401/403 here is a credential failure (revoked/expired/misconfigured key), not a transient blip.
		// The probe runs BEFORE the fetch path that carries the usual onAuthError instrumentation, so
		// without firing it here a bad key with expected_workspace set never increments auth_errors_total
		// while scope is unverified (fresh deploy / restart / failover / no-traffic re-probe) — #103.
		if source.IsAuthStatus(resp.StatusCode) && onAuthError != nil {
			onAuthError(loop, sourceInstance)
		}
		return scopeUndeterminable, "", fmt.Errorf("workspace scope probe: status %d", resp.StatusCode)
	}
	var gr groupsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&gr); err != nil {
		return scopeUndeterminable, "", fmt.Errorf("workspace scope probe: decode: %w", err)
	}

	// The dimension value is the `workspace` field per row (a slug, e.g. "ws-acme-001" — live-probed).
	seen := map[string]bool{}
	for _, row := range gr.Data {
		var ws string
		if raw, ok := row["workspace"]; ok {
			_ = json.Unmarshal(raw, &ws)
		}
		if ws != "" {
			seen[ws] = true
		}
	}
	if len(seen) == 0 {
		return scopeUndeterminable, "no workspace rows in the probe window", nil
	}
	if len(seen) == 1 && seen[expected] {
		return scopeMatched, expected, nil
	}
	got := make([]string, 0, len(seen))
	for w := range seen {
		got = append(got, w)
	}
	sort.Strings(got)
	return scopeMismatch, strings.Join(got, ","), nil
}

// verifyScopeForCollect runs the one-time workspace-scope assertion at the top of a key-scoped loop's
// Collect (analytics/groups). Returns (verified, err):
//   - matched      ⇒ (true, nil): cache it; never re-check.
//   - mismatch     ⇒ (false, err) AND fires onGraphSkipped(loop,"workspace_scope_mismatch"): the caller
//     bubbles the error so the loop REFUSES TO EMIT (loud, no advance, window_lag grows) — the resilient
//     "stay up but emit nothing wrong" posture; recovers without restart once the key is fixed.
//   - undeterminable (no traffic) ⇒ (false, nil): proceed unverified, re-check next tick (don't block a
//     legitimately-quiet workspace).
//   - transient probe failure     ⇒ (false, err) WITHOUT the hook: retryable, not a real mismatch.
func verifyScopeForCollect(ctx context.Context, hc *httpx.Client, baseURL, authHdr, authVal, expected, loop, sourceInstance string, now time.Time, onGraphSkipped func(loop, graph string), onAuthError func(loop, source string)) (bool, error) {
	res, detail, err := checkWorkspaceScope(ctx, hc, baseURL, authHdr, authVal, expected, loop, sourceInstance, now, onAuthError)
	if err != nil {
		return false, fmt.Errorf("portkey %s: workspace scope probe failed (transient; retrying): %w", loop, err)
	}
	switch res {
	case scopeMatched:
		return true, nil
	case scopeUndeterminable:
		slog.Warn("portkey: workspace scope unverified (no traffic in the probe window) — proceeding, will re-check",
			"loop", loop, "expected_workspace", expected)
		return false, nil
	default: // scopeMismatch
		if onGraphSkipped != nil {
			onGraphSkipped(loop, "workspace_scope_mismatch")
		}
		return false, fmt.Errorf("portkey %s: API key analytics scope is %q but expected_workspace=%q — refusing to emit (the key is too broad; use a workspace-scoped key)", loop, detail, expected)
	}
}
