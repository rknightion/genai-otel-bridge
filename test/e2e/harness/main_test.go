// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// The sink must read back exactly what the bridge's own encoder writes, or the e2e data checks would
// compare against a mis-decoded record.
func TestDecodeExportRoundTripsTheEmitterEncoding(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	body, err := otlp.Encode(map[string]string{"service.name": "x"}, []model.Sample{
		{Name: "portkey_api_requests", Kind: model.Gauge, Value: 7, Timestamp: ts},
		{Name: "portkey_api_latency_seconds", Kind: model.Gauge, Labels: map[string]string{"quantile": "p50"}, Value: 0.5, Timestamp: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	pts, err := decodeExport(body)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]point{}
	for _, p := range pts {
		got[p.Series] = p
	}
	if len(pts) != 2 {
		t.Fatalf("decoded %d points, want 2: %+v", len(pts), pts)
	}
	if p := got["portkey_api_requests"]; p.Value != 7 || !p.Time.Equal(ts) || p.Labels != "" {
		t.Fatalf("requests point = %+v", p)
	}
	if p := got["portkey_api_latency_seconds"]; p.Value != 0.5 || p.Labels != "quantile=p50" {
		t.Fatalf("latency point = %+v", p)
	}
}

// Every minute bucket the window touches gets a point, so a settled bucket is never missing from a
// response and the suite can treat a missing bucket as the bridge's gap.
func TestGraphPointsCoverEveryTouchedBucket(t *testing.T) {
	lo := time.Date(2026, 1, 2, 3, 4, 30, 0, time.UTC)
	hi := time.Date(2026, 1, 2, 3, 7, 10, 0, time.UTC)
	pts := graphPoints("requests", lo, hi, 0)
	want := []string{"2026-01-02T03:04:00.000Z", "2026-01-02T03:05:00.000Z", "2026-01-02T03:06:00.000Z", "2026-01-02T03:07:00.000Z"}
	if len(pts) != len(want) {
		t.Fatalf("got %d points, want %d: %v", len(pts), len(want), pts)
	}
	for i, p := range pts {
		if p["timestamp"] != want[i] {
			t.Fatalf("point %d timestamp %v, want %s", i, p["timestamp"], want[i])
		}
		b, _ := time.Parse(time.RFC3339, want[i])
		if p["total"] != pointValue(b) {
			t.Fatalf("point %d total %v, want %v", i, p["total"], pointValue(b))
		}
	}
	tok := graphPoints("tokens", lo, lo, 0)[0]
	if tok["total"] != tok["total_request_units"].(float64)+tok["total_response_units"].(float64) {
		t.Fatalf("tokens total must equal input+output: %v", tok)
	}
}

// The sink records each export's Basic-auth credential and its resource's pipeline attribute on every
// point, and /points?cred= serves one credential's points only, so the tenant suite can read what each
// tenant's destination received.
func TestSinkRecordsPointsPerCredential(t *testing.T) {
	s := &sink{}
	ingest := httptest.NewServer(http.HandlerFunc(s.ingest))
	defer ingest.Close()
	query := httptest.NewServer(s.queryMux())
	defer query.Close()

	ts := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	send := func(cred, pipeline string, v float64) {
		t.Helper()
		res := map[string]string{"service.name": "x"}
		if pipeline != "" {
			res[pipelineAttribute] = pipeline
		}
		body, err := otlp.Encode(res, []model.Sample{{Name: "portkey_api_requests", Kind: model.Gauge, Value: v, Timestamp: ts}})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, ingest.URL, strings.NewReader(string(body)))
		if cred != "" {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cred)))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send("901:token-a", "team-a.prod", 1)
	send("902:token-b", "team-b.prod", 2)
	send("901:token-a", "team-a.prod", 3)
	send("", "", 4)

	read := func(q string) (int64, []point) {
		t.Helper()
		resp, err := http.Get(query.URL + "/points" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var d struct {
			Exports int64   `json:"exports"`
			Points  []point `json:"points"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
		return d.Exports, d.Points
	}
	if n, pts := read(""); n != 4 || len(pts) != 4 {
		t.Fatalf("all points: %d exports, %d points; want 4 and 4", n, len(pts))
	}
	n, pts := read("?cred=901:token-a")
	if n != 4 || len(pts) != 2 {
		t.Fatalf("cred 901: %d exports, %d points %+v; want 4 exports and its 2 points", n, len(pts), pts)
	}
	for i, p := range pts {
		if p.Cred != "901:token-a" || p.Pipeline != "team-a.prod" || p.Value != float64(1+2*i) || p.Seq != int64(1+2*i) {
			t.Fatalf("cred 901 point %d = %+v", i, p)
		}
	}
	if _, pts := read("?cred=902:token-b"); len(pts) != 1 || pts[0].Pipeline != "team-b.prod" || pts[0].Value != 2 {
		t.Fatalf("cred 902 points = %+v", pts)
	}
	if _, pts := read("?cred="); len(pts) != 1 || pts[0].Pipeline != "" || pts[0].Value != 4 {
		t.Fatalf("points with no credential = %+v", pts)
	}
	if _, pts := read("?cred=903:none"); len(pts) != 0 {
		t.Fatalf("an unknown credential has points %+v", pts)
	}
}

// The mock serves each API key its own marker in every graph value and its own workspace in the
// workspace grouping; an unknown key is 401 and a workspace_slug that is not the key's is 400. The legacy
// key's values are pointValue unchanged, so the legacy phase's checks still hold.
func TestMockPortkeyServesPerKey(t *testing.T) {
	srv := httptest.NewServer(portkeyHandler())
	defer srv.Close()
	lo := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	window := "time_of_generation_min=" + lo.Format(time.RFC3339) + "&time_of_generation_max=" + lo.Add(30*time.Second).Format(time.RFC3339)
	get := func(path, key string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("x-portkey-api-key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if out != nil && resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode
	}
	type graph struct {
		DataPoints []map[string]any `json:"data_points"`
	}
	type groups struct {
		Data []map[string]any `json:"data"`
	}
	cases := []struct {
		key       string
		marker    int
		workspace string
	}{
		{"e2e", 0, ""},
		{"e2e-key-team-a", 1, "e2e-ws-team-a"},
		{"e2e-key-team-b", 2, "e2e-ws-team-b"},
		{"e2e-key-team-c", 3, "e2e-ws-team-c"},
	}
	for _, c := range cases {
		var g graph
		q := "?" + window
		if c.workspace != "" {
			q += "&workspace_slug=" + c.workspace
		}
		if code := get("/v1/analytics/graphs/requests"+q, c.key, &g); code != http.StatusOK || len(g.DataPoints) != 1 {
			t.Fatalf("%s: requests status %d, %d points", c.key, code, len(g.DataPoints))
		}
		if want := pointValue(lo) + float64(c.marker*markerStep); g.DataPoints[0]["total"] != want {
			t.Errorf("%s: requests total %v, want %v", c.key, g.DataPoints[0]["total"], want)
		}
		var gr groups
		if code := get("/v1/analytics/groups/workspace"+q, c.key, &gr); code != http.StatusOK {
			t.Fatalf("%s: workspace grouping status %d", c.key, code)
		}
		var seen []string
		for _, row := range gr.Data {
			seen = append(seen, fmt.Sprint(row["workspace"]))
		}
		if want := c.workspace; (want == "" && len(seen) != 0) || (want != "" && (len(seen) != 1 || seen[0] != want)) {
			t.Errorf("%s: workspace grouping lists %v, want %q", c.key, seen, want)
		}
	}
	if code := get("/v1/analytics/graphs/requests?"+window, "e2e-key-unknown", nil); code != http.StatusUnauthorized {
		t.Errorf("unknown key: status %d, want 401", code)
	}
	if code := get("/v1/analytics/graphs/requests?"+window+"&workspace_slug=e2e-ws-team-b", "e2e-key-team-a", nil); code != http.StatusBadRequest {
		t.Errorf("another key's workspace_slug: status %d, want 400", code)
	}
}
