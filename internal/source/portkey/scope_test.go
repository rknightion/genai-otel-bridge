// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// wsServer serves GET /analytics/groups/workspace returning the given workspace dimension rows (+ a
// configurable status). Each row is {"workspace": <slug>, "requests": 1}.
func wsServer(t *testing.T, status int, workspaces ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/analytics/groups/workspace" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		rows := make([]string, 0, len(workspaces))
		for _, ws := range workspaces {
			rows = append(rows, fmt.Sprintf(`{"workspace":%q,"requests":1,"object":"analytics-group"}`, ws))
		}
		fmt.Fprintf(w, `{"object":"list","total":%d,"is_quota_exceeded":false,"data":[%s]}`, len(workspaces), strings.Join(rows, ","))
	}))
}

func scopeTestClient(t *testing.T) *httpx.Client {
	t.Helper()
	return httpx.New(httpx.Config{UserAgent: "test", AllowPrivate: true, Timeout: 5 * time.Second})
}

// TestCollectScopeProbeAuthWrapsErrAuth [#537]: with expected_workspace set, a 401/403 from the scope
// probe (which runs before the graph fetch) must satisfy errors.Is(err, source.ErrAuth) through Collect so
// the scheduler's auth backoff applies on this path too; the probe's own error text is kept.
func TestCollectScopeProbeAuthWrapsErrAuth(t *testing.T) {
	now := time.Unix(1_750_000_000, 0).UTC()
	for _, status := range []int{401, 403} {
		srv := wsServer(t, status)
		cfg := config.SourceConfig{
			Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-test",
			Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
			RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
			HTTP:      config.HTTPConfig{AllowPrivate: true},
			Loops: map[string]config.LoopConfig{"analytics": {
				Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
				BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
				MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
				Graphs:   []string{"requests"},
				Settings: map[string]string{"expected_workspace": "ws-acme-001"},
			}},
		}
		src, err := New(cfg, source.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		lp := src.Loops()[0].(*analyticsLoop)
		lp.now = func() time.Time { return now }
		_, err = lp.Collect(context.Background(), model.Watermark{Time: now.Add(-10 * time.Minute)})
		srv.Close()
		if !errors.Is(err, source.ErrAuth) {
			t.Fatalf("status %d: scope-probe failure must wrap source.ErrAuth, got %v", status, err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("workspace scope probe: status %d", status)) {
			t.Fatalf("status %d: probe error text must be kept, got %v", status, err)
		}
	}
}

func TestCheckWorkspaceScope(t *testing.T) {
	now := time.Unix(1_750_000_000, 0).UTC()
	cases := []struct {
		name       string
		status     int
		workspaces []string
		expected   string
		wantRes    workspaceScopeResult
		wantErr    bool
		wantAuth   bool // #103: a 401/403 probe must fire onAuthError (credential-failure signal)
	}{
		{"match", 200, []string{"ws-acme-001"}, "ws-acme-001", scopeMatched, false, false},
		{"wrong-single", 200, []string{"ws-other-12ab34"}, "ws-acme-001", scopeMismatch, false, false},
		{"multi-too-broad", 200, []string{"ws-acme-001", "ws-prod-99zz", "ws-test-55yy"}, "ws-acme-001", scopeMismatch, false, false},
		{"undeterminable-no-traffic", 200, nil, "ws-acme-001", scopeUndeterminable, false, false},
		{"transient-5xx", 503, nil, "ws-acme-001", scopeUndeterminable, true, false},
		{"auth-401", 401, nil, "ws-acme-001", scopeUndeterminable, true, true},
		{"auth-403", 403, nil, "ws-acme-001", scopeUndeterminable, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := wsServer(t, tc.status, tc.workspaces...)
			defer srv.Close()
			var auth [][2]string
			res, detail, err := checkWorkspaceScope(context.Background(), scopeTestClient(t), srv.URL, "x-portkey-api-key", "k", tc.expected, "analytics", "pk-test", now,
				func(loop, s string) { auth = append(auth, [2]string{loop, s}) })
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if res != tc.wantRes {
				t.Fatalf("res=%v want %v (detail=%q)", res, tc.wantRes, detail)
			}
			if tc.name == "multi-too-broad" && !strings.Contains(detail, "ws-prod-99zz") {
				t.Fatalf("mismatch detail should list the observed workspaces, got %q", detail)
			}
			if tc.wantAuth {
				if len(auth) != 1 || auth[0] != [2]string{"analytics", "pk-test"} {
					t.Fatalf("auth status must fire OnAuthError once (analytics,pk-test), got %v", auth)
				}
			} else if len(auth) != 0 {
				t.Fatalf("non-auth status must NOT fire OnAuthError, got %v", auth)
			}
		})
	}
}

// TestGroupsCollectRefusesOnWorkspaceScopeMismatch proves the guardrail is WIRED into Collect: a key whose
// analytics scope is a different/too-broad workspace makes Collect refuse to emit (error, zero samples) and
// fire the alertable hook — the "stay up, emit nothing wrong" posture. Only /analytics/groups/workspace is
// hit (the check returns before any data fetch), so wsServer suffices.
func TestGroupsCollectRefusesOnWorkspaceScopeMismatch(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := wsServer(t, 200, "ws-global-admin") // key sees the wrong (too-broad) workspace
	defer srv.Close()
	var capabilities [][3]string
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"expected_workspace": "ws-acme-001", "page_size": "100"}),
		source.Deps{OnCapability: func(l, g string, state source.CapabilityState) {
			capabilities = append(capabilities, [3]string{l, g, string(state)})
		}}, now)
	batch, err := gl.Collect(context.Background(), model.Watermark{})
	if err == nil {
		t.Fatal("scope mismatch must refuse to emit (error)")
	}
	if len(batch.Samples) != 0 {
		t.Fatalf("must emit nothing on mismatch, got %d samples", len(batch.Samples))
	}
	want := [3]string{"groups", "workspace_scope", string(source.CapabilityPermissionDenied)}
	if len(capabilities) != 1 || capabilities[0] != want {
		t.Fatalf("want OnCapability %v, got %v", want, capabilities)
	}
}

