// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// fakeGroups serves /analytics/groups/<endpoint> with current_page pagination. `pages[ep][i]` is the
// response for current_page=i (past the end ⇒ empty data, mirroring the live "total drops to 0" end
// behaviour). `pageStatus[ep][i]` overrides the HTTP status for a specific (endpoint,page). Captured
// request URIs (path+query) land in *queries when non-nil.
func fakeGroups(t *testing.T, pages map[string][]groupsResponse, pageStatus map[string]map[int]int, queries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep := strings.TrimPrefix(r.URL.Path, "/analytics/groups/")
		if queries != nil {
			*queries = append(*queries, r.URL.RequestURI())
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("current_page"))
		if st, ok := pageStatus[ep][page]; ok {
			w.WriteHeader(st)
			return
		}
		ps := pages[ep]
		if page < len(ps) {
			_ = json.NewEncoder(w).Encode(ps[page])
			return
		}
		_ = json.NewEncoder(w).Encode(groupsResponse{Object: "list", Total: 0, Data: nil}) // past end
	}))
}

func modelRows(models ...string) groupsResponse {
	r := groupsResponse{Object: "list"}
	for i, m := range models {
		raw := map[string]json.RawMessage{
			"ai_model": json.RawMessage(strconv.Quote(m)),
			"requests": json.RawMessage(strconv.Itoa(100 + i)),
			"cost":     json.RawMessage("1.5"),
			"object":   json.RawMessage(`"analytics-group"`),
		}
		r.Data = append(r.Data, raw)
		r.Total++
	}
	return r
}

func metaRows(values ...string) groupsResponse {
	r := groupsResponse{Object: "list"}
	for i, v := range values {
		raw := map[string]json.RawMessage{
			"metadata_value": json.RawMessage(strconv.Quote(v)),
			"requests":       json.RawMessage(strconv.Itoa(10 + i)),
			"object":         json.RawMessage(`"analytics-group"`),
		}
		r.Data = append(r.Data, raw)
		r.Total++
	}
	return r
}

func groupsCfg(srv *httptest.Server, settings map[string]string) config.SourceConfig {
	return config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"groups": {
			Enabled: true, Cadence: config.Duration(5 * time.Minute), MetricPrefix: "portkey_api",
			Settings: settings,
		}},
	}
}

func mkGroups(t *testing.T, cfg config.SourceConfig, deps source.Deps, now time.Time) *groupsLoop {
	t.Helper()
	src, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	for _, lp := range src.Loops() {
		if gl, ok := lp.(*groupsLoop); ok {
			gl.now = func() time.Time { return now }
			return gl
		}
	}
	t.Fatal("no groups loop built")
	return nil
}

// TestGroupsCollectAuthErrorFiresHook asserts a 401/403 on a groups endpoint fires Deps.OnAuthError so a
// credential failure is its own alertable signal (followup §9), distinct from a generic skip/slow endpoint.
func TestGroupsCollectAuthErrorFiresHook(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, nil, map[string]map[int]int{"ai-models": {0: 401}}, nil)
	defer srv.Close()
	var auth [][2]string
	// emit_prompts:false keeps the endpoint set to the single failing ai-models endpoint, so the all-fail
	// assertion + single-hook-fire assertion stay precise (the default-on prompt endpoint would otherwise
	// succeed empty and add a second poll).
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "emit_prompts": "false"}),
		source.Deps{OnAuthError: func(loop, s string) { auth = append(auth, [2]string{loop, s}) }}, now)
	if _, err := gl.Collect(context.Background(), model.Watermark{Time: now.Add(-time.Hour)}); err == nil {
		t.Fatal("all-endpoints-fail (401) must error")
	}
	if len(auth) != 1 || auth[0] != [2]string{"groups", "pk-test"} {
		t.Fatalf("OnAuthError want one (groups,pk-test), got %v", auth)
	}
}

func countByName(samples []model.Sample) map[string]int {
	m := map[string]int{}
	for _, s := range samples {
		m[s.Name]++
	}
	return m
}

