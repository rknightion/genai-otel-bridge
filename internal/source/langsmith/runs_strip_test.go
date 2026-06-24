// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseRunTime(t *testing.T) {
	cases := []struct {
		in   string
		want string // RFC3339 in UTC, or "" for not-ok
	}{
		{`"2026-06-19T13:22:24.560930"`, "2026-06-19T13:22:24Z"},  // naive + fractional (real 0.13.5 shape)
		{`"2026-06-19T13:22:24"`, "2026-06-19T13:22:24Z"},         // naive, no fractional
		{`"2026-06-19T13:22:24.560930Z"`, "2026-06-19T13:22:24Z"}, // already-zoned (defensive)
		{`null`, ""},
		{`"not-a-time"`, ""},
		{`123`, ""},
	}
	for _, c := range cases {
		got, ok := parseRunTime(json.RawMessage(c.in))
		if c.want == "" {
			if ok {
				t.Errorf("%s: want not-ok, got %v", c.in, got)
			}
			continue
		}
		if !ok || got.UTC().Format(time.RFC3339) != c.want {
			t.Errorf("%s: got %v ok=%v, want %s", c.in, got, ok, c.want)
		}
	}
}

func TestSeverityFor(t *testing.T) {
	for in, want := range map[string]string{
		"success": "INFO", "pending": "INFO", "running": "INFO",
		"error": "ERROR", "timeout": "ERROR", "interrupted": "ERROR", "": "INFO",
	} {
		if got := severityFor(in); got != want {
			t.Errorf("severityFor(%q)=%q want %q", in, got, want)
		}
	}
}

// validLangsmithSelectEnum is the set of `select` values the live self-hosted LangSmith 0.13.5 accepts on
// POST /runs/query — captured verbatim from the server's 422 validation error (2026-06-21 live probe).
// `select` IS enum-validated server-side: ANY value outside this set 422s the WHOLE query, killing every
// run page (a single bad field takes down the entire runs loop). So the loop's select projection MUST be
// a subset of this. Other LangSmith versions may differ — refresh from a new 422 probe if the server changes.
var validLangsmithSelectEnum = set(
	"id", "name", "run_type", "start_time", "end_time", "status", "error", "extra", "events", "inputs",
	"inputs_preview", "inputs_s3_urls", "inputs_or_signed_url", "outputs", "outputs_preview", "outputs_s3_urls",
	"outputs_or_signed_url", "s3_urls", "error_or_signed_url", "events_or_signed_url", "extra_or_signed_url",
	"serialized_or_signed_url", "parent_run_id", "manifest_id", "manifest_s3_id", "manifest", "session_id",
	"serialized", "reference_example_id", "reference_dataset_id", "total_tokens", "prompt_tokens",
	"prompt_token_details", "completion_tokens", "completion_token_details", "total_cost", "prompt_cost",
	"prompt_cost_details", "completion_cost", "completion_cost_details", "price_model_id", "first_token_time",
	"trace_id", "dotted_order", "last_queued_at", "feedback_stats", "child_run_ids", "parent_run_ids", "tags",
	"in_dataset", "app_path", "share_token", "trace_tier", "trace_first_received_at", "ttl_seconds",
	"trace_upgrade", "thread_id", "trace_min_max_start_time", "messages", "inserted_at",
)

// TestRunsSelectFieldsAreValidServerEnum guards against a select projection field the server rejects:
// any such field 422s the entire runs/query (regression guard for the execution_order outage, 2026-06-21).
func TestRunsSelectFieldsAreValidServerEnum(t *testing.T) {
	policies := map[string]runsFieldPolicy{
		"default":            defaultRunsFieldPolicy(),
		"with_extra_record":  defaultRunsFieldPolicy().withExtraRecordFields([]string{"tags", "child_run_ids", "app_path"}),
		"with_extra_indexed": defaultRunsFieldPolicy().withExtraIndexedFields([]string{"app_path"}),
	}
	for name, p := range policies {
		for _, f := range p.selectKeys() {
			if !validLangsmithSelectEnum[f] {
				t.Errorf("%s: select field %q is NOT in the LangSmith 0.13.5 select enum — runs/query will 422 the whole query", name, f)
			}
		}
	}
}

// fmtAttrs renders an attr map deterministically for content-leak scanning.
func fmtAttrs(m map[string]string) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
		b.WriteString(";")
	}
	return b.String()
}