// TestScopeProbeAuthErrorFiresForBothLoops proves the Collect-path wiring (#103): with expected_workspace
// set and scope UNVERIFIED, a 401 on the scope probe fires Deps.OnAuthError for BOTH the analytics and
// groups loops — so a credential failure increments auth_errors_total even though the probe returns before
// the graph/endpoint fetch path that carries the usual onAuthError instrumentation.
func TestScopeProbeAuthErrorFiresForBothLoops(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	t.Run("groups", func(t *testing.T) {
		srv := wsServer(t, 401) // scope probe 401 (revoked/expired key)
		defer srv.Close()
		var auth [][2]string
		gl := mkGroups(t, groupsCfg(srv, map[string]string{"expected_workspace": "ws-acme-001", "page_size": "100"}),
			source.Deps{OnAuthError: func(loop, s string) { auth = append(auth, [2]string{loop, s}) }}, now)
		if _, err := gl.Collect(context.Background(), model.Watermark{}); err == nil {
			t.Fatal("a 401 scope probe must make Collect refuse to emit (error)")
		}
		if len(auth) != 1 || auth[0] != [2]string{"groups", "pk-test"} {
			t.Fatalf("OnAuthError want one (groups,pk-test), got %v", auth)
		}
	})

	t.Run("analytics", func(t *testing.T) {
		srv := wsServer(t, 401)
		defer srv.Close()
		var auth [][2]string
		cfg := config.SourceConfig{
			Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-test",
			Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
			RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
			HTTP:      config.HTTPConfig{AllowPrivate: true},
			Loops: map[string]config.LoopConfig{"analytics": {
				Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
				BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
				MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
				Graphs:   []string{"requests"},
				Settings: map[string]string{"expected_workspace": "ws-acme-001"},
			}},
		}
		src, err := New(cfg, source.Deps{OnAuthError: func(loop, s string) { auth = append(auth, [2]string{loop, s}) }})
		if err != nil {
			t.Fatal(err)
		}
		lp := src.Loops()[0].(*analyticsLoop)
		lp.now = func() time.Time { return now }
		if _, err := lp.Collect(context.Background(), model.Watermark{}); err == nil {
			t.Fatal("a 401 scope probe must make Collect refuse to emit (error)")
		}
		if len(auth) != 1 || auth[0] != [2]string{"analytics", "pk-test"} {
			t.Fatalf("OnAuthError want one (analytics,pk-test), got %v", auth)
		}
	})
}

