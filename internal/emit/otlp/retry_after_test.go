// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
)

// fakeClock drives post()'s retry loop deterministically: now() reads a virtual time and sleep()
// records the requested delay and advances the virtual clock by it (no real wall-clock wait). Every
// test here installs one so Retry-After behaviour is provable without seconds-long sleeps.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleepFn(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	return nil
}

func (c *fakeClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// emitterWithClock builds an emitter whose retry budget is generous (so short Retry-After waits fit)
// and whose computed backoff is tiny (so Retry-After, when honoured, clearly dominates). Jitter is 0
// for determinism.
func emitterWithClock(t *testing.T, url string, clk *fakeClock) *Emitter {
	t.Helper()
	em := New(Config{
		Endpoint: url, InstanceID: "123", Token: "secret-token",
		Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20,
		Retry: RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 5 * time.Minute, Jitter: 0},
	})
	em.now = clk.now
	em.sleep = clk.sleepFn
	return em
}

// TestEmitHonorsRetryAfterSeconds [#122 acceptance #1]: a 429 with `Retry-After: N` (seconds) must
// wait at least N before the next attempt, even though the computed backoff (1ms) is far smaller —
// proving max(backoff, Retry-After) is used. Then the retry succeeds.
func TestEmitHonorsRetryAfterSeconds(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	if err := emitterWithClock(t, srv.URL, clk).Emit(context.Background(), oneBatch()); err != nil {
		t.Fatalf("expected success after honouring Retry-After: %v", err)
	}
	sl := clk.sleeps()
	if len(sl) != 1 {
		t.Fatalf("expected exactly one backoff sleep, got %v", sl)
	}
	if sl[0] < 2*time.Second {
		t.Fatalf("slept %v before retry, want >= 2s (Retry-After honoured, not the 1ms computed backoff)", sl[0])
	}
}

// TestEmitHonorsRetryAfterHTTPDate [#122 acceptance #1]: the same, but Retry-After is an HTTP-date in
// the future — its delta from now must be honoured.
func TestEmitHonorsRetryAfterHTTPDate(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	future := clk.now().Add(3 * time.Second).UTC()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", future.Format(http.TimeFormat))
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	if err := emitterWithClock(t, srv.URL, clk).Emit(context.Background(), oneBatch()); err != nil {
		t.Fatalf("expected success after honouring Retry-After date: %v", err)
	}
	sl := clk.sleeps()
	if len(sl) != 1 || sl[0] < 3*time.Second {
		t.Fatalf("slept %v, want a single wait >= 3s from the HTTP-date Retry-After", sl)
	}
}

// TestEmit429WithoutRetryAfterUnchanged [#122 acceptance #2]: a 429 with NO Retry-After must behave
// exactly as before — the computed exponential backoff (InitialDelay, Jitter 0) drives the wait.
func TestEmit429WithoutRetryAfterUnchanged(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(429) // no Retry-After header
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	if err := emitterWithClock(t, srv.URL, clk).Emit(context.Background(), oneBatch()); err != nil {
		t.Fatalf("expected success after computed backoff: %v", err)
	}
	sl := clk.sleeps()
	// Two 429s → two backoff sleeps: InitialDelay=1ms then 1ms*1.5=1.5ms (Jitter 0), unaffected by any
	// Retry-After logic.
	if len(sl) != 2 || sl[0] != time.Millisecond || sl[1] != 1500*time.Microsecond {
		t.Fatalf("computed-backoff schedule changed: got %v, want [1ms 1.5ms]", sl)
	}
}

// TestEmitRetryAfterBeyondBudgetShortCircuits [#122 acceptance #3]: a Retry-After longer than the
// remaining retry budget must short-circuit to a RetryableError immediately — no sleep past the
// deadline, and no second request to the gateway.
func TestEmitRetryAfterBeyondBudgetShortCircuits(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "600") // 10m, far beyond the 60s budget below
		w.WriteHeader(429)
	}))
	defer srv.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	em := New(Config{
		Endpoint: srv.URL, InstanceID: "123", Token: "secret-token",
		Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20,
		Retry: RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 60 * time.Second, Jitter: 0},
	})
	em.now = clk.now
	em.sleep = clk.sleepFn

	err := em.Emit(context.Background(), oneBatch())
	var re *emit.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("Retry-After beyond budget must surface as RetryableError, got %v", err)
	}
	if got := clk.sleeps(); len(got) != 0 {
		t.Fatalf("must NOT sleep past the deadline; slept %v", got)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("gateway hit %d times, want exactly 1 (no futile retry after a too-long Retry-After)", got)
	}
}

// TestParseRetryAfter unit-tests the header parser directly across the RFC 7231 forms and the
// malformed/past cases that must fall back to 0.
func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"0", 0},
		{"-3", 0},
		{"not-a-number", 0},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(-10 * time.Second).Format(http.TimeFormat), 0}, // past date → no wait
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in, now); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
