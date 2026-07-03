// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	var skipped [][2]string
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"expected_workspace": "ws-acme-001", "page_size": "100"}),
		source.Deps{OnGraphSkipped: func(l, g string) { skipped = append(skipped, [2]string{l, g}) }}, now)
	batch, err := gl.Collect(context.Background(), model.Watermark{})
	if err == nil {
		t.Fatal("scope mismatch must refuse to emit (error)")
	}
	if len(batch.Samples) != 0 {
		t.Fatalf("must emit nothing on mismatch, got %d samples", len(batch.Samples))
	}
	if len(skipped) != 1 || skipped[0] != [2]string{"groups", "workspace_scope_mismatch"} {
		t.Fatalf("want OnGraphSkipped(groups, workspace_scope_mismatch), got %v", skipped)
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
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "analytics", "pk-test", now, func(string, string) { hookFired = true }, nil)
		if !ok || err != nil || hookFired {
			t.Fatalf("match: ok=%v err=%v hook=%v", ok, err, hookFired)
		}
	})
	t.Run("mismatch-fires-hook-and-errors", func(t *testing.T) {
		srv := wsServer(t, 200, "ws-global-admin")
		defer srv.Close()
		var gotLoop, gotGraph string
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "analytics", "pk-test", now, func(l, g string) { gotLoop, gotGraph = l, g }, nil)
		if ok || err == nil {
			t.Fatalf("mismatch must refuse: ok=%v err=%v", ok, err)
		}
		if gotLoop != "analytics" || gotGraph != "workspace_scope_mismatch" {
			t.Fatalf("hook args = %q/%q, want analytics/workspace_scope_mismatch", gotLoop, gotGraph)
		}
	})
	t.Run("undeterminable-proceeds", func(t *testing.T) {
		srv := wsServer(t, 200)
		defer srv.Close()
		var hookFired bool
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "groups", "pk-test", now, func(string, string) { hookFired = true }, nil)
		if ok || err != nil || hookFired {
			t.Fatalf("undeterminable: want (false,nil,no-hook), got ok=%v err=%v hook=%v", ok, err, hookFired)
		}
	})
	t.Run("transient-errors-no-hook", func(t *testing.T) {
		srv := wsServer(t, 503)
		defer srv.Close()
		var hookFired bool
		ok, err := verifyScopeForCollect(context.Background(), scopeTestClient(t), srv.URL, "h", "k", "ws-acme-001", "groups", "pk-test", now, func(string, string) { hookFired = true }, nil)
		if ok || err == nil || hookFired {
			t.Fatalf("transient: want (false,err,no-hook), got ok=%v err=%v hook=%v", ok, err, hookFired)
		}
	})
}