// TestRunsWithExtraIndexedFields: a content-free field opted into extra_indexed_fields is routed to
// IndexedAttributes (not RecordAttributes) by the strip — the indexed-tier promotion. app_path is not in
// the default sets, so absent the opt-in it would be dropped; with it, it lands indexed.
func TestRunsWithExtraIndexedFields(t *testing.T) {
	p := defaultRunsFieldPolicy().withExtraIndexedFields([]string{"app_path"})
	raw := map[string]json.RawMessage{
		"run_type":   json.RawMessage(`"llm"`),
		"app_path":   json.RawMessage(`"/team/app"`),
		"start_time": json.RawMessage(`"2026-06-19T13:22:24"`),
	}
	lr := p.strip(raw, time.Unix(0, 0).UTC())
	if lr.IndexedAttributes["app_path"] != "/team/app" {
		t.Fatalf("app_path should be promoted to IndexedAttributes, got indexed=%v record=%v", lr.IndexedAttributes, lr.RecordAttributes)
	}
	if _, ok := lr.RecordAttributes["app_path"]; ok {
		t.Fatalf("app_path should be indexed-only, not also in RecordAttributes")
	}
}

func TestRunsStripContentSafe(t *testing.T) {
	raw := map[string]json.RawMessage{
		// operational (allow-listed)
		"id":           json.RawMessage(`"r1"`),
		"trace_id":     json.RawMessage(`"t1"`),
		"session_id":   json.RawMessage(`"proj-1"`),
		"run_type":     json.RawMessage(`"llm"`),
		"status":       json.RawMessage(`"success"`),
		"trace_tier":   json.RawMessage(`"longlived"`),
		"start_time":   json.RawMessage(`"2026-06-19T13:22:24.560930"`),
		"total_tokens": json.RawMessage(`1312`),
		"total_cost":   json.RawMessage(`0.0194`),
		// CONTENT (must be dropped — present-with-content)
		"inputs":               json.RawMessage(`{"messages":["ZZPROMPTZZ"]}`),
		"outputs":              json.RawMessage(`{"choices":["ZZCOMPLETIONZZ"]}`),
		"inputs_preview":       json.RawMessage(`"ZZPREVIEWZZ"`),
		"events":               json.RawMessage(`[{"x":"ZZEVENTZZ"}]`),
		"extra":                json.RawMessage(`{"k":"ZZEXTRAZZ"}`),
		"serialized":           json.RawMessage(`{"k":"ZZSERIALZZ"}`),
		"messages":             json.RawMessage(`["ZZMSGZZ"]`),
		"error":                json.RawMessage(`"ZZERRORTEXTZZ"`), // free text → content → dropped
		"name":                 json.RawMessage(`"ZZRUNNAMEZZ"`),   // excluded by default (may embed content)
		"prompt_token_details": json.RawMessage(`{"cached":10}`),   // nested object → skipped
		"feedback_stats":       json.RawMessage(`{"acc":{"avg":0.9,"values":{"ZZIDZZ":1}}}`),
	}
	lr := defaultRunsFieldPolicy().strip(raw, time.Unix(0, 0).UTC())

	if lr.Body != "" {
		t.Fatalf("Body must be empty, got %q", lr.Body)
	}
	if lr.IndexedAttributes["run_type"] != "llm" || lr.IndexedAttributes["status"] != "success" {
		t.Fatalf("indexed attrs wrong: %v", lr.IndexedAttributes)
	}
	// trace_tier was dropped from the default indexed set (NULL at scale, reclaims a Loki stream-label
	// slot — followup.md). It is in neither the indexed nor the record allow-list, so it is dropped.
	if _, ok := lr.IndexedAttributes["trace_tier"]; ok {
		t.Fatalf("trace_tier must NOT be indexed (dropped from default set): %v", lr.IndexedAttributes)
	}
	if _, ok := lr.RecordAttributes["trace_tier"]; ok {
		t.Fatalf("trace_tier must NOT be a record attr (fully dropped): %v", lr.RecordAttributes)
	}
	for _, hi := range []string{"trace_id", "id", "session_id"} {
		if _, ok := lr.IndexedAttributes[hi]; ok {
			t.Fatalf("%s must NOT be indexed (high-card)", hi)
		}
	}
	if lr.RecordAttributes["id"] != "r1" || lr.RecordAttributes["total_tokens"] != "1312" || lr.RecordAttributes["session_id"] != "proj-1" {
		t.Fatalf("record attrs wrong: %v", lr.RecordAttributes)
	}
	if lr.RecordAttributes["total_cost"] != "0.0194" {
		t.Fatalf("total_cost (number) should survive as a record string: %q", lr.RecordAttributes["total_cost"])
	}
	all := fmtAttrs(lr.IndexedAttributes) + fmtAttrs(lr.RecordAttributes) + lr.Body
	for _, marker := range []string{"ZZPROMPTZZ", "ZZCOMPLETIONZZ", "ZZPREVIEWZZ", "ZZEVENTZZ",
		"ZZEXTRAZZ", "ZZSERIALZZ", "ZZMSGZZ", "ZZERRORTEXTZZ", "ZZRUNNAMEZZ", "ZZIDZZ"} {
		if strings.Contains(all, marker) {
			t.Fatalf("CONTENT LEAK: %q survived the strip", marker)
		}
	}
	if lr.Timestamp.UTC().Format(time.RFC3339) != "2026-06-19T13:22:24Z" {
		t.Fatalf("timestamp not parsed from start_time: %v", lr.Timestamp)
	}
	if lr.Severity != "INFO" {
		t.Fatalf("severity=%q want INFO", lr.Severity)
	}
}

