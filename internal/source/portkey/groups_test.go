// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// realAIModels24h is a verbatim capture of GET /v1/analytics/groups/ai-models over 24h (groups PoC,
// 2026-06-19). Used to assert the decoder handles the ACTUAL wire shape: flat window-total rows, no
// timestamp/data_points, float costs, and NO is_quota_exceeded field (ai-models omits it — the decoder
// must treat it as absent⇒false, not required).
const realAIModels24h = `{"object":"list","total":14,"data":[` +
	`{"ai_model":"gpt-5.4-2026-03-05","requests":3706,"cost":6619.191450000001,"object":"analytics-group"},` +
	`{"ai_model":"text-embedding-3-small","requests":2920,"cost":0.3565380000000011,"object":"analytics-group"},` +
	`{"ai_model":"gpt-4.1-2025-04-14","requests":1888,"cost":1708.6768000000002,"object":"analytics-group"},` +
	`{"ai_model":"gpt-5-2025-08-07","requests":1439,"cost":4366.091225000002,"object":"analytics-group"},` +
	`{"ai_model":"gpt-5.5-2026-04-24","requests":600,"cost":0,"object":"analytics-group"}` +
	`]}`

// realUsers24h has the OTHER envelope variant: is_quota_exceeded present, dim field "user" with an
// empty value. Proves the tolerant row parser extracts the lone non-reserved field regardless of name.
const realUsers24h = `{"object":"list","total":1,"is_quota_exceeded":false,"data":[` +
	`{"user":"","requests":12616,"cost":14888.057116999998,"object":"analytics-group"}]}`

