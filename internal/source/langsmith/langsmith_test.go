// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

func loadFixture(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		t.Fatal(err)
	}
	return arr
}

// fakeLangSmith serves /sessions with offset/limit pagination over `all`, requiring x-api-key auth.
func fakeLangSmith(t *testing.T, all []json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if lim <= 0 {
			lim = 100
		}
		if off > len(all) {
			off = len(all)
		}
		end := off + lim
		if end > len(all) {
			end = len(all)
		}
		_ = json.NewEncoder(w).Encode(all[off:end])
	}))
}

func testConfig(srv *httptest.Server) Config {
	return Config{
		BaseURL: srv.URL, AuthHeader: "x-api-key", AuthValue: "k", SourceInstance: "ls-test",
		Prefix: "langsmith", Cadence: time.Minute, RPS: 1000, Burst: 1000, AllowPrivate: true,
		StatsWindow: time.Hour, SessionLabelKey: "session", SessionLabelValue: "id",
		PageLimit: 100, MaxSessions: 1000, EmitFeedback: true,
	}
}

func mkLoop(t *testing.T, cfg Config, now time.Time) *sessionsLoop {
	t.Helper()
	src, err := newSource(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	lp := src.Loops()[0].(*sessionsLoop)
	lp.now = func() time.Time { return now }
	return lp
}

func TestCollectDerivesStampsAtNow(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	srv := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer srv.Close()
	lp := mkLoop(t, testConfig(srv), now)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Samples) == 0 {
		t.Fatal("no samples derived from fixture")
	}
	if !b.Watermark.Time.Equal(now) {
		t.Fatalf("watermark=%v want now=%v (rolling-snapshot liveness cursor)", b.Watermark.Time, now)
	}
	for _, s := range b.Samples {
		if !s.Timestamp.Equal(now) {
			t.Fatalf("sample %q stamped %v, want now", s.Name, s.Timestamp)
		}
	}
	found := false
	for _, s := range b.Samples {
		if s.Name == "langsmith_runs" && s.Labels["session"] == "11111111-1111-1111-1111-111111111111" && s.Value == 450 {
			found = true
		}
	}
	if !found {
		t.Fatal("missing langsmith_runs=450 for session 1")
	}
}

// TestCollectStampsAtMinuteResolution guards the snapshot 1DPM fix (followup §8 review-M3): the sessions
// loop is a per-poll snapshot, so two polls landing in the same wall-clock minute (sub-minute cadence, or
// the ~60s+jitter edge) must NOT emit two distinct sub-minute timestamps — that pushes >1 point/series/
// minute past Mimir (CoalesceDPM is per-batch, can't dedup across polls). Sample timestamps are truncated
// to the minute (so same-minute re-polls share a ts → LWW dedup → exactly 1DPM); the watermark liveness
// cursor stays the precise `now`. Mirrors the portkey groups loop.
func TestCollectStampsAtMinuteResolution(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 37, 500_000_000, time.UTC) // sub-minute: 37.5s past the minute
	srv := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer srv.Close()
	lp := mkLoop(t, testConfig(srv), now)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Samples) == 0 {
		t.Fatal("no samples derived from fixture")
	}
	wantStamp := now.Truncate(time.Minute) // 12:00:00
	for _, s := range b.Samples {
		if !s.Timestamp.Equal(wantStamp) {
			t.Fatalf("sample %q stamped %v, want minute-truncated %v (1DPM)", s.Name, s.Timestamp, wantStamp)
		}
	}
	if !b.Watermark.Time.Equal(now) {
		t.Fatalf("watermark=%v want precise now=%v (liveness cursor stays un-truncated)", b.Watermark.Time, now)
	}
}

func runSampleValue(samples []model.Sample, name, sessionLabel string) (float64, bool) {
	for _, s := range samples {
		if s.Name == name && s.Labels["session"] == sessionLabel {
			return s.Value, true
		}
	}
	return 0, false
}

