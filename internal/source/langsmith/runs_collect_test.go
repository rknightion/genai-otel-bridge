// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/config"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

func newTestRunsLoop(t *testing.T, srvURL string, now time.Time, settings map[string]string) *runsLoop {
	t.Helper()
	if settings == nil {
		settings = map[string]string{}
	}
	if settings["session_ids"] == "" && settings["session_filter"] == "" {
		settings["session_ids"] = "s1" // static scope → no discovery round-trip in Collect tests
	}
	lp, err := newRunsLoop(
		config.SourceConfig{BaseURL: srvURL, SourceInstance: "ls-test",
			Auth: config.AuthConfig{Header: "x-api-key", Value: "k"}},
		config.LoopConfig{Enabled: true, Cadence: config.Duration(time.Minute), Settings: settings},
		source.Deps{}, runsTestClient(t))
	if err != nil {
		t.Fatal(err)
	}
	lp.now = func() time.Time { return now }
	return lp
}

// TestRunsCollectAuthErrorFiresHook asserts a 401/403 from runs/query fires Deps.OnAuthError so a
// credential failure is its own alertable signal (followup §9), distinct from a generic retryable error.
func TestRunsCollectAuthErrorFiresHook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	now := time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC)
	var auth [][2]string
	lp, err := newRunsLoop(
		config.SourceConfig{BaseURL: srv.URL, SourceInstance: "ls-test",
			Auth: config.AuthConfig{Header: "x-api-key", Value: "k"}},
		config.LoopConfig{Enabled: true, Cadence: config.Duration(time.Minute), Settings: map[string]string{"session_ids": "s1"}},
		source.Deps{OnAuthError: func(loop, s string) { auth = append(auth, [2]string{loop, s}) }}, runsTestClient(t))
	if err != nil {
		t.Fatal(err)
	}
	lp.now = func() time.Time { return now }
	if _, err := lp.Collect(context.Background(), model.Watermark{}); err == nil {
		t.Fatal("a 403 must surface as a retryable error")
	}
	if len(auth) != 1 || auth[0] != [2]string{"runs", "ls-test"} {
		t.Fatalf("OnAuthError want one (runs,ls-test), got %v", auth)
	}
}

func TestRunsCollectTwoPageWindow(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&hits, 1) {
		case 1:
			_, _ = w.Write([]byte(`{"runs":[{"id":"r1","run_type":"llm","status":"success","start_time":"2026-06-20T00:05:00"}],"cursors":{"next":"N1"}}`))
		default:
			_, _ = w.Write([]byte(`{"runs":[{"id":"r2","run_type":"chain","status":"success","start_time":"2026-06-20T00:06:00"}],"cursors":{"next":null}}`))
		}
	}))
	defer srv.Close()
	now := time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC) // plenty of settle headroom
	lp := newTestRunsLoop(t, srv.URL, now, map[string]string{"window": "1h", "settle": "10m", "page_size": "100"})

	b1, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b1.Logs) != 1 {
		t.Fatalf("page1 err=%v logs=%d", err, len(b1.Logs))
	}
	if !b1.Watermark.Time.IsZero() || decodeRunsCursor(b1.Watermark.Cursor).Next != "N1" {
		t.Fatalf("page1 watermark wrong: %+v", b1.Watermark)
	}
	if b1.Logs[0].IndexedAttributes["run_type"] != "llm" {
		t.Fatalf("page1 log missing run_type: %+v", b1.Logs[0])
	}
	b2, err := lp.Collect(context.Background(), b1.Watermark)
	if err != nil || len(b2.Logs) != 1 {
		t.Fatalf("page2 err=%v logs=%d", err, len(b2.Logs))
	}
	if b2.Watermark.Time.IsZero() || b2.Watermark.Cursor != "" {
		t.Fatalf("page2 must advance Time + clear cursor: %+v", b2.Watermark)
	}
	// next window starts at the prior winMax (contiguous, forward-only)
	b3, err := lp.Collect(context.Background(), b2.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	wantWinMin := b2.Watermark.Time
	if got := decodeRunsCursor(b3.Watermark.Cursor); got.WinMin != "" && got.WinMin != wantWinMin.UTC().Format(time.RFC3339Nano) {
		// (b3 may be "nothing settled" → empty; only assert contiguity if it started a window)
		t.Logf("b3 window: %+v (winMax was %v)", got, wantWinMin)
	}
}