// TestVerifyScopeForCollect: the Collect-path wrapper — match ⇒ (true,nil); mismatch ⇒ (false,err) AND
// fires the alertable hook; undeterminable ⇒ (false,nil) proceed unverified; transient ⇒ (false,err) no hook.
func TestVerifyScopeForCollect(t *testing.T) {
	now := time.Unix(1_750_000_000, 0).UTC()
	t.Run("match", func(t *testing.T) {
		srv := wsServer(t, 200, "ws-acme-001")
		defer srv.Close()
		var hookFired bool
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "analytics", "pk-test", now, func(string, string, source.CapabilityState) { hookFired = true }, nil)
		if !ok || err != nil || hookFired {
			t.Fatalf("match: ok=%v err=%v hook=%v", ok, err, hookFired)
		}
	})
	t.Run("mismatch-fires-hook-and-errors", func(t *testing.T) {
		srv := wsServer(t, 200, "ws-global-admin")
		defer srv.Close()
		var gotLoop, gotGraph string
		var gotState source.CapabilityState
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "analytics", "pk-test", now, func(l, g string, state source.CapabilityState) { gotLoop, gotGraph, gotState = l, g, state }, nil)
		if ok || err == nil {
			t.Fatalf("mismatch must refuse: ok=%v err=%v", ok, err)
		}
		if gotLoop != "analytics" || gotGraph != "workspace_scope" || gotState != source.CapabilityPermissionDenied {
			t.Fatalf("hook args = %q/%q/%q, want analytics/workspace/permission-denied", gotLoop, gotGraph, gotState)
		}
	})
	t.Run("undeterminable-proceeds", func(t *testing.T) {
		srv := wsServer(t, 200)
		defer srv.Close()
		var hookFired bool
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "groups", "pk-test", now, func(string, string, source.CapabilityState) { hookFired = true }, nil)
		if ok || err != nil || hookFired {
			t.Fatalf("undeterminable: want (false,nil,no-hook), got ok=%v err=%v hook=%v", ok, err, hookFired)
		}
	})
	t.Run("transient-errors-no-hook", func(t *testing.T) {
		srv := wsServer(t, 503)
		defer srv.Close()
		var hookFired bool
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "groups", "pk-test", now, func(string, string, source.CapabilityState) { hookFired = true }, nil)
		if ok || err == nil || hookFired {
			t.Fatalf("transient: want (false,err,no-hook), got ok=%v err=%v hook=%v", ok, err, hookFired)
		}
	})
}

// Tests for unit 4b (#526): the scope assertion, the workspace slug and the scope refusals.

const testScopeName = "ws-acme-001"

// scopedSourceCfg is a source with a scope, the analytics and groups loops (a metadata key and an
// api_key_ids filter on both, so every request shape is exercised), against baseURL.
func scopedSourceCfg(baseURL string, scope config.ScopeConfig) config.SourceConfig {
	cfg := legacyURLConfig(baseURL,
		map[string]string{"api_key_ids": "uuid-1,uuid-2"},
		map[string]string{"page_size": "100", "api_key_ids": "uuid-1,uuid-2", "metadata_keys": "use_case"})
	cfg.Auth.Value = "api-key-value-must-not-leak"
	cfg.Scope = scope
	return cfg
}