// TestRunsStripCostVariants: cost is a JSON number on 0.13.5 but a quoted string on other versions, and
// null when absent — the scalar strip handles all three (number/string → raw string; null/object → dropped).
func TestRunsStripCostVariants(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    string
		present bool
	}{
		{`0.0194`, "0.0194", true},   // number (0.13.5)
		{`"0.0194"`, "0.0194", true}, // quoted string (other versions)
		{`null`, "", false},          // absent → dropped (no misleading 0)
		{`{"x":1}`, "", false},       // object → dropped
	} {
		lr := defaultRunsFieldPolicy().strip(map[string]json.RawMessage{
			"run_type": json.RawMessage(`"llm"`), "total_cost": json.RawMessage(tc.raw),
		}, time.Unix(0, 0).UTC())
		got, ok := lr.RecordAttributes["total_cost"]
		if ok != tc.present || (tc.present && got != tc.want) {
			t.Fatalf("cost %s: got %q present=%v, want %q present=%v", tc.raw, got, ok, tc.want, tc.present)
		}
	}
}

// TestRunsStripArrayToCSV: a JSON array of SCALARS under an allow-listed key emits as a comma-joined
// string (so operational arrays like tags/child_run_ids/parent_run_ids can be opted in via
// extra_record_fields); any array carrying a non-scalar element (object/nested array/null), an empty
// array, or a bare object is DROPPED — nested content is never flattened into an attribute.
func TestRunsStripArrayToCSV(t *testing.T) {
	p := runsFieldPolicy{
		indexed: set("run_type"),
		record:  set("tags", "child_run_ids", "scores", "flags", "mixed", "empty", "nested", "withnull"),
	}
	raw := map[string]json.RawMessage{
		"run_type":      json.RawMessage(`"llm"`),
		"tags":          json.RawMessage(`["prod","eu","v2"]`),    // string array → csv
		"child_run_ids": json.RawMessage(`["a","b"]`),             // uuid-ish array → csv
		"scores":        json.RawMessage(`[1,2.5,3]`),             // number array → csv
		"flags":         json.RawMessage(`[true,false]`),          // bool array → csv
		"mixed":         json.RawMessage(`["a",{"x":"ZZOBJZZ"}]`), // object element → drop WHOLE field
		"empty":         json.RawMessage(`[]`),                    // empty → drop (no misleading "")
		"nested":        json.RawMessage(`[[1,2],[3]]`),           // nested array element → drop
		"withnull":      json.RawMessage(`["a",null,"b"]`),        // null element → drop WHOLE field
	}
	lr := p.strip(raw, time.Unix(0, 0).UTC())
	for k, want := range map[string]string{
		"tags": "prod,eu,v2", "child_run_ids": "a,b", "scores": "1,2.5,3", "flags": "true,false",
	} {
		if lr.RecordAttributes[k] != want {
			t.Errorf("%s: got %q want %q", k, lr.RecordAttributes[k], want)
		}
	}
	for _, k := range []string{"mixed", "empty", "nested", "withnull"} {
		if v, ok := lr.RecordAttributes[k]; ok {
			t.Errorf("%s must be dropped (non-scalar/empty array), got %q", k, v)
		}
	}
	if strings.Contains(fmtAttrs(lr.RecordAttributes), "ZZOBJZZ") {
		t.Fatal("CONTENT LEAK: nested object content survived an array→csv flatten")
	}
}

func TestSeverityFromErrorStatus(t *testing.T) {
	lr := defaultRunsFieldPolicy().strip(map[string]json.RawMessage{
		"run_type": json.RawMessage(`"llm"`), "status": json.RawMessage(`"error"`),
	}, time.Unix(0, 0).UTC())
	if lr.Severity != "ERROR" {
		t.Fatalf("status=error must map to severity ERROR, got %q", lr.Severity)
	}
}

// TestStripStampsSourceAttribute asserts every stripped run carries the producer-identity `source`
// record attribute (= "langsmith") so portkey vs langsmith log data is distinguishable in Loki
// (structured metadata; option B). Set unconditionally, even on a minimal record.
func TestStripStampsSourceAttribute(t *testing.T) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"id":"r1","status":"success"}`), &raw); err != nil {
		t.Fatal(err)
	}
	lr := defaultRunsFieldPolicy().strip(raw, time.Unix(0, 0).UTC())
	if got := lr.RecordAttributes["source"]; got != "langsmith" {
		t.Fatalf(`RecordAttributes["source"]=%q want "langsmith" (record=%v)`, got, lr.RecordAttributes)
	}
}
