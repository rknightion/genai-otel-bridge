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
)

// fakeSessions serves a /sessions list: `n` projects at offset 0, empty beyond (pagination terminates).
func fakeSessions(t *testing.T, n int, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		out := []map[string]any{}
		if r.URL.Query().Get("offset") == "0" || r.URL.Query().Get("offset") == "" {
			for i := range n {
				out = append(out, map[string]any{"id": "s" + string(rune('0'+i)), "name": "proj-" + string(rune('0'+i))})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveSessionsStatic(t *testing.T) {
	var hits int32
	srv := fakeSessions(t, 3, &hits)
	l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		sessionIDs: []string{"a", "b"}, maxSessions: 100, sessionRefresh: time.Hour}
	got, err := l.resolveSessions(context.Background(), time.Unix(0, 0).UTC(), false)
	if err != nil || len(got) != 2 || got[0] != "a" {
		t.Fatalf("static scope wrong: %v %v", got, err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("static scope must NOT hit /sessions")
	}
}

func TestResolveSessionsDiscoveryAndCache(t *testing.T) {
	var hits int32
	srv := fakeSessions(t, 3, &hits)
	l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		sessionFilter: `eq(name,"x")`, maxSessions: 100, sessionRefresh: time.Hour}
	now := time.Unix(1000, 0).UTC()
	got, err := l.resolveSessions(context.Background(), now, false)
	if err != nil || len(got) != 3 {
		t.Fatalf("discovery wrong: %v %v", got, err)
	}
	h1 := atomic.LoadInt32(&hits)
	// second call within TTL → cache hit, no new request
	got2, _ := l.resolveSessions(context.Background(), now.Add(time.Minute), false)
	if len(got2) != 3 || atomic.LoadInt32(&hits) != h1 {
		t.Fatalf("expected cache hit (no new request): got %v hits %d->%d", got2, h1, atomic.LoadInt32(&hits))
	}
}

func TestResolveSessionsCap(t *testing.T) {
	var hits int32
	srv := fakeSessions(t, 5, &hits)
	l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		sessionFilter: `eq(name,"x")`, maxSessions: 2, sessionRefresh: time.Hour}
	got, err := l.resolveSessions(context.Background(), time.Unix(0, 0).UTC(), false)
	if err != nil || len(got) != 2 {
		t.Fatalf("cap not enforced: %v %v", got, err)
	}
}

func TestResolveSessionsNoMatch(t *testing.T) {
	var hits int32
	srv := fakeSessions(t, 0, &hits)
	l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		sessionFilter: `eq(name,"nope")`, maxSessions: 100, sessionRefresh: time.Hour}
	if _, err := l.resolveSessions(context.Background(), time.Unix(0, 0).UTC(), false); err == nil {
		t.Fatal("a filter matching no projects must error loudly (not silently advance over no scope)")
	}
}