func TestGroupsCollectPaginates(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models": {modelRows("a", "b", "c"), modelRows("d", "e")}, // page0 full(3), page1 short(2)
	}, nil, nil)
	defer srv.Close()
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "3", "settle": "10m", "window_span": "1h"}), source.Deps{}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{Time: now.Add(-time.Hour), Epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	if c := countByName(b.Samples); c["portkey_api_requests_by_model"] != 5 {
		t.Fatalf("requests_by_model=%d want 5 (paginated across 2 pages); all=%v", c["portkey_api_requests_by_model"], c)
	}
	// snapshot heartbeat: watermark advances to now (liveness cursor), Epoch carried through.
	if !b.Watermark.Time.Equal(now) || b.Watermark.Epoch != 7 {
		t.Fatalf("watermark=%+v want Time=now Epoch=7", b.Watermark)
	}
	// gauge stamped at the window upper bound (now-settle), not now.
	if len(b.Samples) > 0 && !b.Samples[0].Timestamp.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("sample ts=%v want now-settle", b.Samples[0].Timestamp)
	}
}

func TestGroupsCollectPartialPageIsAllOrNothing(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models":         {modelRows("a", "b", "c")}, // page0 full(3) ⇒ loop asks page1…
		"metadata/use_case": {metaRows("chatbot")},
	}, map[string]map[int]int{
		"ai-models": {1: http.StatusInternalServerError}, // …page1 500 ⇒ whole endpoint discarded
	}, nil)
	defer srv.Close()
	var skipped []string
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "3", "metadata_keys": "use_case"}),
		source.Deps{OnGraphSkipped: func(_, g string) { skipped = append(skipped, g) }}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("metadata succeeded so the loop must not error: %v", err)
	}
	c := countByName(b.Samples)
	if c["portkey_api_requests_by_model"] != 0 {
		t.Fatalf("ai-models page1 failed ⇒ entire endpoint discarded (all-or-nothing); got %d by_model samples", c["portkey_api_requests_by_model"])
	}
	if c["portkey_api_requests_by_metadata"] != 1 {
		t.Fatalf("metadata endpoint should still emit; got %d", c["portkey_api_requests_by_metadata"])
	}
	if len(skipped) != 1 || skipped[0] != "ai-models" {
		t.Fatalf("OnGraphSkipped want [ai-models], got %v", skipped)
	}
}

func TestGroupsCollectPerEndpointIndependent(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models": {modelRows("a", "b")},
	}, map[string]map[int]int{
		"metadata/use_case": {0: http.StatusInternalServerError},
	}, nil)
	defer srv.Close()
	var skipped []string
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "metadata_keys": "use_case"}),
		source.Deps{OnGraphSkipped: func(_, g string) { skipped = append(skipped, g) }}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("one endpoint 5xx must not fail the whole snapshot: %v", err)
	}
	if countByName(b.Samples)["portkey_api_requests_by_model"] != 2 {
		t.Fatalf("ai-models should emit independently of metadata failure")
	}
	if len(skipped) != 1 || skipped[0] != "metadata/use_case" {
		t.Fatalf("skip hook want [metadata/use_case], got %v", skipped)
	}
}

func TestGroupsCollectAllEndpointsFailErrors(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, nil, map[string]map[int]int{
		"ai-models": {0: http.StatusInternalServerError},
	}, nil)
	defer srv.Close()
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "emit_prompts": "false"}), source.Deps{}, now)
	if _, err := gl.Collect(context.Background(), model.Watermark{}); err == nil {
		t.Fatal("all endpoints failing must error (loud, no advance), not return an empty healthy batch")
	}
}

func TestGroupsCollectQuotaPerEndpoint(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	quota := metaRows("x")
	quota.IsQuotaExceeded = true
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models":         {modelRows("a", "b")},
		"metadata/use_case": {quota},
	}, nil, nil)
	defer srv.Close()
	var skipped []string
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "metadata_keys": "use_case"}),
		source.Deps{OnGraphSkipped: func(_, g string) { skipped = append(skipped, g) }}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("a per-endpoint quota must discard that endpoint, not error the loop: %v", err)
	}
	if countByName(b.Samples)["portkey_api_requests_by_model"] != 2 {
		t.Fatal("ai-models should still emit when only metadata hits quota")
	}
	if countByName(b.Samples)["portkey_api_requests_by_metadata"] != 0 {
		t.Fatal("quota-exceeded metadata endpoint must emit nothing")
	}
	if len(skipped) != 1 || skipped[0] != "metadata/use_case" {
		t.Fatalf("quota skip hook want [metadata/use_case], got %v", skipped)
	}
}

func TestGroupsCollectEmptyEndpointIsHealthy(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models":         {modelRows("a")},
		"metadata/use_case": {{Object: "list", Total: 0, Data: nil}}, // untagged key ⇒ 200 total:0
	}, nil, nil)
	defer srv.Close()
	var skipped []string
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "metadata_keys": "use_case"}),
		source.Deps{OnGraphSkipped: func(_, g string) { skipped = append(skipped, g) }}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if countByName(b.Samples)["portkey_api_requests_by_metadata"] != 0 {
		t.Fatal("an empty (200 total:0) endpoint emits nothing")
	}
	if len(skipped) != 0 {
		t.Fatalf("an empty endpoint is a healthy success, NOT a skip; got %v", skipped)
	}
}