func TestCollectPaginates(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	srv := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer srv.Close()
	cfg := testConfig(srv)
	cfg.PageLimit = 1 // force multi-page walk (one session per page)
	lp := mkLoop(t, cfg, now)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := runSampleValue(b.Samples, "langsmith_runs", "11111111-1111-1111-1111-111111111111"); !ok || v != 450 {
		t.Fatalf("session 1 runs across pages: %v ok=%v", v, ok)
	}
	if v, ok := runSampleValue(b.Samples, "langsmith_runs", "22222222-2222-2222-2222-222222222222"); !ok || v != 60 {
		t.Fatalf("session 2 runs across pages: %v ok=%v", v, ok)
	}
}

func TestCollectMaxSessionsCapBounds(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	srv := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer srv.Close()
	cfg := testConfig(srv)
	cfg.MaxSessions = 1 // cap below the population
	lp := mkLoop(t, cfg, now)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	// server orders by start_time desc ⇒ session 1 first; cap=1 ⇒ only session 1 emitted.
	if _, ok := runSampleValue(b.Samples, "langsmith_runs", "22222222-2222-2222-2222-222222222222"); ok {
		t.Fatal("session 2 emitted despite MaxSessions=1 cap")
	}
	if _, ok := runSampleValue(b.Samples, "langsmith_runs", "11111111-1111-1111-1111-111111111111"); !ok {
		t.Fatal("session 1 missing under MaxSessions=1")
	}
}

// A server that ignores `offset` (returns the full list every page) must NOT cause Collect to loop
// forever — the no-progress guard terminates it, emitting each session exactly once.
func TestCollectTerminatesOnOffsetIgnoringServer(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	fixture := loadFixture(t, "sessions_with_stats.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fixture) // ignores offset/limit — always the full list
	}))
	defer srv.Close()
	cfg := testConfig(srv)
	cfg.PageLimit = 1 // would loop forever without the no-progress break
	lp := mkLoop(t, cfg, now)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range b.Samples {
		if s.Name == "langsmith_runs" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("langsmith_runs count=%d want 2 (each session once, deduped)", n)
	}
}

func fakeStatus(t *testing.T, code int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
}

func TestCollectErrorTaxonomy(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	t.Run("429_is_quota_exceeded", func(t *testing.T) {
		srv := fakeStatus(t, http.StatusTooManyRequests)
		defer srv.Close()
		lp := mkLoop(t, testConfig(srv), now)
		_, err := lp.Collect(context.Background(), model.Watermark{})
		if err != source.ErrQuotaExceeded {
			t.Fatalf("429 ⇒ want ErrQuotaExceeded, got %v", err)
		}
	})
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run("retryable_"+strconv.Itoa(code), func(t *testing.T) {
			srv := fakeStatus(t, code)
			defer srv.Close()
			lp := mkLoop(t, testConfig(srv), now)
			_, err := lp.Collect(context.Background(), model.Watermark{})
			if err == nil || err == source.ErrQuotaExceeded {
				t.Fatalf("status %d ⇒ want retryable non-quota error, got %v", code, err)
			}
		})
	}
}

// TestSessionsAuthErrorFiresHook asserts a 401/403 from the sessions endpoint fires Deps.OnAuthError so
// a credential failure is its own alertable signal (followup §9), distinct from a generic retryable error.
func TestSessionsAuthErrorFiresHook(t *testing.T) {
	srv := fakeStatus(t, http.StatusUnauthorized)
	defer srv.Close()
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	var auth [][2]string
	src, err := newSource(testConfig(srv), source.Deps{OnAuthError: func(loop, s string) { auth = append(auth, [2]string{loop, s}) }})
	if err != nil {
		t.Fatal(err)
	}
	lp := src.Loops()[0].(*sessionsLoop)
	lp.now = func() time.Time { return now }
	if _, err := lp.Collect(context.Background(), model.Watermark{}); err == nil {
		t.Fatal("a 401 must surface as a retryable error")
	}
	if len(auth) != 1 || auth[0] != [2]string{"sessions", "ls-test"} {
		t.Fatalf("OnAuthError want one (sessions,ls-test), got %v", auth)
	}
}

