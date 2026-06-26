// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// TestRunsDiscoveryFrozenWhileDraining_H1 [adversarial-review H1]: under auto-discovery, the resolved
// session scope MUST stay frozen for a window's pages. A TTL refresh mid-window would query later pages
// against a different project set than page 1 (the opaque cursor is bound to page 1's scope) → silent
// within-window gaps/dups. So a DRAINING Collect must reuse the cache regardless of the refresh TTL.
func TestRunsDiscoveryFrozenWhileDraining_H1(t *testing.T) {
	var sessHits, runHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sessions":
			atomic.AddInt32(&sessHits, 1)
			if off := r.URL.Query().Get("offset"); off == "" || off == "0" {
				_, _ = w.Write([]byte(`[{"id":"p1"},{"id":"p2"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case "/runs/query":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			atomic.AddInt32(&runHits, 1)
			if _, hasCursor := body["cursor"]; hasCursor {
				_, _ = w.Write([]byte(`{"runs":[{"id":"r2","run_type":"llm","status":"success","start_time":"2026-06-20T00:06:00"}],"cursors":{"next":null}}`))
			} else {
				_, _ = w.Write([]byte(`{"runs":[{"id":"r1","run_type":"llm","status":"success","start_time":"2026-06-20T00:05:00"}],"cursors":{"next":"N1"}}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t0 := time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC)
	lp := newTestRunsLoop(t, srv.URL, t0, map[string]string{
		"session_filter": `eq(name,"x")`, "session_refresh": "1ms", "window": "1h", "settle": "10m",
	})
	// page 1 (idle): discovers once + queries
	b1, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || decodeRunsCursor(b1.Watermark.Cursor).Next != "N1" {
		t.Fatalf("page1: err=%v wm=%+v", err, b1.Watermark)
	}
	if got := atomic.LoadInt32(&sessHits); got != 1 {
		t.Fatalf("page1 expected exactly 1 discovery, got %d", got)
	}
	// advance now WELL PAST session_refresh, then drain page 2: discovery MUST NOT re-run mid-window.
	lp.now = func() time.Time { return t0.Add(time.Hour) }
	b2, err := lp.Collect(context.Background(), b1.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Watermark.Cursor != "" {
		t.Fatalf("page2 should complete the window: %+v", b2.Watermark)
	}
	if got := atomic.LoadInt32(&sessHits); got != 1 {
		t.Fatalf("[H1] discovery re-ran mid-window (scope churn): %d /sessions hits, want 1", got)
	}
}

// TestSessionDiscoveryTruncationCounted_M1 [adversarial-review M1]: a max_sessions truncation must be
// COUNTED (OnGraphSkipped → genai_otel_bridge_source_graph_unavailable_total), not merely logged — otherwise a
// project population growing past the cap silently stops being pulled with no alert.
func TestSessionDiscoveryTruncationCounted_M1(t *testing.T) {
	var hits int32
	srv := fakeSessions(t, 5, &hits)
	var skipped int32
	lp := &runsLoop{
		baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		sessionFilter: `eq(name,"x")`, maxSessions: 2, sessionRefresh: time.Hour,
		onGraphSkipped: func(loop, graph string) {
			if loop == "runs" && graph == "sessions_truncated" {
				atomic.AddInt32(&skipped, 1)
			}
		},
	}
	got, err := lp.resolveSessions(context.Background(), time.Unix(0, 0).UTC(), false)
	if err != nil || len(got) != 2 {
		t.Fatalf("cap: got %v err=%v", got, err)
	}
	if atomic.LoadInt32(&skipped) != 1 {
		t.Fatalf("[M1] session-cap truncation must fire the sessions_truncated counter, got %d", skipped)
	}
}