// asserterOf builds cfg and returns its scope asserter, failing the test when the source has none.
func asserterOf(t *testing.T, cfg config.SourceConfig, deps source.Deps) source.ScopeAsserter {
	t.Helper()
	src, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := src.(source.ScopeAsserter)
	if !ok {
		t.Fatalf("a source with a scope must implement source.ScopeAsserter, got %T", src)
	}
	return a
}

// workspaceAnswer serves the workspace grouping with a fixed status and body.
func workspaceAnswer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/analytics/groups/workspace" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wsRows is a workspace-grouping body whose rows carry the given raw JSON workspace values; a value of
// "-" leaves the field out of its row.
func wsRows(raw ...string) string {
	rows := make([]string, 0, len(raw))
	for _, v := range raw {
		if v == "-" {
			rows = append(rows, `{"requests":1,"object":"analytics-group"}`)
			continue
		}
		rows = append(rows, fmt.Sprintf(`{"workspace":%s,"requests":1,"object":"analytics-group"}`, v))
	}
	return fmt.Sprintf(`{"object":"list","total":%d,"is_quota_exceeded":false,"data":[%s]}`, len(raw), strings.Join(rows, ","))
}

// TestSlugOnEveryScopedRequest (AC2): with a scope, every analytics, groups and assertion request
// carries workspace_slug=<scope name>, exactly once and escaped; the analytics URL appends it after
// api_key_ids. Apart from the slug, each request is the one the same source sends with no scope.
func TestSlugOnEveryScopedRequest(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 10, 30, 0, time.UTC)
	f := newURIRecorder(t, now.Add(-20*time.Minute))
	const name = "ws acme/001&x=y" // escaping matters: a raw & or space would split or end the value
	collect := func(cfg config.SourceConfig) []string {
		t.Helper()
		src, err := New(cfg, source.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		f.take()
		for _, lp := range src.Loops() {
			if !SetLoopClockForTest(lp, func() time.Time { return now }) {
				t.Fatalf("no clock for loop %s", lp.Key().Loop)
			}
			if _, err := lp.Collect(context.Background(), model.Watermark{Time: now.Add(-30 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
		}
		return f.take()
	}
	scopedCfg := scopedSourceCfg(f.URL+"/v1", config.ScopeConfig{Name: name, ID: "ws-id-1"})
	unscopedCfg := scopedCfg
	unscopedCfg.Scope = config.ScopeConfig{}
	scoped, unscoped := collect(scopedCfg), collect(unscopedCfg)

	paths := map[string]bool{}
	for _, uri := range scoped {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		paths[u.Path] = true
		if got := u.Query()["workspace_slug"]; len(got) != 1 || got[0] != name {
			t.Errorf("%s: workspace_slug = %q, want exactly [%q]", u.Path, got, name)
		}
	}
	for _, p := range []string{"/v1/analytics/graphs/latency", "/v1/analytics/graphs/requests", "/v1/analytics/groups/ai-models",
		"/v1/analytics/groups/metadata/use_case", "/v1/analytics/groups/prompt"} {
		if !paths[p] {
			t.Errorf("no scoped request to %s was seen (seen %v)", p, paths)
		}
	}
	if len(scoped) != len(unscoped) {
		t.Fatalf("a scope changed the request count: %d scoped, %d unscoped", len(scoped), len(unscoped))
	}
	slug := "&workspace_slug=" + url.QueryEscape(name)
	for i := range scoped {
		if strings.Contains(scoped[i], "/analytics/graphs/") {
			// Hand-built: the slug is appended after api_key_ids and nothing else moves.
			if scoped[i] != unscoped[i]+slug {
				t.Errorf("analytics request %d:\n got %s\nwant %s", i, scoped[i], unscoped[i]+slug)
			}
			continue
		}
		su, _ := url.Parse(scoped[i])
		uu, _ := url.Parse(unscoped[i])
		q := su.Query()
		q.Del("workspace_slug")
		if su.Path != uu.Path || q.Encode() != uu.RawQuery {
			t.Errorf("groups request %d differs beyond the slug:\n got %s\nwant %s plus the slug", i, scoped[i], unscoped[i])
		}
	}

	// The assertion request, after the loops' requests have been checked.
	if res, err := asserterOf(t, scopedCfg, source.Deps{}).AssertScope(context.Background()); err != nil || res != source.ScopeMismatch {
		t.Fatalf("the fake answers ws-acme-001, which is not the scope: want Mismatch, got %d, %v", res, err)
	}
	assertion := f.take()
	if len(assertion) != 1 || !strings.HasPrefix(assertion[0], "/v1/analytics/groups/workspace?") {
		t.Fatalf("the assertion sent %q, want one workspace grouping request", assertion)
	}
	u, err := url.Parse(assertion[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query()["workspace_slug"]; len(got) != 1 || got[0] != name {
		t.Errorf("assertion: workspace_slug = %q, want exactly [%q]", got, name)
	}
}

// TestAssertScopeOutcomes (AC3): the strict reading of the workspace grouping. Exactly one workspace,
// equal to the scope name, is Matched; another workspace, several, or a row with no workspace (empty,
// null or absent, even beside the right one) is Mismatch; no row at all is Unverifiable.
func TestAssertScopeOutcomes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want source.ScopeResult
	}{
		{"matched", wsRows(`"ws-acme-001"`), source.ScopeMatched},
		{"matched, one workspace on two rows", wsRows(`"ws-acme-001"`, `"ws-acme-001"`), source.ScopeMatched},
		{"mismatch, another workspace", wsRows(`"ws-other-12ab"`), source.ScopeMismatch},
		{"mismatch, the name in another case", wsRows(`"WS-ACME-001"`), source.ScopeMismatch},
		{"mismatch, several workspaces", wsRows(`"ws-acme-001"`, `"ws-other-12ab"`), source.ScopeMismatch},
		{"mismatch, an empty workspace", wsRows(`""`), source.ScopeMismatch},
		{"mismatch, an empty workspace beside the scope", wsRows(`"ws-acme-001"`, `""`), source.ScopeMismatch},
		{"mismatch, a null workspace beside the scope", wsRows(`"ws-acme-001"`, `null`), source.ScopeMismatch},
		{"mismatch, a row with no workspace field beside the scope", wsRows(`"ws-acme-001"`, "-"), source.ScopeMismatch},
		{"unverifiable, no traffic", wsRows(), source.ScopeUnverifiable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := workspaceAnswer(t, http.StatusOK, c.body)
			got, err := asserterOf(t, scopedSourceCfg(srv.URL, config.ScopeConfig{Name: testScopeName, ID: "ws-id-1"}), source.Deps{}).AssertScope(context.Background())
			if err != nil {
				t.Fatalf("want result %d, got error %v", c.want, err)
			}
			if got != c.want {
				t.Fatalf("result = %d, want %d", got, c.want)
			}
		})
	}
}

// TestAssertScopeFailures (AC4): a call that cannot be read as a result is an error, never a result,
// even when its rows would match: any non-200 status, the vendor's quota flag, a body that does not
// decode, or a workspace value that is not a string. 401 and 403 are *source.AuthError (an ErrAuth).
// The asserter never calls Deps.OnAuthError (the scheduler raises the signal), and no error carries the
// API key.
func TestAssertScopeFailures(t *testing.T) {
	var authHooks []string
	deps := source.Deps{OnAuthError: func(loop, src string) { authHooks = append(authHooks, loop+"/"+src) }}
	match := wsRows(`"ws-acme-001"`)
	cases := []struct {
		name       string
		status     int
		body       string
		authStatus int // non-zero: the error must be a *source.AuthError with this status
	}{
		{"500", http.StatusInternalServerError, match, 0},
		{"503", http.StatusServiceUnavailable, "", 0},
		{"429", http.StatusTooManyRequests, match, 0},
		{"404", http.StatusNotFound, match, 0},
		{"202 with matching rows", http.StatusAccepted, match, 0},
		{"quota flag with matching rows", http.StatusOK, `{"object":"list","total":1,"is_quota_exceeded":true,"data":[{"workspace":"ws-acme-001","requests":1}]}`, 0},
		{"body is not JSON", http.StatusOK, "<html>gateway error</html>", 0},
		{"data is not a list", http.StatusOK, `{"object":"list","data":{"workspace":"ws-acme-001"}}`, 0},
		{"workspace is a number", http.StatusOK, wsRows(`42`), 0},
		{"workspace is an object beside the scope", http.StatusOK, wsRows(`"ws-acme-001"`, `{"slug":"ws-acme-001"}`), 0},
		{"401", http.StatusUnauthorized, "", http.StatusUnauthorized},
		{"403", http.StatusForbidden, match, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := workspaceAnswer(t, c.status, c.body)
			cfg := scopedSourceCfg(srv.URL, config.ScopeConfig{Name: testScopeName, ID: "ws-id-1"})
			_, err := asserterOf(t, cfg, deps).AssertScope(context.Background())
			if err == nil {
				t.Fatal("want an error, got a result")
			}
			if strings.Contains(err.Error(), cfg.Auth.Value) {
				t.Errorf("error %q carries the API key", err)
			}
			var ae *source.AuthError
			isAuth := errors.As(err, &ae)
			if c.authStatus != 0 {
				if !isAuth || ae.Status != c.authStatus || !errors.Is(err, source.ErrAuth) {
					t.Fatalf("want *source.AuthError{%d} (an ErrAuth), got %T %v", c.authStatus, err, err)
				}
			} else if isAuth || errors.Is(err, source.ErrAuth) {
				t.Fatalf("status %d must not be an auth error, got %v", c.status, err)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		srv := workspaceAnswer(t, http.StatusOK, match)
		srv.Close()
		if _, err := asserterOf(t, scopedSourceCfg(srv.URL, config.ScopeConfig{Name: testScopeName, ID: "ws-id-1"}), deps).AssertScope(context.Background()); err == nil {
			t.Fatal("an unreachable vendor must be an error")
		}
	})
	if len(authHooks) != 0 {
		t.Fatalf("the asserter called Deps.OnAuthError %v; the scheduler raises the signal", authHooks)
	}
}

// TestScopeRefusals (AC5): with a scope, New refuses settings.expected_workspace on either loop, an
// analytics max_backfill over six days and a groups window_span over six days, and a scope with no name.
// Six days exactly is accepted, and the same configs without a scope build as before (the legacy path
// is untouched).
func TestScopeRefusals(t *testing.T) {
	scope := config.ScopeConfig{Name: testScopeName, ID: "ws-id-1"}
	const sixDays = 6 * 24 * time.Hour
	mk := func(edit func(*config.SourceConfig)) config.SourceConfig {
		cfg := scopedSourceCfg("https://api.portkey.example/v1", scope)
		edit(&cfg)
		return cfg
	}
	setting := func(loop, k, v string) func(*config.SourceConfig) {
		return func(c *config.SourceConfig) {
			lc := c.Loops[loop]
			s := map[string]string{}
			for kk, vv := range lc.Settings {
				s[kk] = vv
			}
			s[k] = v
			lc.Settings = s
			c.Loops[loop] = lc
		}
	}
	maxBackfill := func(d time.Duration) func(*config.SourceConfig) {
		return func(c *config.SourceConfig) {
			lc := c.Loops["analytics"]
			lc.MaxBackfill = config.Duration(d)
			c.Loops["analytics"] = lc
		}
	}
	refused := []struct {
		name, mention string
		edit          func(*config.SourceConfig)
	}{
		{"expected_workspace on analytics", "expected_workspace", setting("analytics", "expected_workspace", testScopeName)},
		{"expected_workspace on groups", "expected_workspace", setting("groups", "expected_workspace", testScopeName)},
		{"empty expected_workspace on groups", "expected_workspace", setting("groups", "expected_workspace", "")},
		{"analytics max_backfill over six days", "max_backfill", maxBackfill(sixDays + time.Minute)},
		{"groups window_span over six days", "window_span", setting("groups", "window_span", (sixDays + time.Minute).String())},
		{"a scope id with no name", "scope", func(c *config.SourceConfig) { c.Scope = config.ScopeConfig{ID: "ws-id-1"} }},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			cfg := mk(c.edit)
			_, err := New(cfg, source.Deps{})
			if err == nil || !strings.Contains(err.Error(), c.mention) {
				t.Fatalf("want an error naming %s, got %v", c.mention, err)
			}
			if cfg.Scope.Name == "" {
				return
			}
			cfg.Scope = config.ScopeConfig{}
			if _, err := New(cfg, source.Deps{}); err != nil {
				t.Fatalf("the same config with no scope must build as before: %v", err)
			}
		})
	}
	for _, c := range []struct {
		name string
		edit func(*config.SourceConfig)
	}{
		{"analytics max_backfill of six days", maxBackfill(sixDays)},
		{"groups window_span of six days", setting("groups", "window_span", sixDays.String())},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(mk(c.edit), source.Deps{}); err != nil {
				t.Fatalf("six days is the longest lookback allowed with a scope, and allowed: %v", err)
			}
		})
	}
}

