// SPDX-License-Identifier: Apache-2.0

package langsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"golang.org/x/time/rate"
)

// fakeUsageServer serves /sessions (offset/limit over `all`) + /runs/stats (returns spansPerSession as
// run_count). Optional overrides let a test force a status on either endpoint. Records the raw request
// bodies/queries so the content-leak test can assert nothing content-y was ever sent.
type fakeUsageServer struct {
	srv           *httptest.Server
	sessionsCode  int         // 0 ⇒ 200
	statsCode     int         // 0 ⇒ 200
	statsCodeAt   map[int]int // 1-based runs/stats call number ⇒ forced status (beats statsCode)
	spans         int         // run_count returned by /runs/stats
	statsBodies   []string
	sessionsQuery []string
}

func newFakeUsageServer(t *testing.T, all []json.RawMessage) *fakeUsageServer {
	t.Helper()
	f := &fakeUsageServer{spans: 800}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs/stats"):
			b, _ := io.ReadAll(r.Body)
			f.statsBodies = append(f.statsBodies, string(b))
			if c, ok := f.statsCodeAt[len(f.statsBodies)]; ok {
				w.WriteHeader(c)
				return
			}
			if f.statsCode != 0 {
				w.WriteHeader(f.statsCode)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"run_count": f.spans})
		case strings.HasSuffix(r.URL.Path, "/sessions"):
			f.sessionsQuery = append(f.sessionsQuery, r.URL.RawQuery)
			if f.sessionsCode != 0 {
				w.WriteHeader(f.sessionsCode)
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
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func mkUsageLoop(t *testing.T, f *fakeUsageServer, now time.Time, mut func(*usageConfig)) *usageLoop {
	t.Helper()
	cfg := usageConfig{
		BaseURL: f.srv.URL, AuthHeader: "x-api-key", AuthValue: "k", SourceInstance: "ls-test",
		Prefix: "langsmith", Cadence: 10 * time.Minute, StatsWindow: 10 * time.Minute,
		SessionLabelKey: "session", SessionLabelValue: "id", PageLimit: 100, MaxSessions: 1000,
		EmitSpanCounts: true,
	}
	if mut != nil {
		mut(&cfg)
	}
	hc := httpx.New(httpx.Config{UserAgent: "t", Timeout: 5 * time.Second, AllowPrivate: true,
		Limiter: rate.NewLimiter(1000, 1000)})
	lp, err := newUsageLoop(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	lp.now = func() time.Time { return now }
	return lp
}

// two projects, one longlived (traces=100) one shortlived (traces=5).
func usageFixture() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"id":"p1","name":"proj-a","run_count":100,"trace_tier":"longlived"}`),
		json.RawMessage(`{"id":"p2","name":"proj-b","run_count":5,"trace_tier":"shortlived"}`),
	}
}

func byNameLabel(samples []model.Sample) map[string]model.Sample {
	m := map[string]model.Sample{}
	for _, s := range samples {
		m[s.Name+"|"+s.Labels["session"]] = s
	}
	return m
}

func TestUsageCollectTracesSpansTier(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	lp := mkUsageLoop(t, f, now, nil)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	m := byNameLabel(b.Samples)
	if got := m["langsmith_usage_traces|p1"]; got.Value != 100 || got.Labels["retention_tier"] != "longlived" {
		t.Fatalf("p1 traces=%+v want 100/longlived", got)
	}
	if got := m["langsmith_usage_traces|p2"]; got.Value != 5 || got.Labels["retention_tier"] != "shortlived" {
		t.Fatalf("p2 traces=%+v want 5/shortlived", got)
	}
	if got := m["langsmith_usage_spans|p1"]; got.Value != 800 || got.Labels["retention_tier"] != "longlived" {
		t.Fatalf("p1 spans=%+v want 800/longlived", got)
	}
	if !b.Watermark.Time.Equal(now) {
		t.Fatalf("watermark=%v want now=%v (forward-only liveness)", b.Watermark.Time, now)
	}
}

func TestUsageSpanCallsShareEndTime(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 15, 30, 0, time.FixedZone("UTC+2", 2*60*60))
	f := newFakeUsageServer(t, usageFixture())
	lp := mkUsageLoop(t, f, now, func(c *usageConfig) { c.StatsWindow = 7 * time.Minute })
	// A fresh clock read during fan-out would give a different bound.
	next := now
	lp.now = func() time.Time {
		current := next
		next = next.Add(time.Second)
		return current
	}

	if _, err := lp.Collect(context.Background(), model.Watermark{}); err != nil {
		t.Fatal(err)
	}
	projects := map[string]bool{"p1": true, "p2": true}
	if got := len(f.statsBodies); got != len(projects) {
		t.Fatalf("runs/stats calls = %d, want %d", got, len(projects))
	}
	for _, body := range f.statsBodies {
		var req struct {
			Session   []string `json:"session"`
			StartTime string   `json:"start_time"`
			EndTime   string   `json:"end_time"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Session) != 1 || !projects[req.Session[0]] {
			t.Fatalf("unexpected project scope %v", req.Session)
		}
		delete(projects, req.Session[0])
		if want := "2026-07-02T12:08:30Z"; req.StartTime != want {
			t.Errorf("project %s: start_time = %q, want %q", req.Session[0], req.StartTime, want)
		}
		if want := "2026-07-02T12:15:30Z"; req.EndTime != want {
			t.Errorf("project %s: end_time = %q, want %q", req.Session[0], req.EndTime, want)
		}
	}
}