func TestGroupsCollectQueryParams(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	var queries []string
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, nil, &queries)
	defer srv.Close()
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100"}), source.Deps{}, now)
	if _, err := gl.Collect(context.Background(), model.Watermark{}); err != nil {
		t.Fatal(err)
	}
	if len(queries) == 0 {
		t.Fatal("no request captured")
	}
	q := queries[0]
	for _, must := range []string{"time_of_generation_min", "time_of_generation_max", "page_size", "current_page"} {
		if !strings.Contains(q, must) {
			t.Fatalf("query %q missing required param %q", q, must)
		}
	}
	for _, bad := range []string{"request", "response", "inputs", "outputs", "events", "metadata"} {
		if strings.Contains(q, bad) {
			t.Fatalf("content/PII field %q must never appear in a groups query: %q", bad, q)
		}
	}
}

func TestGroupsCostDefaultOnAndUSDName(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a", "b")}}, nil, nil)
	defer srv.Close()
	// emit_cost unset ⇒ default ON ⇒ cost_usd gauge ships by default (unit confirmed as cents).
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100"}), source.Deps{}, now)
	b, _ := gl.Collect(context.Background(), model.Watermark{})
	c := countByName(b.Samples)
	if c["portkey_api_cost_usd_by_model"] != 2 {
		t.Fatalf("cost_usd must be ON by default and emit 2 gauges; got %d (all=%v)", c["portkey_api_cost_usd_by_model"], c)
	}
	if c["portkey_api_cost_by_model"] != 0 {
		t.Fatal("old neutral name portkey_api_cost_by_model must never appear")
	}
	// emit_cost=false ⇒ operator can still suppress the cost gauge.
	gl2 := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "emit_cost": "false"}), source.Deps{}, now)
	b2, _ := gl2.Collect(context.Background(), model.Watermark{})
	if countByName(b2.Samples)["portkey_api_cost_usd_by_model"] != 0 {
		t.Fatal("emit_cost=false must suppress the cost_usd gauge")
	}
}

func TestNewBuildsGroupsAndBothLoops(t *testing.T) {
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, nil, nil)
	defer srv.Close()
	// groups only
	src, err := New(groupsCfg(srv, map[string]string{}), source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Loops()) != 1 {
		t.Fatalf("groups-only ⇒ 1 loop, got %d", len(src.Loops()))
	}
	if _, ok := src.Loops()[0].(*groupsLoop); !ok {
		t.Fatalf("loop[0] is not a groupsLoop: %T", src.Loops()[0])
	}
	// analytics + groups: analytics must be first (existing tests depend on Loops()[0]=analytics).
	cfg := groupsCfg(srv, map[string]string{})
	cfg.Loops["analytics"] = config.LoopConfig{
		Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
		BucketSettle: config.Duration(10 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
		MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
		Graphs: []string{"requests"},
	}
	src2, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src2.Loops()) != 2 {
		t.Fatalf("both ⇒ 2 loops, got %d", len(src2.Loops()))
	}
	if _, ok := src2.Loops()[0].(*analyticsLoop); !ok {
		t.Fatalf("loop[0] must be analyticsLoop, got %T", src2.Loops()[0])
	}
	if _, ok := src2.Loops()[1].(*groupsLoop); !ok {
		t.Fatalf("loop[1] must be groupsLoop, got %T", src2.Loops()[1])
	}
}

func TestGroupsSeriesNamesDistinct(t *testing.T) {
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, nil, nil)
	defer srv.Close()
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"metadata_keys": "use_case", "emit_cost": "true"}), source.Deps{}, time.Now().UTC())
	names := gl.SeriesNames()
	want := map[string]bool{
		"portkey_api_requests_by_model":    true,
		"portkey_api_cost_usd_by_model":    true,
		"portkey_api_requests_by_metadata": true,
		"portkey_api_cost_usd_by_metadata": true,
		"portkey_api_requests_by_prompt":   true, // emit_prompts default-on (no cost on this dimension)
	}
	if len(names) != len(want) {
		t.Fatalf("series names=%v want %d distinct", names, len(want))
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected series name %q", n)
		}
		// must NOT collide with the analytics aggregate names.
		if n == "portkey_api_requests" || n == "portkey_api_cost_usd" {
			t.Fatalf("groups name %q collides with an analytics aggregate name", n)
		}
	}
}

