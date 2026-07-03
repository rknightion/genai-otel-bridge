// SPDX-License-Identifier: AGPL-3.0-only

package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// mustReq builds a *http.Request for a raw URL for the redirect-policy unit tests.
func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("build request %q: %v", raw, err)
	}
	return req
}

// TestCheckRedirectCreds covers the redirect credential-leak policy: cross-host is blocked, a same-host
// https→http DOWNGRADE is blocked (#62 — Go forwards the vendor auth header same-host, so cleartext must
// never be reached), while legitimate same-host / same-scheme (incl. path-canonicalisation) redirects and
// an http→https UPGRADE are allowed.
func TestCheckRedirectCreds(t *testing.T) {
	cases := []struct {
		name      string
		origin    string
		next      string
		wantBlock bool
	}{
		{"same-host same-scheme path canonicalisation", "https://api.example.com/v1/x", "https://api.example.com/v1/x/", false},
		{"same-host https->http downgrade (#62)", "https://api.example.com/v1/x", "http://api.example.com/v1/x", true},
		{"same-host http->https upgrade", "http://api.example.com/v1/x", "https://api.example.com/v1/x", false},
		{"cross-host https->https", "https://api.example.com/v1/x", "https://evil.example.com/v1/x", true},
		{"same-host http->http (never was secure)", "http://api.example.com/v1/x", "http://api.example.com/y", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			via := []*http.Request{mustReq(t, tc.origin)}
			err := checkRedirectCreds(mustReq(t, tc.next), via)
			if tc.wantBlock && err == nil {
				t.Fatalf("redirect %s -> %s should be blocked", tc.origin, tc.next)
			}
			if !tc.wantBlock && err != nil {
				t.Fatalf("redirect %s -> %s should be allowed, got %v", tc.origin, tc.next, err)
			}
		})
	}
	// The very first hop (empty via) is never a credential-leak concern.
	if err := checkRedirectCreds(mustReq(t, "http://api.example.com/x"), nil); err != nil {
		t.Fatalf("initial hop (no via) must not be blocked: %v", err)
	}
}

func TestReviewRedirectRechecksAllowHosts(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(targetURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	targetURL.Host = net.JoinHostPort("localhost", port)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetURL.String(), http.StatusFound)
	}))
	defer redirector.Close()

	c := New(Config{Timeout: 2 * time.Second, AllowHosts: []string{"127.0.0.1"}, AllowPrivate: true})
	req, err := http.NewRequest(http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("redirect to disallowed host succeeded with status %d; target hit=%v", resp.StatusCode, hit)
	}
	if hit {
		t.Fatal("redirect target was reached despite not being in allow_hosts")
	}
}

// [ext-review-14] Go's http.Client strips Authorization/Cookie on a cross-domain redirect but FORWARDS
// arbitrary custom headers — so a source's vendor auth header (e.g. x-portkey-api-key) would leak to a
// different host on redirect. The client must reject cross-host redirects (the auth header never
// reaches another origin). Here a redirect crosses from 127.0.0.1 to "localhost" (same IP, different
// hostname) with no allow_hosts, so only the cross-host rule can stop it.
func TestRedirectDoesNotForwardAuthCrossHost(t *testing.T) {
	var gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-portkey-api-key")
	}))
	defer target.Close()
	tu, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(tu.Host)
	if err != nil {
		t.Fatal(err)
	}
	crossHost := "http://" + net.JoinHostPort("localhost", port) // same IP, DIFFERENT hostname than the redirector

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, crossHost, http.StatusFound)
	}))
	defer redirector.Close()

	c := New(Config{Timeout: 2 * time.Second, AllowPrivate: true}) // no AllowHosts → IP guard alone permits both
	req, err := http.NewRequest(http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-portkey-api-key", "secret")
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected cross-host redirect to be rejected")
	}
	if gotAuth == "secret" {
		t.Fatal("source credential was forwarded across a cross-host redirect")
	}
}