func TestUsageCollectSpansOff(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	lp := mkUsageLoop(t, f, now, func(c *usageConfig) { c.EmitSpanCounts = false })

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range b.Samples {
		if strings.HasSuffix(s.Name, "_usage_spans") {
			t.Fatalf("spans emitted with emit_span_counts=false: %+v", s)
		}
	}
	if len(f.statsBodies) != 0 {
		t.Fatalf("runs/stats called %d times with spans off; want 0", len(f.statsBodies))
	}
}

func TestUsageCollectSessions429IsQuota(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	f.sessionsCode = http.StatusTooManyRequests
	lp := mkUsageLoop(t, f, now, nil)

	_, err := lp.Collect(context.Background(), model.Watermark{})
	if !errors.Is(err, source.ErrQuotaExceeded) {
		t.Fatalf("err=%v want ErrQuotaExceeded", err)
	}
}

func TestUsageCollectSessions401FiresAuthError(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	f.sessionsCode = http.StatusForbidden
	lp := mkUsageLoop(t, f, now, nil)
	var authLoop string
	lp.onAuthError = func(loop, src string) { authLoop = loop }

	if _, err := lp.Collect(context.Background(), model.Watermark{}); err == nil {
		t.Fatal("want error on 403")
	}
	if authLoop != "usage" {
		t.Fatalf("onAuthError loop=%q want usage", authLoop)
	}
}

// A span-call failure (non-429) must NOT sink the batch: traces still emit, the skip is counted, no spans.
func TestUsageCollectSpanCallSkipKeepsTraces(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	f.statsCode = http.StatusInternalServerError
	lp := mkUsageLoop(t, f, now, nil)
	var skips int
	lp.onDataIncomplete = func(loop, _ string, reason source.IncompleteReason) {
		if loop == "usage" && reason == source.IncompleteSpanStatsUnavailable {
			skips++
		}
	}

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("span-call failure must not error the batch: %v", err)
	}
	for _, s := range b.Samples {
		if strings.HasSuffix(s.Name, "_usage_spans") {
			t.Fatalf("spans emitted despite span-call failure: %+v", s)
		}
	}
	traces := 0
	for _, s := range b.Samples {
		if strings.HasSuffix(s.Name, "_usage_traces") {
			traces++
		}
	}
	if traces != 2 {
		t.Fatalf("traces=%d want 2 (still emitted despite span skip)", traces)
	}
	if skips != 2 {
		t.Fatalf("span_stats skips=%d want 2 (one per project, counted)", skips)
	}
}

// A /sessions page 429 still backs off the WHOLE batch (quota); only the per-project span fan-out keeps
// partial progress (TestUsageSpanFanout429KeepsProgress).
func TestUsageCollectSessionsPage429IsQuota(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	f.sessionsCode = http.StatusTooManyRequests
	lp := mkUsageLoop(t, f, now, nil)

	_, err := lp.Collect(context.Background(), model.Watermark{})
	if !errors.Is(err, source.ErrQuotaExceeded) {
		t.Fatalf("err=%v want ErrQuotaExceeded on a /sessions 429", err)
	}
}

// Content-safety: neither the sessions query nor the runs/stats body ever names a content field, and the
// output carries only counts + the enum tier + the session dimension.
func TestUsageCollectNeverLeaksContent(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	lp := mkUsageLoop(t, f, now, nil)

	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"inputs", "outputs", "messages", "select", "request", "response", "metadata"}
	for _, q := range f.sessionsQuery {
		for _, w := range banned {
			if strings.Contains(q, w) {
				t.Fatalf("sessions query %q contains banned token %q", q, w)
			}
		}
	}
	for _, body := range f.statsBodies {
		for _, w := range banned {
			if strings.Contains(body, w) {
				t.Fatalf("runs/stats body %q contains banned token %q", body, w)
			}
		}
	}
	allowedLabels := map[string]bool{"session": true, "retention_tier": true}
	for _, s := range b.Samples {
		for k := range s.Labels {
			if !allowedLabels[k] {
				t.Fatalf("unexpected label %q on %s (only session/retention_tier allowed)", k, s.Name)
			}
		}
	}
}