// [review-H1] settle >= window_span inverts the query window (time_of_generation_min > max). Reject it
// fast (loud misconfig), mirroring the analytics positive-window guard, instead of a silent-empty poll.
func TestGroupsRejectsInvertedWindow(t *testing.T) {
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, nil, nil)
	defer srv.Close()
	// settle (2h) >= window_span (default 1h) ⇒ until < from ⇒ inverted window. New must fail fast.
	if _, err := New(groupsCfg(srv, map[string]string{"settle": "2h"}), source.Deps{}); err == nil {
		t.Fatal("expected New to reject settle >= window_span (inverted query window)")
	}
	// settle == window_span is also degenerate (zero-width window) → reject.
	if _, err := New(groupsCfg(srv, map[string]string{"settle": "1h", "window_span": "1h"}), source.Deps{}); err == nil {
		t.Fatal("expected New to reject settle == window_span (zero-width window)")
	}
}

// [review-M1] An offset-ignoring server that returns the SAME full page for every current_page must
// not infinite-loop AND must not silently early-truncate: pagination is bounded by a page cap and
// returns the real (deduped) rows, not a partial set.
func TestGroupsPaginationBoundedOnIgnoringServer(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(modelRows("a", "b", "c")) // same full page regardless of current_page
	}))
	defer srv.Close()
	// page_size 3 (page is always "full"), max_groups 6 ⇒ page cap = ceil(6/3) = 2 pages.
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "3", "max_groups": "6"}), source.Deps{}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatal(err)
	}
	if hits > 4 { // bounded: a few pages, not unbounded
		t.Fatalf("offset-ignoring server caused %d requests — pagination not bounded", hits)
	}
	if got := countByName(b.Samples)["portkey_api_requests_by_model"]; got != 3 {
		t.Fatalf("want the 3 real deduped rows (not truncated, not duplicated), got %d", got)
	}
}

// [review-M3] A snapshot loop stamping at now-settle must stamp at MINUTE resolution so two polls in the
// same wall-clock minute produce the SAME timestamp — otherwise distinct sub-minute timestamps would
// emit >1 point/series/minute and violate the 1DPM cap (CoalesceDPM is per-batch, can't dedup across polls).
func TestGroupsTimestampTruncatedToMinute(t *testing.T) {
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, nil, nil)
	defer srv.Close()
	poll := func(now time.Time) time.Time {
		gl := mkGroups(t, groupsCfg(srv, map[string]string{"settle": "10m", "page_size": "100"}), source.Deps{}, now)
		b, err := gl.Collect(context.Background(), model.Watermark{})
		if err != nil || len(b.Samples) == 0 {
			t.Fatalf("collect: err=%v samples=%d", err, len(b.Samples))
		}
		return b.Samples[0].Timestamp
	}
	// Two polls 13s apart but within the same wall-clock minute (12:00:37 and 12:00:50).
	ts1 := poll(time.Date(2026, 6, 18, 12, 0, 37, 0, time.UTC))
	ts2 := poll(time.Date(2026, 6, 18, 12, 0, 50, 0, time.UTC))
	if !ts1.Equal(ts2) {
		t.Fatalf("same-minute polls must share a timestamp (1DPM); got %v vs %v", ts1, ts2)
	}
	if ts1.Truncate(time.Minute) != ts1 {
		t.Fatalf("timestamp %v is not minute-aligned", ts1)
	}
}

func TestApplyGroupsSettings(t *testing.T) {
	base := func() groupsSettings {
		return groupsSettings{windowSpan: time.Hour, settle: 10 * time.Minute, pageSize: 1000, maxGroups: 10000}
	}
	// malformed known key → error
	g := base()
	if err := applyGroupsSettings(&g, map[string]string{"page_size": "notanint"}); err == nil {
		t.Fatal("malformed page_size must error")
	}
	// non-positive page_size → error
	g = base()
	if err := applyGroupsSettings(&g, map[string]string{"page_size": "0"}); err == nil {
		t.Fatal("page_size<=0 must error")
	}
	// unknown key → warn, no error
	g = base()
	if err := applyGroupsSettings(&g, map[string]string{"totally_unknown": "x"}); err != nil {
		t.Fatalf("unknown key should warn not error: %v", err)
	}
	// good values round-trip
	g = base()
	if err := applyGroupsSettings(&g, map[string]string{
		"window_span": "30m", "settle": "5m", "page_size": "250", "max_groups": "500",
		"metadata_keys": "use_case, tenant", "emit_cost": "true",
	}); err != nil {
		t.Fatal(err)
	}
	if g.windowSpan != 30*time.Minute || g.settle != 5*time.Minute || g.pageSize != 250 || g.maxGroups != 500 {
		t.Fatalf("settings not applied: %+v", g)
	}
	if !g.emitCost || len(g.metadataKeys) != 2 || g.metadataKeys[0] != "use_case" || g.metadataKeys[1] != "tenant" {
		t.Fatalf("metadata_keys/emit_cost not parsed (csv trim): %+v", g)
	}
}