// otherSource is a source that is not Portkey's.
type otherSource struct{}

func (otherSource) ID() string           { return "other" }
func (otherSource) Loops() []source.Loop { return nil }

// TestSetSourceClockForTest: the hook sets the scope assertion's clock (its window ends at the injected
// now) and every loop's, on a scoped and an unscoped source, and refuses a source that is not Portkey's.
func TestSetSourceClockForTest(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 10, 30, 0, time.UTC)
	f := newURIRecorder(t, now.Add(-20*time.Minute))
	for _, scope := range []config.ScopeConfig{{Name: testScopeName, ID: "ws-id-1"}, {}} {
		src, err := New(scopedSourceCfg(f.URL, scope), source.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if !SetSourceClockForTest(src, func() time.Time { return now }) {
			t.Fatalf("scope %+v: SetSourceClockForTest refused a Portkey source", scope)
		}
		for _, lp := range src.Loops() {
			var got time.Time
			switch l := lp.(type) {
			case *analyticsLoop:
				got = l.now()
			case *groupsLoop:
				got = l.now()
			}
			if !got.Equal(now) {
				t.Errorf("scope %+v: loop %s clock = %s, want %s", scope, lp.Key().Loop, got, now)
			}
		}
		a, ok := src.(source.ScopeAsserter)
		if !ok {
			continue
		}
		f.take()
		if _, err := a.AssertScope(context.Background()); err != nil {
			t.Fatal(err)
		}
		uris := f.take()
		if len(uris) != 1 {
			t.Fatalf("assertion sent %q, want one request", uris)
		}
		u, _ := url.Parse(uris[0])
		if got := u.Query().Get("time_of_generation_max"); got != now.Format(time.RFC3339) {
			t.Errorf("assertion window ends %s, want the injected %s", got, now.Format(time.RFC3339))
		}
	}
	if SetSourceClockForTest(otherSource{}, time.Now) {
		t.Error("SetSourceClockForTest accepted a source that is not Portkey's")
	}
}