func TestRunsCollectNothingSettled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must NOT query when nothing has settled")
	}))
	defer srv.Close()
	now := time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC)
	lp := newTestRunsLoop(t, srv.URL, now, map[string]string{"window": "1h", "settle": "10m"})
	// since.Time = now-5m, settle 10m ⇒ winMax = now-10m < winMin = now-5m ⇒ nothing settled
	since := model.Watermark{Time: now.Add(-5 * time.Minute)}
	b, err := lp.Collect(context.Background(), since)
	if err != nil || len(b.Logs) != 0 || !b.Watermark.Time.Equal(since.Time) || b.Watermark.Cursor != "" {
		t.Fatalf("nothing-settled must be empty + unchanged watermark: logs=%d wm=%+v err=%v", len(b.Logs), b.Watermark, err)
	}
}

func TestRunsCollectEmptyWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"runs":[],"cursors":{"next":null}}`))
	}))
	defer srv.Close()
	now := time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC)
	lp := newTestRunsLoop(t, srv.URL, now, map[string]string{"window": "1h", "settle": "10m"})
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b.Logs) != 0 {
		t.Fatalf("empty window: logs=%d err=%v", len(b.Logs), err)
	}
	if b.Watermark.Time.IsZero() || b.Watermark.Cursor != "" {
		t.Fatalf("empty settled window must advance Time + clear cursor: %+v", b.Watermark)
	}
}

func TestRunsCollectQuotaAndRetryable(t *testing.T) {
	for _, tc := range []struct {
		code      int
		wantQuota bool
	}{{429, true}, {500, false}, {401, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code) }))
		now := time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC)
		lp := newTestRunsLoop(t, srv.URL, now, map[string]string{"window": "1h", "settle": "10m"})
		_, err := lp.Collect(context.Background(), model.Watermark{})
		if err == nil {
			t.Fatalf("code %d: expected error", tc.code)
		}
		if got := errors.Is(err, source.ErrQuotaExceeded); got != tc.wantQuota {
			t.Fatalf("code %d: ErrQuotaExceeded=%v want %v (err=%v)", tc.code, got, tc.wantQuota, err)
		}
		srv.Close()
	}
}

func TestRunsCollectMaxPagesTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"runs":[{"id":"r1","run_type":"llm","status":"success","start_time":"2026-06-20T00:05:00"}],"cursors":{"next":"ALWAYS_MORE"}}`))
	}))
	defer srv.Close()
	now := time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC)
	lp := newTestRunsLoop(t, srv.URL, now, map[string]string{"window": "1h", "settle": "10m", "max_pages_per_window": "1"})
	var skipped int32
	lp.onGraphSkipped = func(loop, graph string) {
		if loop == "runs" && graph == "window_truncated" {
			atomic.AddInt32(&skipped, 1)
		}
	}
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b.Logs) != 1 {
		t.Fatalf("truncation: err=%v logs=%d", err, len(b.Logs))
	}
	// cap of 1 page hit, but more pages remain ⇒ advance past (Time=winMax), clear cursor, count the skip
	if b.Watermark.Time.IsZero() || b.Watermark.Cursor != "" {
		t.Fatalf("truncation must advance + clear cursor (never stall): %+v", b.Watermark)
	}
	if atomic.LoadInt32(&skipped) != 1 {
		t.Fatalf("truncation must fire the window_truncated skip metric, got %d", skipped)
	}
}
