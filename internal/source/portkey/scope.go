// SPDX-License-Identifier: Apache-2.0

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
	// The legacy check sends no workspace_slug: a source with expected_workspace has no scope (New refuses
	// the two together), so its request is byte-identical to the one it sent before scopes existed.
	gr, status, err := fetchWorkspaceGroups(ctx, hc, baseURL, authHdr, authVal, "", now)
	if err != nil {
		return scopeUndeterminable, "", err
	}
	if status != http.StatusOK {
		// A 401/403 here is a credential failure (revoked/expired/misconfigured key), not a transient blip.
		// The probe runs BEFORE the fetch path that carries the usual onAuthError instrumentation, so
		// without firing it here a bad key with expected_workspace set never increments auth_errors_total
		// while scope is unverified (fresh deploy / restart / failover / no-traffic re-probe) — #103.
		if source.IsAuthStatus(status) && onAuthError != nil {
			onAuthError(loop, sourceInstance)
		}
		return scopeUndeterminable, "", source.WrapAuth(status, fmt.Errorf("workspace scope probe: status %d", status))
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

// fetchWorkspaceGroups is the one workspace-grouping request, shared by the legacy expected_workspace
// check and the scope assertion: GET <base>/analytics/groups/workspace over the scopeProbeWindow ending
// at now, first page of 100. slug, when set, is sent as workspace_slug (a source with a scope sends it on
// every analytics and groups request). A non-200 answer returns its status with the body drained and no
// error, so each caller reads the status its own way. A 200 whose body does not decode is an error
// prefixed "workspace scope probe: decode:".
func fetchWorkspaceGroups(ctx context.Context, hc *httpx.Client, baseURL, authHdr, authVal, slug string, now time.Time) (groupsResponse, int, error) {
	u, err := url.Parse(baseURL + "/analytics/groups/workspace")
	if err != nil {
		return groupsResponse{}, 0, err
	}
	q := url.Values{}
	q.Set("time_of_generation_min", now.Add(-scopeProbeWindow).UTC().Format(time.RFC3339))
	q.Set("time_of_generation_max", now.UTC().Format(time.RFC3339))
	q.Set("page_size", "100")
	q.Set("current_page", "0")
	if slug != "" {
		q.Set("workspace_slug", slug)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return groupsResponse{}, 0, err
	}
	req.Header.Set(authHdr, authVal)
	resp, err := hc.Do(req)
	if err != nil {
		return groupsResponse{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return groupsResponse{}, resp.StatusCode, nil
	}
	var gr groupsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&gr); err != nil {
		return groupsResponse{}, resp.StatusCode, fmt.Errorf("workspace scope probe: decode: %w", err)
	}
	return gr, resp.StatusCode, nil
}

// scopeMaxLookback is the longest lookback a loop may have when its source has a scope: one day inside
// scopeProbeWindow, so the traffic a loop reads is always traffic the assertion has seen (design §5.1).
const scopeMaxLookback = 6 * 24 * time.Hour

// assertWorkspaceScope is the scope assertion's strict reading of the workspace grouping (design §5.1),
// requested with the scope name as workspace_slug:
//   - exactly one workspace, equal to name: Matched;
//   - another workspace, more than one, or a row with no workspace (empty, null or absent): Mismatch;
//   - no row: Unverifiable.
//
// Anything else is an error and never a result: a failed call, any non-200 status (401 and 403 are
// *source.AuthError), the vendor's quota flag, a body that does not decode, or a workspace value that is
// not a string. Unlike the legacy check it never calls Deps.OnAuthError (the scheduler raises the
// auth-error signal for a gate's call), and no error or log line carries the API key or a workspace
// name the response returned.
func assertWorkspaceScope(ctx context.Context, hc *httpx.Client, baseURL, authHdr, authVal, name, sourceInstance string, now time.Time) (source.ScopeResult, error) {
	gr, status, err := fetchWorkspaceGroups(ctx, hc, baseURL, authHdr, authVal, name, now)
	if err != nil {
		return 0, fmt.Errorf("portkey: scope assertion: %w", err)
	}
	if status != http.StatusOK {
		if source.IsAuthStatus(status) {
			return 0, &source.AuthError{Status: status}
		}
		return 0, fmt.Errorf("portkey: scope assertion: workspace grouping status %d", status)
	}
	if gr.IsQuotaExceeded {
		return 0, fmt.Errorf("portkey: scope assertion: workspace grouping: %w", source.ErrQuotaExceeded)
	}
	seen := map[string]bool{}
	noWorkspace := 0
	for i, row := range gr.Data {
		raw, ok := row["workspace"]
		if !ok {
			noWorkspace++
			continue
		}
		var ws *string // a JSON null decodes to nil: a row with no workspace, as an empty string is
		if err := json.Unmarshal(raw, &ws); err != nil {
			return 0, fmt.Errorf("portkey: scope assertion: workspace grouping row %d: the workspace value is not a string", i)
		}
		if ws == nil || *ws == "" {
			noWorkspace++
			continue
		}
		seen[*ws] = true
	}
	switch {
	case len(gr.Data) == 0:
		return source.ScopeUnverifiable, nil
	case noWorkspace == 0 && len(seen) == 1 && seen[name]:
		return source.ScopeMatched, nil
	default:
		// The other workspaces' names are not logged: they may belong to another tenant.
		slog.Warn("portkey: scope assertion: the API key reads data outside its scope",
			"source", sourceInstance, "workspaces_seen", len(seen), "scope_seen", seen[name], "rows_without_workspace", noWorkspace)
		return source.ScopeMismatch, nil
	}
}

// scopedSource is the source New returns when its config has a scope: the loops of portkeySource, every
// analytics and groups request of which carries the scope name as workspace_slug, plus the scope
// assertion. The capability exists only on this type, so a source with no scope is not a
// source.ScopeAsserter and assembly gives it no gate.
type scopedSource struct {
	*portkeySource
	hc                                        *httpx.Client
	baseURL, authHdr, authVal, name, instance string
	now                                       func() time.Time
}

// AssertScope reports what the source's API key reads, from the workspace grouping over the last
// scopeProbeWindow, sent as the loops send theirs (same client, auth and slug). See assertWorkspaceScope.
func (s *scopedSource) AssertScope(ctx context.Context) (source.ScopeResult, error) {
	return assertWorkspaceScope(ctx, s.hc, s.baseURL, s.authHdr, s.authVal, s.name, s.instance, s.now())
}

var _ source.ScopeAsserter = (*scopedSource)(nil)

// verifyScopeForCollect runs the one-time workspace-scope assertion at the top of a key-scoped loop's
// Collect (analytics/groups). Returns (verified, err):
//   - matched      ⇒ (true, nil): cache it; never re-check.
//   - mismatch     ⇒ (false, err) AND fires OnCapability(loop,"workspace_scope",permission-denied): the caller
//     bubbles the error so the loop REFUSES TO EMIT (loud, no advance, window_lag grows) — the resilient
//     "stay up but emit nothing wrong" posture; recovers without restart once the key is fixed.
//   - undeterminable (no traffic) ⇒ (false, nil): proceed unverified, re-check next tick (don't block a
//     legitimately-quiet workspace).
//   - transient probe failure     ⇒ (false, err) WITHOUT the hook: retryable, not a real mismatch.
func verifyScopeForCollect(ctx context.Context, hc *httpx.Client, baseURL, authHdr, authVal, expected, loop, sourceInstance string, now time.Time, onCapability func(loop, graph string, state source.CapabilityState), onAuthError func(loop, source string)) (bool, error) {
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
		if onCapability != nil {
			onCapability(loop, "workspace_scope", source.CapabilityPermissionDenied)
		}
		return false, fmt.Errorf("portkey %s: API key analytics scope is %q but expected_workspace=%q — refusing to emit (the key is too broad; use a workspace-scoped key)", loop, detail, expected)
	}
}