func TestGroupsDecodeRealWireShape(t *testing.T) {
	var resp groupsResponse
	if err := json.Unmarshal([]byte(realAIModels24h), &resp); err != nil {
		t.Fatalf("decode ai-models: %v", err)
	}
	if resp.Total != 14 {
		t.Fatalf("total=%d want 14", resp.Total)
	}
	if resp.IsQuotaExceeded {
		t.Fatal("ai-models has no is_quota_exceeded field → must decode to false")
	}
	if len(resp.Data) != 5 {
		t.Fatalf("rows=%d want 5", len(resp.Data))
	}
	rows, err := parseGroupRows(resp.Data, "ai_model")
	if err != nil {
		t.Fatalf("parseGroupRows: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("parsed rows=%d want 5", len(rows))
	}
	if rows[0].dimValue != "gpt-5.4-2026-03-05" || rows[0].requests != 3706 {
		t.Fatalf("row0=%+v want gpt-5.4/3706", rows[0])
	}
	if !rows[0].hasCost || rows[0].cost != 6619.191450000001 {
		t.Fatalf("row0 cost=%v hasCost=%v want 6619.19.../true", rows[0].cost, rows[0].hasCost)
	}
	// A real zero cost is still a present cost (gpt-5.5 cost:0).
	if !rows[4].hasCost || rows[4].cost != 0 {
		t.Fatalf("row4 (zero cost) must be hasCost=true value 0, got %v/%v", rows[4].cost, rows[4].hasCost)
	}
}

func TestGroupsParseTolerantDimField(t *testing.T) {
	var resp groupsResponse
	if err := json.Unmarshal([]byte(realUsers24h), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.IsQuotaExceeded == false { // present-and-false
		t.Fatal("users envelope is_quota_exceeded should be false")
	}
	rows, err := parseGroupRows(resp.Data, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].dimValue != "" || rows[0].requests != 12616 {
		t.Fatalf("tolerant parse of 'user' field failed: %+v", rows)
	}
}

// TestParseGroupRowsMetadataRealShape proves that the real metadata row shape is decoded correctly.
// A real metadata row carries extra stat fields (avg_tokens, avg_weighted_feedback, last_seen,
// requests_with_feedback) that the old "lone non-reserved field" heuristic would mis-pick; the
// explicit dimField="metadata_value" reads the correct dimension value regardless.
func TestParseGroupRowsMetadataRealShape(t *testing.T) {
	data := []map[string]json.RawMessage{{
		"metadata_value":         json.RawMessage(`"team-a"`),
		"requests":               json.RawMessage(`42`),
		"cost":                   json.RawMessage(`1234.5`),
		"object":                 json.RawMessage(`"analytics-group"`),
		"avg_tokens":             json.RawMessage(`123.4`),
		"avg_weighted_feedback":  json.RawMessage(`null`),
		"last_seen":              json.RawMessage(`"2026-06-20T00:00:00Z"`),
		"requests_with_feedback": json.RawMessage(`7`),
	}}
	rows, err := parseGroupRows(data, "metadata_value")
	if err != nil {
		t.Fatalf("parseGroupRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.dimValue != "team-a" {
		t.Fatalf("dimValue=%q want %q", r.dimValue, "team-a")
	}
	if r.requests != 42 {
		t.Fatalf("requests=%v want 42", r.requests)
	}
	if !r.hasCost || r.cost != 1234.5 {
		t.Fatalf("hasCost=%v cost=%v want hasCost=true cost=1234.5", r.hasCost, r.cost)
	}
}

func TestGroupsDeriveByModel(t *testing.T) {
	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	rows := []groupRow{
		{dimValue: "gpt-5", requests: 100, cost: 12.5, hasCost: true},
		{dimValue: "claude", requests: 50, cost: 9, hasCost: true},
	}
	// emitCost=false ⇒ only requests gauges, NO cost.
	got := deriveGroups(rows, "portkey_api", "model", nil, "ai_model", false, at)
	if len(got) != 2 {
		t.Fatalf("samples=%d want 2 (requests only, cost off)", len(got))
	}
	for _, s := range got {
		if s.Name != "portkey_api_requests_by_model" {
			t.Fatalf("name=%s want portkey_api_requests_by_model", s.Name)
		}
		if s.Kind != model.Gauge {
			t.Fatalf("kind=%v want Gauge", s.Kind)
		}
		if !s.Timestamp.Equal(at) {
			t.Fatalf("ts=%v want %v (window upper bound)", s.Timestamp, at)
		}
		if s.Labels["ai_model"] == "" {
			t.Fatalf("missing ai_model label: %+v", s.Labels)
		}
	}
	if got[0].Value != 100 || got[0].Labels["ai_model"] != "gpt-5" {
		t.Fatalf("row0 sample wrong: %+v", got[0])
	}
}

func TestGroupsDeriveByModelWithCost(t *testing.T) {
	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	rows := []groupRow{{dimValue: "gpt-5", requests: 100, cost: 12.5, hasCost: true}}
	got := deriveGroups(rows, "portkey_api", "model", nil, "ai_model", true, at)
	if len(got) != 2 {
		t.Fatalf("samples=%d want 2 (requests + cost)", len(got))
	}
	var sawReq, sawCost bool
	for _, s := range got {
		switch s.Name {
		case "portkey_api_requests_by_model":
			sawReq = true
		case "portkey_api_cost_usd_by_model": // Portkey cents → ÷100 → USD dollars
			sawCost = true
			if s.Value != 0.125 {
				t.Fatalf("cost value=%v want 0.125 (12.5 cents ÷ 100)", s.Value)
			}
		default:
			t.Fatalf("unexpected metric name %q", s.Name)
		}
	}
	if !sawReq || !sawCost {
		t.Fatalf("want both requests+cost gauges, sawReq=%v sawCost=%v", sawReq, sawCost)
	}
}

func TestGroupsDeriveByMetadata(t *testing.T) {
	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	rows := []groupRow{{dimValue: "chatbot", requests: 7, hasCost: false}}
	base := map[string]string{"metadata_key": "use_case"}
	got := deriveGroups(rows, "portkey_api", "metadata", base, "metadata_value", false, at)
	if len(got) != 1 {
		t.Fatalf("samples=%d want 1", len(got))
	}
	s := got[0]
	if s.Name != "portkey_api_requests_by_metadata" {
		t.Fatalf("name=%s", s.Name)
	}
	if s.Labels["metadata_key"] != "use_case" || s.Labels["metadata_value"] != "chatbot" {
		t.Fatalf("labels=%+v want {metadata_key:use_case, metadata_value:chatbot}", s.Labels)
	}
	// A row with no cost number must NOT emit a cost gauge even if emitCost were true.
	got2 := deriveGroups(rows, "portkey_api", "metadata", base, "metadata_value", true, at)
	if len(got2) != 1 {
		t.Fatalf("no-cost row must not emit a cost gauge even with emitCost=true, got %d samples", len(got2))
	}
}

// deriveGroups must not alias the shared base-label map across samples (each sample owns its labels).
func TestGroupsDeriveLabelsNotAliased(t *testing.T) {
	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	rows := []groupRow{
		{dimValue: "a", requests: 1, cost: 1, hasCost: true},
		{dimValue: "b", requests: 2, cost: 2, hasCost: true},
	}
	base := map[string]string{"metadata_key": "use_case"}
	got := deriveGroups(rows, "portkey_api", "metadata", base, "metadata_value", true, at)
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.Labels["metadata_value"]] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("labels aliased — distinct metadata_value lost: %v", seen)
	}
}
