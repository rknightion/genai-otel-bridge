// SPDX-License-Identifier: AGPL-3.0-only

package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestEgressGuard(t *testing.T) {
	cases := []struct {
		addr         string
		allowPrivate bool
		wantErr      bool
	}{
		{"169.254.169.254:80", false, true}, // cloud metadata (link-local)
		{"169.254.169.254:80", true, true},  // ...still blocked even with allowPrivate
		{"10.0.0.5:443", false, true},       // RFC-1918 blocked by default
		{"10.0.0.5:443", true, false},       // ...permitted when allowPrivate
		{"192.168.1.1:443", false, true},
		{"100.100.100.200:80", false, true}, // [round3 MEDIUM-2] Alibaba metadata (CGNAT, not RFC-1918)
		{"100.64.1.1:443", false, true},     // [round3 MEDIUM-2] CGNAT blocked by default
		{"100.64.1.1:443", true, true},      // ...and ALWAYS, even with allowPrivate
		{"[fd00:ec2::254]:80", true, true},  // [round3 MEDIUM-2] AWS IPv6 IMDS (ULA) blocked even with allowPrivate
		{"93.184.216.34:443", false, false}, // public ok
	}
	for _, c := range cases {
		err := guardControl(c.allowPrivate)("tcp", c.addr, nil)
		if (err != nil) != c.wantErr {
			t.Errorf("guard(%s, allowPrivate=%v): err=%v want wantErr=%v", c.addr, c.allowPrivate, err, c.wantErr)
		}
	}
}

func TestDoSetsUserAgentAndAcquiresToken(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	c := New(Config{UserAgent: "aip-oi/0.1", Timeout: 2 * time.Second, AllowPrivate: true,
		Limiter: rate.NewLimiter(rate.Every(time.Millisecond), 1)})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotUA != "aip-oi/0.1" {
		t.Fatalf("UA=%q", gotUA)
	}
}

func TestDoBlocksOnExhaustedLimiterBeforeDialing(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	// Zero burst, refill once/hour ⇒ no token available within the test.
	c := New(Config{UserAgent: "x", Timeout: time.Second, AllowPrivate: true,
		Limiter: rate.NewLimiter(rate.Every(time.Hour), 0)})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected ctx deadline from limiter.Wait, got nil")
	}
	if hit {
		t.Fatal("server was hit despite no rate token (token must be acquired before the request)")
	}
}

func TestDoInvokesObserverOnResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	var got RequestInfo
	var calls int
	c := New(Config{Timeout: 2 * time.Second, AllowPrivate: true,
		Observer: func(i RequestInfo) { got = i; calls++ }})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Fatalf("observer should fire exactly once, got %d", calls)
	}
	if got.Method != http.MethodGet || got.StatusCode != 503 || got.Err != nil || got.Target == "" || got.Duration < 0 {
		t.Fatalf("unexpected observation: %+v", got)
	}
}

func TestDoInvokesObserverOnTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connections now refused → transport error (but the request WAS attempted upstream)
	var got RequestInfo
	var calls int
	c := New(Config{Timeout: 2 * time.Second, AllowPrivate: true,
		Observer: func(i RequestInfo) { got = i; calls++ }})
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected transport error")
	}
	if calls != 1 {
		t.Fatalf("observer should fire once even on error, got %d", calls)
	}
	if got.Err == nil || got.StatusCode != 0 {
		t.Fatalf("error observation should carry Err + StatusCode 0: %+v", got)
	}
}

func TestDoSkipsObserverWhenGuardBlocks(t *testing.T) {
	var calls int
	c := New(Config{Timeout: time.Second, AllowPrivate: false,
		Observer: func(RequestInfo) { calls++ }})
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected egress guard to block the metadata IP")
	}
	if calls != 0 {
		t.Fatalf("observer must not fire for a request the guard never sent, got %d", calls)
	}
}

func TestDoRejectsDisallowedHost(t *testing.T) {
	c := New(Config{UserAgent: "x", Timeout: time.Second, AllowHosts: []string{"api.portkey.ai"},
		Limiter: rate.NewLimiter(rate.Inf, 1)})
	req, _ := http.NewRequest(http.MethodGet, "https://evil.example.com/x", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected host-allowlist rejection")
	}
}
