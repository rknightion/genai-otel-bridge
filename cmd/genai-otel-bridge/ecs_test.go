// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveECSIdentityFromMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"TaskARN":"arn:aws:ecs:eu-west-1:111:task/cluster/abc123"}`))
	}))
	defer srv.Close()
	got := resolveECSIdentity(srv.URL)
	if got != "arn:aws:ecs:eu-west-1:111:task/cluster/abc123" {
		t.Fatalf("identity=%q, want the TaskARN", got)
	}
}

func TestResolveECSIdentityEmptyWhenNoEnv(t *testing.T) {
	if got := resolveECSIdentity(""); got != "" {
		t.Fatalf("identity=%q, want empty when metadata URI unset", got)
	}
}

func TestRunHealthCheck(t *testing.T) {
	// healthy ONLY on /healthz — proves healthCheckCode probes the right path (404 elsewhere → exit 1).
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
	}))
	defer ok.Close()
	if code := healthCheckCode(ok.URL); code != 0 {
		t.Fatalf("healthy /healthz probe exit=%d, want 0", code)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if code := healthCheckCode(bad.URL); code != 1 {
		t.Fatalf("unhealthy probe exit=%d, want 1", code)
	}
}

func TestLocalHealthURL(t *testing.T) {
	cases := map[string]string{
		":8080":          "http://127.0.0.1:8080",
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		"[::]:8080":      "http://127.0.0.1:8080",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
	}
	for in, want := range cases {
		if got := localHealthURL(in); got != want {
			t.Errorf("localHealthURL(%q)=%q, want %q", in, got, want)
		}
	}
}