// promptRows builds a /analytics/groups/prompt page. The dimension field is `prompt` and there is NO
// cost field (the prompt dimension returns request volume only — live-probed 2026-06-22).
func promptRows(slugs ...string) groupsResponse {
	r := groupsResponse{Object: "list"}
	for i, s := range slugs {
		raw := map[string]json.RawMessage{
			"prompt":   json.RawMessage(strconv.Quote(s)),
			"requests": json.RawMessage(strconv.Itoa(200 + i)),
			"object":   json.RawMessage(`"analytics-group"`),
		}
		r.Data = append(r.Data, raw)
		r.Total++
	}
	return r
}

// TestGroupsPromptDimension: with emit_prompts=true the loop polls /analytics/groups/prompt and emits a
// requests_by_prompt gauge per prompt with a {prompt} label and NO cost series (the dimension has none).
func TestGroupsPromptDimension(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models": {modelRows("gpt-4o")},
		"prompt":    {promptRows("summarize-v2", "classify-v1")},
	}, nil, nil)
	defer srv.Close()
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "emit_prompts": "true"}), source.Deps{}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	c := countByName(b.Samples)
	if c["portkey_api_requests_by_prompt"] != 2 {
		t.Fatalf("want 2 requests_by_prompt samples, got %d (all: %v)", c["portkey_api_requests_by_prompt"], c)
	}
	if c["portkey_api_cost_usd_by_prompt"] != 0 {
		t.Fatalf("prompt dimension has no cost; want 0 cost_by_prompt, got %d", c["portkey_api_cost_usd_by_prompt"])
	}
	var sawLabel bool
	for _, s := range b.Samples {
		if s.Name == "portkey_api_requests_by_prompt" {
			if _, ok := s.Labels["prompt"]; !ok {
				t.Fatalf("requests_by_prompt sample missing {prompt} label: %v", s.Labels)
			}
			sawLabel = true
		}
	}
	if !sawLabel {
		t.Fatal("no requests_by_prompt sample produced")
	}
}

// TestGroupsPromptDimensionOffByDefault: without emit_prompts the loop must NOT poll /prompt (proven by
// capturing the request URIs) nor emit any prompt series.
// TestGroupsPromptDimensionOnByDefault: with NO emit_prompts setting the loop polls /prompt and emits the
// requests_by_prompt series (the dimension is content-free + low-cardinality, so it ships on by default).
func TestGroupsPromptDimensionOnByDefault(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	var queries []string
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models": {modelRows("gpt-4o")},
		"prompt":    {promptRows("summarize-v2", "classify-v1")},
	}, nil, &queries)
	defer srv.Close()
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100"}), source.Deps{}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if countByName(b.Samples)["portkey_api_requests_by_prompt"] != 2 {
		t.Fatalf("emit_prompts defaults on — want 2 requests_by_prompt, got %d", countByName(b.Samples)["portkey_api_requests_by_prompt"])
	}
	var polled bool
	for _, q := range queries {
		if strings.Contains(q, "/analytics/groups/prompt") {
			polled = true
		}
	}
	if !polled {
		t.Fatal("emit_prompts defaults on — must poll the prompt endpoint")
	}
}

// TestGroupsPromptDimensionOptOut: emit_prompts=false suppresses the dimension (not polled, not emitted).
func TestGroupsPromptDimensionOptOut(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	var queries []string
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("gpt-4o")}}, nil, &queries)
	defer srv.Close()
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"page_size": "100", "emit_prompts": "false"}), source.Deps{}, now)
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if countByName(b.Samples)["portkey_api_requests_by_prompt"] != 0 {
		t.Fatal("emit_prompts=false — must emit no requests_by_prompt")
	}
	for _, q := range queries {
		if strings.Contains(q, "/analytics/groups/prompt") {
			t.Fatalf("emit_prompts=false — must not poll the prompt endpoint, but saw %q", q)
		}
	}
}
