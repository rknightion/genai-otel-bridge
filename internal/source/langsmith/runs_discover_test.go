// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/source"
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

// TestDiscovery429IsQuotaExceeded (#101): a 429 during runs session discovery must surface as the
// quota sentinel (scheduler counts samples_skipped{reason=quota_exceeded}), NOT a generic collect error —
// mirroring queryRuns/sessions/usage on the same shared ~10req/10s tenant budget.
func TestDiscovery429IsQuotaExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		sessionFilter: `eq(name,"x")`, maxSessions: 100, sessionRefresh: time.Hour}
	_, err := l.resolveSessions(context.Background(), time.Unix(1000, 0).UTC(), false)
	if !errors.Is(err, source.ErrQuotaExceeded) {
		t.Fatalf("discovery 429 must be ErrQuotaExceeded, got %v", err)
	}
}

// TestDiscoveryQuerySharesSessionsShape (#54): runs discovery must send the SAME sort_by=start_time
// ordering the sibling /sessions loops require (a bare offset 403s on the real Cloudflare-fronted
// instance). Capture the discovery request query and assert the ordering params are present.
func TestDiscoveryQuerySharesSessionsShape(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "s1"}})
	}))
	defer srv.Close()
	l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		sessionFilter: `eq(name,"x")`, maxSessions: 100, sessionRefresh: time.Hour}
	if _, err := l.listSessionIDs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("sort_by") != "start_time" || gotQuery.Get("sort_by_desc") != "true" {
		t.Fatalf("discovery query must carry sort_by=start_time&sort_by_desc=true, got %v", gotQuery)
	}
	if gotQuery.Get("filter") != `eq(name,"x")` {
		t.Fatalf("discovery query must carry the session filter, got %v", gotQuery)
	}
}

// TestSetSessionsPageQuery is the single source of truth guarding the three GET /sessions callers from
// drifting (#54): all use this helper, so asserting its shape here locks the shared contract.
func TestSetSessionsPageQuery(t *testing.T) {
	q := url.Values{}
	setSessionsPageQuery(q, 100, 200, `eq(name,"p")`)
	if q.Get("sort_by") != "start_time" || q.Get("sort_by_desc") != "true" {
		t.Fatalf("missing required sort ordering: %v", q)
	}
	if q.Get("limit") != "100" || q.Get("offset") != "200" || q.Get("filter") != `eq(name,"p")` {
		t.Fatalf("pagination/filter wrong: %v", q)
	}
	// empty filter ⇒ no filter param
	q2 := url.Values{}
	setSessionsPageQuery(q2, 50, 0, "")
	if _, ok := q2["filter"]; ok {
		t.Fatalf("empty filter must not set the param: %v", q2)
	}
}
