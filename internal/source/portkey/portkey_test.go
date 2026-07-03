// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

func tAt(base time.Time, mins int) string {
	return base.Add(time.Duration(mins) * time.Minute).Format(time.RFC3339)
}

func TestDeriveForwardOnlyAndSettle(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	since := base // exclusive lower bound
	// 1-min buckets at base+1m .. base+9m (bucket-END semantics for this unit test).
	var dps []dataPoint
	for i := 1; i <= 9; i++ {
		dps = append(dps, dataPoint{Timestamp: tAt(base, i), Total: float64(i)})
	}
	resp := map[string]graphResponse{"requests": {DataPoints: dps}}
	got, err := derive(resp, "portkey_api", since, now, 3*time.Minute, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	// settled cutoff = now-3m = base+7m. forward-only: bucket_end > base.
	// ⇒ buckets base+1m .. base+7m = 7 samples.
	if len(got) != 7 {
		t.Fatalf("samples=%d want 7", len(got))
	}
	for _, s := range got {
		if s.Name != "portkey_api_requests" {
			t.Fatalf("name=%s", s.Name)
		}
		if !s.Timestamp.After(since) || s.Timestamp.After(now.Add(-3*time.Minute)) {
			t.Fatalf("bucket %v out of (%v, %v]", s.Timestamp, since, now.Add(-3*time.Minute))
		}
	}
}

// [OP5a] latency emits one gauge per percentile with a {quantile} label.
func TestDeriveLatencyQuantiles(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	// Portkey reports latency in MILLISECONDS (confirmed against live data 2026-06-19); derive must
	// convert to seconds for the `_seconds`-suffixed metric. Inputs are ms → expected values are seconds.
	var dps []dataPoint
	for i := 1; i <= 5; i++ {
		dps = append(dps, dataPoint{Timestamp: tAt(base, i), P50: 100 + float64(i)*10, P90: 400, P99: 1200})
	}
	resp := map[string]graphResponse{"latency": {DataPoints: dps}}
	got, err := derive(resp, "portkey_api", base, now, 3*time.Minute, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	// 5 settled buckets (base+1..base+5, all ≤ base+7m) × 4 stats (p50/p90/p99/avg) = 20 samples.
	if len(got) != 20 {
		t.Fatalf("latency samples=%d want 20", len(got))
	}
	seen := map[string]int{}
	for _, s := range got {
		if s.Name != "portkey_api_latency_seconds" {
			t.Fatalf("name=%s want portkey_api_latency_seconds", s.Name)
		}
		q := s.Labels["quantile"]
		if q != "p50" && q != "p90" && q != "p99" && q != "avg" {
			t.Fatalf("bad quantile label %q", q)
		}
		seen[q]++
		if q == "p90" && s.Value != 0.40 {
			t.Fatalf("p90 value=%v want 0.40 (400ms→0.40s)", s.Value)
		}
	}
	if seen["p50"] != 5 || seen["p90"] != 5 || seen["p99"] != 5 || seen["avg"] != 5 {
		t.Fatalf("per-stat counts wrong: %v", seen)
	}
}

// [parity] tokens emits one gauge per {token_type} (total/input/output) so the prompt/completion split
// is queryable, matching Portkey's data_points fields (total / total_request_units / total_response_units).
func TestDeriveTokenSplit(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	// One settled, forward bucket carrying the three token fields (total == input + output, as Portkey reports).
	dps := []dataPoint{{Timestamp: tAt(base, 1), Total: 1500, TotalRequestUnits: 1000, TotalResponseUnits: 500}}
	resp := map[string]graphResponse{"tokens": {DataPoints: dps}}
	got, err := derive(resp, "portkey_api", base, now, 3*time.Minute, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	// One bucket × three token_type values.
	if len(got) != 3 {
		t.Fatalf("token samples=%d want 3", len(got))
	}
	byType := map[string]float64{}
	for _, s := range got {
		if s.Name != "portkey_api_tokens" {
			t.Fatalf("name=%s want portkey_api_tokens", s.Name)
		}
		tt := s.Labels["token_type"]
		if tt != "total" && tt != "input" && tt != "output" {
			t.Fatalf("bad token_type label %q", tt)
		}
		byType[tt] = s.Value
	}
	if byType["total"] != 1500 || byType["input"] != 1000 || byType["output"] != 500 {
		t.Fatalf("token split values wrong: %v", byType)
	}
}

// [parity] latency also emits the arithmetic mean as {quantile="avg"} — the notebook surfaces avg
// alongside the percentiles, and avg is NOT derivable from p50/p90/p99 (so it must be emitted, not computed).
func TestDeriveLatencyAvg(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	// One settled bucket; Avg is in MILLISECONDS like the percentiles → expect seconds.
	dps := []dataPoint{{Timestamp: tAt(base, 1), Avg: 250, P50: 100, P90: 400, P99: 1200}}
	resp := map[string]graphResponse{"latency": {DataPoints: dps}}
	got, err := derive(resp, "portkey_api", base, now, 3*time.Minute, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	var avg *model.Sample
	for i := range got {
		if got[i].Labels["quantile"] == "avg" {
			avg = &got[i]
		}
	}
	if avg == nil {
		t.Fatalf("no {quantile=\"avg\"} latency sample emitted; got %d samples", len(got))
		return // unreachable after Fatalf, but proves avg non-nil to staticcheck (SA5011 false positive)
	}
	if avg.Name != "portkey_api_latency_seconds" || avg.Value != 0.25 {
		t.Fatalf("avg sample wrong: name=%s value=%v (want portkey_api_latency_seconds, 0.25)", avg.Name, avg.Value)
	}
}

// [round3-#1] A non-ascending response must be sorted and derived correctly, NOT trip the
// granularity guard (which assumes ascending order).
func TestDeriveOutOfOrderAccepted(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	// buckets base+5 .. base+1 in DESCENDING order.
	var dps []dataPoint
	for i := 5; i >= 1; i-- {
		dps = append(dps, dataPoint{Timestamp: tAt(base, i), Total: float64(i)})
	}
	resp := map[string]graphResponse{"requests": {DataPoints: dps}}
	got, err := derive(resp, "portkey_api", base, now, 3*time.Minute, time.Minute, false)
	if err != nil {
		t.Fatalf("out-of-order response must sort, not error: %v", err)
	}
	if len(got) != 5 { // base+1..base+5, all settled (≤ base+7m) and forward (> base)
		t.Fatalf("samples=%d want 5", len(got))
	}
	for i := 1; i < len(got); i++ { // emitted ascending after sort
		if !got[i].Timestamp.After(got[i-1].Timestamp) {
			t.Fatalf("samples not ascending: %v", got)
		}
	}
}

func TestDeriveGranularityGuard(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	rfc := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }

	// [AR-H-gran] A NON-multiple step (90s vs 60s) is a real granularity flip → reject.
	bad := map[string]graphResponse{"requests": {DataPoints: []dataPoint{
		{Timestamp: rfc(0), Total: 1}, {Timestamp: rfc(60 * time.Second), Total: 1}, {Timestamp: rfc(150 * time.Second), Total: 1},
	}}}
	if _, err := derive(bad, "portkey_api", base.Add(-time.Minute), base.Add(time.Hour), time.Minute, time.Minute, false); !errors.Is(err, source.ErrGranularityUnexpected) {
		t.Fatalf("non-multiple step: want ErrGranularityUnexpected, got %v", err)
	}

	// A k×gran gap (an OMITTED empty bucket) is normal → accepted, never an error (F43/OP5f).
	gap := map[string]graphResponse{"requests": {DataPoints: []dataPoint{
		{Timestamp: rfc(60 * time.Second), Total: 1}, {Timestamp: rfc(180 * time.Second), Total: 1}, // 120s = 2×gran
	}}}
	if _, err := derive(gap, "portkey_api", base, base.Add(time.Hour), time.Minute, time.Minute, false); err != nil {
		t.Fatalf("omitted zero-bucket (2×gran gap) must be accepted, got %v", err)
	}
}

// TestAnalyticsDecodesRawJSONWireTags pins the ON-THE-WIRE Portkey field names by decoding RAW JSON
// bodies (NOT struct-encoded graphResponse literals) through the production json tags, then deriving.
// Guards #64: every other analytics test builds its fixture from the SAME dataPoint struct the decoder
// uses, so a wire-tag regression (total_request_units→request_units, a p90 typo, is_quota_exceeded
// renamed) would pass all of them while silently zeroing the tokens/latency gauges or emitting
// quota-truncated data as if real. Mirrors the langsmith raw-fixture approach (testdata/*.json). A
// deliberate tag typo on dataPoint / graphResponse makes an assertion here fail.
func TestAnalyticsDecodesRawJSONWireTags(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)

	// --- tokens graph: {total, total_request_units, total_response_units} ---
	const tokensRaw = `{
	  "object": "analytics-graph",
	  "data_points": [
	    {"timestamp": "2026-06-18T12:01:00Z", "total": 1500, "total_request_units": 1000, "total_response_units": 500}
	  ]
	}`
	var tokensResp graphResponse
	if err := json.Unmarshal([]byte(tokensRaw), &tokensResp); err != nil {
		t.Fatalf("tokens raw decode: %v", err)
	}
	// Pin the wire tags at the struct level: a renamed tag decodes to the zero value here.
	if got := tokensResp.DataPoints[0]; got.Total != 1500 || got.TotalRequestUnits != 1000 || got.TotalResponseUnits != 500 {
		t.Fatalf("tokens wire tags decoded wrong: %+v (a dataPoint json tag regressed)", got)
	}
	tokSamples, err := derive(map[string]graphResponse{"tokens": tokensResp}, "portkey_api", base, now, 3*time.Minute, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]float64{}
	for _, s := range tokSamples {
		byType[s.Labels["token_type"]] = s.Value
	}
	if byType["total"] != 1500 || byType["input"] != 1000 || byType["output"] != 500 {
		t.Fatalf("tokens split from raw JSON wrong: %v (input/output come from total_request_units/total_response_units)", byType)
	}

	// --- latency graph: {avg, p50, p90, p99} — values in ms → seconds ---
	const latencyRaw = `{
	  "data_points": [
	    {"timestamp": "2026-06-18T12:01:00Z", "avg": 250, "p50": 100, "p90": 400, "p99": 1200}
	  ]
	}`
	var latResp graphResponse
	if err := json.Unmarshal([]byte(latencyRaw), &latResp); err != nil {
		t.Fatalf("latency raw decode: %v", err)
	}
	if got := latResp.DataPoints[0]; got.Avg != 250 || got.P50 != 100 || got.P90 != 400 || got.P99 != 1200 {
		t.Fatalf("latency wire tags decoded wrong: %+v (a percentile json tag regressed)", got)
	}
	latSamples, err := derive(map[string]graphResponse{"latency": latResp}, "portkey_api", base, now, 3*time.Minute, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	byQ := map[string]float64{}
	for _, s := range latSamples {
		byQ[s.Labels["quantile"]] = s.Value
	}
	if byQ["avg"] != 0.25 || byQ["p50"] != 0.1 || byQ["p90"] != 0.4 || byQ["p99"] != 1.2 {
		t.Fatalf("latency stats from raw JSON wrong (ms→s): %v", byQ)
	}

	// --- is_quota_exceeded:true must decode from the wire tag (else quota-truncated batches emit as real) ---
	const quotaRaw = `{"is_quota_exceeded": true, "data_points": [{"timestamp": "2026-06-18T12:01:00Z", "total": 99}]}`
	var quotaResp graphResponse
	if err := json.Unmarshal([]byte(quotaRaw), &quotaResp); err != nil {
		t.Fatalf("quota raw decode: %v", err)
	}
	if !quotaResp.IsQuotaExceeded {
		t.Fatal("is_quota_exceeded wire tag did not decode true (tag regressed → quota-truncated batches would emit as real data)")
	}
}

// TestCollectDecodesRawJSONThroughHTTP drives the FULL client HTTP decode path (not just json.Unmarshal)
// on raw JSON bodies for the tokens + latency graphs, asserting the wire field names survive fetch →
// decode → derive end-to-end. Complements TestAnalyticsDecodesRawJSONWireTags (#64) by exercising the
// real transport, mirroring the langsmith runs raw-body server tests.
func TestCollectDecodesRawJSONThroughHTTP(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	// Buckets base+1m..base+6m are settled+forward under now=base+10m, settle=3m, startSemantics=true.
	rawBody := func(field, extra string) string {
		var pts []string
		for i := 1; i <= 6; i++ {
			pts = append(pts, `{"timestamp": "`+tAt(base, i)+`", `+field+`}`)
		}
		return `{"data_points": [` + strings.Join(pts, ",") + `]` + extra + `}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		graph := r.URL.Path[len("/analytics/graphs/"):]
		switch graph {
		case "tokens":
			_, _ = w.Write([]byte(rawBody(`"total": 1500, "total_request_units": 1000, "total_response_units": 500`, "")))
		case "latency":
			_, _ = w.Write([]byte(rawBody(`"avg": 250, "p50": 100, "p90": 400, "p99": 1200`, "")))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	lp := mkSource(t, srv, now)
	lp.graphs = []string{"tokens", "latency"}
	b, err := lp.Collect(context.Background(), model.Watermark{Time: base})
	if err != nil {
		t.Fatal(err)
	}
	// Assert the token split + latency quantiles decoded from the raw wire tags survived to the samples.
	sawInput, sawOutput, sawP90 := false, false, false
	for _, s := range b.Samples {
		switch {
		case s.Name == "portkey_api_tokens" && s.Labels["token_type"] == "input" && s.Value == 1000:
			sawInput = true
		case s.Name == "portkey_api_tokens" && s.Labels["token_type"] == "output" && s.Value == 500:
			sawOutput = true
		case s.Name == "portkey_api_latency_seconds" && s.Labels["quantile"] == "p90" && s.Value == 0.4:
			sawP90 = true
		}
	}
	if !sawInput || !sawOutput || !sawP90 {
		t.Fatalf("raw-JSON HTTP decode lost wire tags: input=%v output=%v p90=%v (samples=%d)", sawInput, sawOutput, sawP90, len(b.Samples))
	}
}

func fakePortkey(t *testing.T, bodies map[string]graphResponse, status map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		graph := r.URL.Path[len("/analytics/graphs/"):]
		if code, ok := status[graph]; ok {
			w.WriteHeader(code)
			return
		}
		b, ok := bodies[graph]
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(b)
	}))
}

// [api-key-filter] When settings.api_key_ids is set, the analytics loop scopes every graph fetch to those
// key UUIDs (matching the notebook's api_key_ids filter) — comma-joined, trimmed. Unset ⇒ no param (workspace-wide).
func TestAnalyticsAPIKeyFilter(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	for _, tc := range []struct {
		name, setting, wantParam string
	}{
		{"unset", "", ""},
		{"single", "uuid-1", "uuid-1"},
		{"csv trimmed", " uuid-1 , uuid-2 ", "uuid-1,uuid-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotKeyIDs string
			var sawAny bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawAny = true
				gotKeyIDs = r.URL.Query().Get("api_key_ids")
				_, has := r.URL.Query()["api_key_ids"]
				if !has {
					gotKeyIDs = "\x00" // sentinel: param entirely absent
				}
				json.NewEncoder(w).Encode(graphResponse{DataPoints: []dataPoint{{Timestamp: tAt(base, 1), Total: 1}}})
			}))
			defer srv.Close()
			lp := mkSource(t, srv, now)
			lp.passes[0].apiKeyIDsCSV = cleanAPIKeyIDs(tc.setting)
			if _, err := lp.Collect(context.Background(), model.Watermark{Time: base}); err != nil {
				t.Fatal(err)
			}
			if !sawAny {
				t.Fatal("server never hit")
			}
			if tc.wantParam == "" {
				if gotKeyIDs != "\x00" {
					t.Fatalf("expected NO api_key_ids param, got %q", gotKeyIDs)
				}
				return
			}
			if gotKeyIDs != tc.wantParam {
				t.Fatalf("api_key_ids=%q want %q", gotKeyIDs, tc.wantParam)
			}
		})
	}
}

func mkSource(t *testing.T, srv *httptest.Server, now time.Time) *analyticsLoop {
	t.Helper()
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true}, // httptest is loopback
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
			BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
			MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
			Graphs: []string{"requests", "latency"},
		}},
	}
	src, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	lp := src.Loops()[0].(*analyticsLoop)
	lp.now = func() time.Time { return now }
	return lp
}

// TestNewRejectsZeroWindow guards the AR-HIGH fix: a time-bucketed analytics loop with no window
// (window==0, e.g. omitted from config — Load doesn't apply the chart default) would silently no-op
// (until ≤ start every tick → empty batch, watermark never advances). New must fail fast instead.
func TestNewRejectsZeroWindow(t *testing.T) {
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1", SourceInstance: "pk",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 1},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(time.Minute), // Window intentionally omitted (0)
			Graphs: []string{"requests"},
		}},
	}
	if _, err := New(cfg, source.Deps{}); err == nil {
		t.Fatal("expected New to reject a zero/omitted window on the time-bucketed analytics loop")
	}
}

// TestNewRejectsSilentNoOpAnalyticsConfig guards #57: a raw-YAML analytics config (config.Load applies no
// defaults for these knobs) where max_backfill is omitted/0, or bootstrap_lookback <= bucket_settle, would
// make Collect return an empty batch with an un-advancing (zero) watermark EVERY tick — a permanent, fully
// silent data gap (window_lag skipped on zero watermark, no samples, no errors). New must fail fast so the
// zero-value config can never reach the silent empty-Collect loop.
func TestNewRejectsSilentNoOpAnalyticsConfig(t *testing.T) {
	mk := func(bootstrap, maxBackfill, settle time.Duration) config.SourceConfig {
		return config.SourceConfig{
			Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1", SourceInstance: "pk",
			Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
			RateLimit: config.RateLimitConfig{RPS: 1, Burst: 1},
			Loops: map[string]config.LoopConfig{"analytics": {
				Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
				BucketSettle: config.Duration(settle), BootstrapLookback: config.Duration(bootstrap),
				MaxBackfill: config.Duration(maxBackfill), Graphs: []string{"requests"},
			}},
		}
	}
	cases := []struct {
		name                           string
		bootstrap, maxBackfill, settle time.Duration
		wantErr                        bool
	}{
		{"max_backfill omitted (0)", 50 * time.Minute, 0, 10 * time.Minute, true},
		{"max_backfill <= settle", 50 * time.Minute, 10 * time.Minute, 10 * time.Minute, true},
		{"bootstrap == settle", 10 * time.Minute, 90 * time.Minute, 10 * time.Minute, true},
		{"bootstrap < settle", 5 * time.Minute, 90 * time.Minute, 10 * time.Minute, true},
		{"both zero", 0, 0, 0, true},
		{"valid (chart defaults)", 50 * time.Minute, 90 * time.Minute, 10 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(mk(tc.bootstrap, tc.maxBackfill, tc.settle), source.Deps{})
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v got err=%v", tc.wantErr, err)
			}
		})
	}
}

func TestCollectDerivesAndForwardOnly(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	mk := func() []dataPoint {
		var d []dataPoint
		for i := 1; i <= 9; i++ {
			d = append(d, dataPoint{Timestamp: tAt(base, i), Total: float64(i), P50: 0.1, P90: 0.4, P99: 1.0})
		}
		return d
	}
	srv := fakePortkey(t, map[string]graphResponse{"requests": {DataPoints: mk()}, "latency": {DataPoints: mk()}}, nil)
	defer srv.Close()
	lp := mkSource(t, srv, now)
	b, err := lp.Collect(context.Background(), model.Watermark{Time: base})
	if err != nil {
		t.Fatal(err)
	}
	// startSemantics=true (OP5e): bucket_end = ts+1m. settled buckets ⇒ ts base+1..base+6 = 6 buckets.
	// requests: 6 samples; latency: 6×4 stats (p50/p90/p99/avg) = 24; total 30. watermark = until = base+7m.
	if len(b.Samples) != 30 {
		t.Fatalf("samples=%d want 30 (6 requests + 24 latency stats)", len(b.Samples))
	}
	if !b.Watermark.Time.Equal(base.Add(7 * time.Minute)) {
		t.Fatalf("watermark=%v want base+7m", b.Watermark.Time)
	}
}

func TestCollectQuotaDiscards(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakePortkey(t, map[string]graphResponse{
		"requests": {IsQuotaExceeded: true, DataPoints: []dataPoint{{Timestamp: tAt(base, 1), Total: 99}}},
		"latency":  {DataPoints: []dataPoint{{Timestamp: tAt(base, 1), P50: 0.1}}},
	}, nil)
	defer srv.Close()
	lp := mkSource(t, srv, base.Add(10*time.Minute))
	_, err := lp.Collect(context.Background(), model.Watermark{Time: base})
	if !errors.Is(err, source.ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
}

func TestCollect404IsCapabilitySkipNotFatal(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	srv := fakePortkey(t,
		map[string]graphResponse{"requests": {DataPoints: []dataPoint{{Timestamp: tAt(base, 1), Total: 1}}}},
		map[string]int{"latency": 404}, // latency endpoint absent on this plan/version
	)
	defer srv.Close()
	lp := mkSource(t, srv, now)
	b, err := lp.Collect(context.Background(), model.Watermark{Time: base})
	if err != nil {
		t.Fatalf("404 on one graph must not be fatal: %v", err)
	}
	if len(b.Samples) != 1 {
		t.Fatalf("samples=%d want 1 (requests only; latency skipped)", len(b.Samples))
	}
}

// TestCollect404FiresGraphSkippedHook asserts a skipped-404 graph fires Deps.OnGraphSkipped so the
// (otherwise silent) skip is observable via SourceGraphUnavailable (round3-#4).
func TestCollect404FiresGraphSkippedHook(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	srv := fakePortkey(t,
		map[string]graphResponse{"requests": {DataPoints: []dataPoint{{Timestamp: tAt(base, 1), Total: 1}}}},
		map[string]int{"latency": 404},
	)
	defer srv.Close()
	var skipped []string
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
			BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
			MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
			Graphs: []string{"requests", "latency"},
		}},
	}
	src, err := New(cfg, source.Deps{OnGraphSkipped: func(_, g string) { skipped = append(skipped, g) }})
	if err != nil {
		t.Fatal(err)
	}
	lp := src.Loops()[0].(*analyticsLoop)
	lp.now = func() time.Time { return now }
	if _, err := lp.Collect(context.Background(), model.Watermark{Time: base}); err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "latency" {
		t.Fatalf("OnGraphSkipped should fire once for 'latency', got %v", skipped)
	}
}

// TestCollectAuthErrorFiresHook asserts a 401/403 from a graph fires Deps.OnAuthError(loop,source) so a
// credential failure is its own alertable signal (followup §9), in addition to the loud retryable error.
func TestCollectAuthErrorFiresHook(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)
	srv := fakePortkey(t,
		map[string]graphResponse{"requests": {DataPoints: []dataPoint{{Timestamp: tAt(base, 1), Total: 1}}}},
		map[string]int{"latency": 403}, // expired/under-scoped key
	)
	defer srv.Close()
	var auth [][2]string
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
			BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
			MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
			Graphs: []string{"requests", "latency"},
		}},
	}
	src, err := New(cfg, source.Deps{OnAuthError: func(loop, s string) { auth = append(auth, [2]string{loop, s}) }})
	if err != nil {
		t.Fatal(err)
	}
	lp := src.Loops()[0].(*analyticsLoop)
	lp.now = func() time.Time { return now }
	if _, err := lp.Collect(context.Background(), model.Watermark{Time: base}); err == nil {
		t.Fatal("a 403 must surface as a retryable error")
	}
	if len(auth) != 1 || auth[0] != [2]string{"analytics", "pk-test"} {
		t.Fatalf("OnAuthError want one (analytics,pk-test), got %v", auth)
	}
}

// [CP-R3] All configured graphs 404 → ERROR (capability/config), NOT a healthy empty stream.
func TestCollectAllGraphs404IsError(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	srv := fakePortkey(t, nil, map[string]int{"requests": 404, "latency": 404})
	defer srv.Close()
	lp := mkSource(t, srv, base.Add(10*time.Minute))
	if _, err := lp.Collect(context.Background(), model.Watermark{Time: base}); err == nil {
		t.Fatal("all configured graphs 404 must error, not advance over 'no data'")
	}
}

func TestCollectNeverSelectsContentFields(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery += r.URL.RawQuery + "&"
		json.NewEncoder(w).Encode(graphResponse{DataPoints: []dataPoint{{Timestamp: tAt(base, 1), Total: 1}}})
	}))
	defer srv.Close()
	lp := mkSource(t, srv, base.Add(10*time.Minute))
	lp.Collect(context.Background(), model.Watermark{Time: base})
	for _, bad := range []string{"request", "response", "inputs", "outputs", "events"} {
		if strings.Contains(sawQuery, bad) {
			t.Fatalf("content field %q appeared in query %q (FR10 violation)", bad, sawQuery)
		}
	}
}