func TestUsageMaxSessionsTruncationCounted(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		max  int
		want []string
	}{
		{"capped", 1, []string{"usage:sessions_truncated"}},
		{"below cap", 1000, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUsageServer(t, usageFixture())
			lp := mkUsageLoop(t, f, now, func(c *usageConfig) { c.MaxSessions = tc.max; c.EmitSpanCounts = false })
			var events []string
			lp.onDataIncomplete = func(loop, _ string, r source.IncompleteReason) { events = append(events, loop+":"+string(r)) }
			if _, err := lp.Collect(context.Background(), model.Watermark{}); err != nil {
				t.Fatal(err)
			}
			if len(events) != len(tc.want) || (len(tc.want) == 1 && events[0] != tc.want[0]) {
				t.Fatalf("signals = %v, want %v", events, tc.want)
			}
		})
	}
}

func manyProjects(n int) []json.RawMessage {
	all := make([]json.RawMessage, 0, n)
	for i := 0; i < n; i++ {
		all = append(all, json.RawMessage(fmt.Sprintf(`{"id":"p%03d","run_count":1,"trace_tier":"shortlived"}`, i)))
	}
	return all
}

func countSuffix(samples []model.Sample, suffix string) int {
	n := 0
	for _, s := range samples {
		if strings.HasSuffix(s.Name, suffix) {
			n++
		}
	}
	return n
}

// captureSlog swaps the default logger for a buffer for the test and returns the buffer.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// With runs/stats failing for every project the fan-out stops after spanFanoutBreaker consecutive
// failures, the traces still emit, the abandoned remainder is counted once and ONE warning is logged.
func TestUsageSpanFanoutBreaker(t *testing.T) {
	const projects = 300
	logs := captureSlog(t)
	f := newFakeUsageServer(t, manyProjects(projects))
	f.statsCode = http.StatusServiceUnavailable
	lp := mkUsageLoop(t, f, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), nil)
	var events int
	lp.onDataIncomplete = func(loop, _ string, r source.IncompleteReason) {
		if loop == "usage" && r == source.IncompleteSpanStatsUnavailable {
			events++
		}
	}
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if n := len(f.statsBodies); n > spanFanoutBreaker {
		t.Fatalf("runs/stats failed for every project yet Collect issued %d span calls (breaker %d)", n, spanFanoutBreaker)
	}
	if got := countSuffix(b.Samples, "_usage_traces"); got != projects {
		t.Fatalf("usage_traces=%d want %d (traces must still emit)", got, projects)
	}
	if got := countSuffix(b.Samples, "_usage_spans"); got != 0 {
		t.Fatalf("usage_spans=%d want 0", got)
	}
	if want := spanFanoutBreaker + 1; events != want { // each failed call + one for the abandoned remainder
		t.Fatalf("span_stats_unavailable fired %d times, want %d", events, want)
	}
	if n := strings.Count(logs.String(), "span counts skipped"); n != 1 {
		t.Fatalf("want one warning per Collect, got %d:\n%s", n, logs.String())
	}
}

// A failure that is followed by a success resets the count: isolated failures never trip the breaker.
func TestUsageSpanFanoutBreakerResetsOnSuccess(t *testing.T) {
	const projects = 10
	f := newFakeUsageServer(t, manyProjects(projects))
	f.statsCodeAt = map[int]int{}
	for _, call := range []int{1, 2, 3, 4, 6, 7, 8, 9} { // 4 failures, a success, 4 failures, a success
		f.statsCodeAt[call] = http.StatusInternalServerError
	}
	lp := mkUsageLoop(t, f, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), nil)
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.statsBodies) != projects {
		t.Fatalf("span calls=%d want %d: the breaker tripped on non-consecutive failures", len(f.statsBodies), projects)
	}
	if got := countSuffix(b.Samples, "_usage_spans"); got != 2 {
		t.Fatalf("usage_spans=%d want 2", got)
	}
}

// A 429 on the Nth span call keeps what was fetched: every project's trace gauge plus spans for the
// first N-1, with no error and one signal for the abandoned remainder.
func TestUsageSpanFanout429KeepsProgress(t *testing.T) {
	const projects, nth = 8, 4
	f := newFakeUsageServer(t, manyProjects(projects))
	f.statsCodeAt = map[int]int{nth: http.StatusTooManyRequests}
	lp := mkUsageLoop(t, f, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), nil)
	var events int
	lp.onDataIncomplete = func(loop, _ string, r source.IncompleteReason) {
		if loop == "usage" && r == source.IncompleteSpanStatsUnavailable {
			events++
		}
	}
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("a mid-fan-out 429 must not error the batch: %v", err)
	}
	if len(f.statsBodies) != nth {
		t.Fatalf("span calls=%d want %d (stop after the 429)", len(f.statsBodies), nth)
	}
	if got := countSuffix(b.Samples, "_usage_traces"); got != projects {
		t.Fatalf("usage_traces=%d want %d", got, projects)
	}
	if got := countSuffix(b.Samples, "_usage_spans"); got != nth-1 {
		t.Fatalf("usage_spans=%d want %d", got, nth-1)
	}
	if events != 1 {
		t.Fatalf("span_stats_unavailable fired %d times, want 1", events)
	}
}