// Request side: the query must never select run content fields. Response side: feedback `values`
// (raw ids) must never surface in any emitted label. Both are the no-content release gate.
func TestCollectNeverLeaksContent(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	fixture := loadFixture(t, "sessions_with_stats.json")
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery += r.URL.RawQuery + "&"
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if off >= len(fixture) {
			_ = json.NewEncoder(w).Encode([]json.RawMessage{})
			return
		}
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	defer srv.Close()
	cfg := testConfig(srv)
	cfg.PageLimit = 100
	lp := mkLoop(t, cfg, now)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"inputs", "outputs", "input", "output", "events", "body"} {
		if strings.Contains(sawQuery, bad) {
			t.Fatalf("content selector %q in query %q", bad, sawQuery)
		}
	}
	for _, s := range b.Samples {
		for k, v := range s.Labels {
			for _, leak := range []string{"fake-trace", "fake-user", "fake-req"} {
				if strings.Contains(v, leak) {
					t.Fatalf("feedback `values` id leaked into label %s=%q", k, v)
				}
			}
		}
	}
}

func TestSetLoopClockForTest(t *testing.T) {
	srv := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer srv.Close()
	src, err := newSource(testConfig(srv), source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	lp := src.Loops()[0]
	fixed := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if !SetLoopClockForTest(lp, func() time.Time { return fixed }) {
		t.Fatal("want true applying clock to a langsmith loop")
	}
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Watermark.Time.Equal(fixed) {
		t.Fatalf("injected clock not used: watermark=%v want %v", b.Watermark.Time, fixed)
	}
	if SetLoopClockForTest(nil, func() time.Time { return fixed }) {
		t.Fatal("want false for a non-langsmith loop")
	}
}

func TestNewFromSourceConfig(t *testing.T) {
	srv := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer srv.Close()
	sc := config.SourceConfig{
		Type: "langsmith", Enabled: true, BaseURL: srv.URL, SourceInstance: "ls-prod",
		Auth:      config.AuthConfig{Header: "x-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 3},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"sessions": {
			Enabled: true, Cadence: config.Duration(time.Minute), MetricPrefix: "langsmith",
		}},
	}
	src, err := New(sc, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Loops()) != 1 {
		t.Fatalf("loops=%d want 1", len(src.Loops()))
	}
	if src.ID() != "langsmith" {
		t.Fatalf("id=%q", src.ID())
	}

	// Disabled loop ⇒ empty source (no loops).
	sc.Loops["sessions"] = config.LoopConfig{Enabled: false}
	src2, err := New(sc, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src2.Loops()) != 0 {
		t.Fatalf("disabled loop should yield 0 loops, got %d", len(src2.Loops()))
	}
}

// TestNewRejectsStrayWindowOnSnapshotLoops (#56): the sessions/usage loops are aggregate-now snapshots,
// so LoopConfig.Window MUST be 0. A stray non-zero window is silently ignored by the loop but flips the
// scheduler into windowed semantics (via app.go's LoopSpec.Window copy), firing false
// backfill_unstorable alarms every tick. New must reject it fast, pointing at settings.stats_window.
func TestNewRejectsStrayWindowOnSnapshotLoops(t *testing.T) {
	base := config.SourceConfig{
		Type: "langsmith", Enabled: true, BaseURL: "https://api.smith.langchain.com/api/v1",
		SourceInstance: "ls", Auth: config.AuthConfig{Header: "x-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 1},
	}
	for _, loop := range []string{"sessions", "usage"} {
		t.Run(loop, func(t *testing.T) {
			sc := base
			sc.Loops = map[string]config.LoopConfig{loop: {
				Enabled: true, Cadence: config.Duration(time.Minute),
				Window: config.Duration(50 * time.Minute), // stray — must be rejected
			}}
			_, err := New(sc, source.Deps{})
			if err == nil {
				t.Fatalf("New must reject a non-zero window on the %s snapshot loop", loop)
			}
			if !strings.Contains(err.Error(), "stats_window") {
				t.Fatalf("error should point at settings.stats_window, got %v", err)
			}
		})
	}
}
