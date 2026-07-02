// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	sessionsCode  int // 0 ⇒ 200
	statsCode     int // 0 ⇒ 200
	spans         int // run_count returned by /runs/stats
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
		case strings.HasSuffix(r.URL.Path, "/runs/stats"):
			b, _ := io.ReadAll(r.Body)
			f.statsBodies = append(f.statsBodies, string(b))
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
	lp.onGraphSkipped = func(loop, graph string) {
		if loop == "usage" && graph == "span_stats" {
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

// A span-call 429 backs off the WHOLE batch (quota).
func TestUsageCollectSpanCall429IsQuota(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	f.statsCode = http.StatusTooManyRequests
	lp := mkUsageLoop(t, f, now, nil)

	_, err := lp.Collect(context.Background(), model.Watermark{})
	if !errors.Is(err, source.ErrQuotaExceeded) {
		t.Fatalf("err=%v want ErrQuotaExceeded on span-call 429", err)
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
