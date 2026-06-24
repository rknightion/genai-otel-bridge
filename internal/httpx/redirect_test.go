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
