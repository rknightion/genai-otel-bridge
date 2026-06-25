// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// rawRecord parses a JSONL line into the tolerant map the strip consumes.
func rawRecord(t *testing.T, line string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestStripAllowListsAndDropsContent is the load-bearing content-safety test: a real-shaped export
// record carrying the PII/config fields Portkey injects regardless of requested_data (metadata,
// portkeyHeaders) PLUS message-body fields — only the explicitly allow-listed content-free fields may
// survive, split into indexed (stream-label) vs record (structured-metadata) tiers. Everything else is
// dropped by default-deny. Body is never set.
func TestStripAllowListsAndDropsContent(t *testing.T) {
	line := `{"id":"r1","trace_id":"t1","created_at":"2026-06-18T14:23:45Z","response_time":1485,` +
		`"response_status_code":200,"ai_org":"org-acme","ai_model":"gpt-4.1-2025-04-14","cost":0.3194,` +
		`"total_units":1312,"currency":"USD",` +
		`"metadata":{"owner":"a person","ritm_number":"RITM123","data_classification":"C3"},` +
		`"portkeyHeaders":{"x-portkey-config":{"cache":{}}},` +
		`"prompt":"secret prompt","request":{"messages":[]},"response":{"choices":[]}}`
	lr := defaultLogFieldPolicy().strip(rawRecord(t, line), time.Unix(0, 0).UTC())

	// Indexed (→ stream labels via GS1): ai_org, ai_model, response_status_code only.
	wantIndexed := map[string]string{"ai_org": "org-acme", "ai_model": "gpt-4.1-2025-04-14", "response_status_code": "200"}
	if len(lr.IndexedAttributes) != len(wantIndexed) {
		t.Fatalf("indexed=%v want %v", lr.IndexedAttributes, wantIndexed)
	}
	for k, v := range wantIndexed {
		if lr.IndexedAttributes[k] != v {
			t.Fatalf("indexed[%s]=%q want %q", k, lr.IndexedAttributes[k], v)
		}
	}
	// Record (structured metadata): content-free operational fields, scalar-stringified.
	if lr.RecordAttributes["id"] != "r1" || lr.RecordAttributes["trace_id"] != "t1" ||
		lr.RecordAttributes["response_time"] != "1485" || lr.RecordAttributes["cost"] != "0.3194" {
		t.Fatalf("record attrs wrong: %v", lr.RecordAttributes)
	}
	// CONTENT / PII must be absent from BOTH tiers and the body.
	for _, banned := range []string{"metadata", "portkeyHeaders", "prompt", "request", "response", "data_classification", "owner", "ritm_number"} {
		if _, ok := lr.IndexedAttributes[banned]; ok {
			t.Fatalf("banned key %q leaked into IndexedAttributes", banned)
		}
		if _, ok := lr.RecordAttributes[banned]; ok {
			t.Fatalf("banned key %q leaked into RecordAttributes", banned)
		}
	}
	if lr.Body != "" {
		t.Fatalf("Body must stay empty (FR10), got %q", lr.Body)
	}
}

// TestStripDefaultsPromptIdentity guards the S12 per-prompt usage signal (followup §9): prompt_slug /
// prompt_version_id / prompt_id are content-free Portkey prompt identifiers (NOT prompt content) and ship
// by DEFAULT in the RECORD (structured-metadata) tier — so per-prompt cost/token/latency correlation works
// out of the box, no extra_record_fields opt-in needed. They are per-prompt context, not routing identity,
// so they stay in the record tier (never indexed/stream-label). defaultRequestedData() derives from this
// policy, so they're also asked of Portkey automatically. Field names confirmed against the live
// instance 2026-06-21 (prompt_slug + prompt_version_id + prompt_id populated together on saved-prompt
// requests; the earlier `prompt_version` guess is NOT a real column — see followup §9).
func TestStripDefaultsPromptIdentity(t *testing.T) {
	line := `{"prompt_slug":"summarise-ticket","prompt_version_id":"pv_3","prompt_id":"pp_9","ai_model":"gpt-4.1","prompt":"ZZSECRETZZ","trace_id":"t"}`
	lr := defaultLogFieldPolicy().strip(rawRecord(t, line), time.Unix(0, 0).UTC())
	if lr.RecordAttributes["prompt_slug"] != "summarise-ticket" || lr.RecordAttributes["prompt_version_id"] != "pv_3" ||
		lr.RecordAttributes["prompt_id"] != "pp_9" {
		t.Fatalf("prompt identity should ship in record tier by default, got %v", lr.RecordAttributes)
	}
	// Prompt identity is per-prompt context, NOT routing identity — must not be indexed (stream label).
	for _, k := range []string{"prompt_slug", "prompt_version_id", "prompt_id"} {
		if _, ok := lr.IndexedAttributes[k]; ok {
			t.Fatalf("%s must stay in the record tier, not indexed", k)
		}
	}
	// The bare prompt CONTENT field must still be dropped (content floor holds).
	if _, ok := lr.RecordAttributes["prompt"]; ok {
		t.Fatal("prompt content leaked into record attrs")
	}
	// defaultRequestedData() is derived from the policy, so it must now ask Portkey for the identifiers.
	rd := defaultRequestedData()
	for _, want := range []string{"prompt_slug", "prompt_version_id", "prompt_id"} {
		if !slices.Contains(rd, want) {
			t.Fatalf("defaultRequestedData missing %q: %v", want, rd)
		}
	}
}

// TestStripDropsNestedObjectsUnderAllowedKey: even if an allow-listed key unexpectedly holds an
// object/array (not a scalar), it is skipped — never stringified (defence against content hiding in a
// nested structure under a benign key name).
func TestStripDropsNestedObjectsUnderAllowedKey(t *testing.T) {
	line := `{"ai_model":{"nested":"obj"},"cost":[1,2,3],"trace_id":"ok"}`
	lr := defaultLogFieldPolicy().strip(rawRecord(t, line), time.Unix(0, 0).UTC())
	if _, ok := lr.IndexedAttributes["ai_model"]; ok {
		t.Fatal("ai_model holding an object must be skipped, not stringified")
	}
	if _, ok := lr.RecordAttributes["cost"]; ok {
		t.Fatal("cost holding an array must be skipped")
	}
	if lr.RecordAttributes["trace_id"] != "ok" {
		t.Fatal("scalar trace_id should still pass")
	}
}

// TestStripDropsNullScalar: a JSON null under an allow-listed key must be DROPPED, not emitted as an
// empty-string attribute (json.Unmarshal of "null" into a *string succeeds with "", so guard explicitly).
func TestStripDropsNullScalar(t *testing.T) {
	lr := defaultLogFieldPolicy().strip(rawRecord(t, `{"cost":null,"trace_id":"ok"}`), time.Unix(0, 0).UTC())
	if v, ok := lr.RecordAttributes["cost"]; ok {
		t.Fatalf("null cost must be dropped, got %q", v)
	}
	if lr.RecordAttributes["trace_id"] != "ok" {
		t.Fatal("a real scalar must still pass")
	}
}

// TestStripExtraRecordFields: an operator can opt extra content-free fields into the RECORD allow-list;
// the default content-free set is preserved and the message-body/PII fields are still dropped.
func TestStripExtraRecordFields(t *testing.T) {
	p := defaultLogFieldPolicy().withExtraRecordFields([]string{"cache_status", "mode"})
	lr := p.strip(rawRecord(t, `{"cache_status":"HIT","mode":"proxy","prompt":"ZZSECRETZZ","trace_id":"t","cost":0.5}`), time.Unix(0, 0).UTC())
	if lr.RecordAttributes["cache_status"] != "HIT" || lr.RecordAttributes["mode"] != "proxy" {
		t.Fatalf("opted-in fields should flow: %v", lr.RecordAttributes)
	}
	if _, ok := lr.RecordAttributes["prompt"]; ok {
		t.Fatal("prompt must still be dropped (not opted in)")
	}
	if lr.RecordAttributes["trace_id"] != "t" || lr.RecordAttributes["cost"] != "0.5" {
		t.Fatalf("default content-free fields must be preserved: %v", lr.RecordAttributes)
	}
}

// TestStripExtraIndexedFields: a content-free field opted into extra_indexed_fields is routed to the
// INDEXED tier (IndexedAttributes → Loki stream-label candidate), not the RECORD tier; defaults and the
// content drops are preserved.
func TestStripExtraIndexedFields(t *testing.T) {
	p := defaultLogFieldPolicy().withExtraIndexedFields([]string{"cache_status"})
	lr := p.strip(rawRecord(t, `{"cache_status":"HIT","ai_model":"m","prompt":"ZZSECRETZZ","trace_id":"t"}`), time.Unix(0, 0).UTC())
	if lr.IndexedAttributes["cache_status"] != "HIT" {
		t.Fatalf("cache_status should be promoted to IndexedAttributes, got indexed=%v record=%v", lr.IndexedAttributes, lr.RecordAttributes)
	}
	if _, ok := lr.RecordAttributes["cache_status"]; ok {
		t.Fatal("cache_status should be indexed-only, not also in RecordAttributes")
	}
	if lr.IndexedAttributes["ai_model"] != "m" {
		t.Fatal("default indexed attr ai_model must be preserved")
	}
	if _, ok := lr.RecordAttributes["prompt"]; ok {
		t.Fatal("prompt must still be dropped")
	}
}

// TestStripMetadataRecordFields: named scalar sub-keys of the (otherwise hard-denied) metadata object
// are lifted into RECORD attributes under their bare names, while every OTHER metadata sub-key (the
// PII) stays dropped. This is the only sanctioned path INTO the metadata blob — operator-named,
// content-free sub-keys only.
func TestStripMetadataRecordFields(t *testing.T) {
	p := defaultLogFieldPolicy().withMetadataFields([]string{"correlation_id"}, "")
	line := `{"trace_id":"t","metadata":{"correlation_id":"abc-123","owner":"ZZSECRETZZ","ritm_number":"RITM1"}}`
	lr := p.strip(rawRecord(t, line), time.Unix(0, 0).UTC())
	if lr.RecordAttributes["correlation_id"] != "abc-123" {
		t.Fatalf("correlation_id should lift into record attrs, got %v", lr.RecordAttributes)
	}
	// The rest of metadata (PII) must NOT leak, and the metadata blob itself must never appear.
	for _, banned := range []string{"owner", "ritm_number", "metadata"} {
		if _, ok := lr.RecordAttributes[banned]; ok {
			t.Fatalf("metadata PII %q leaked into record attrs", banned)
		}
		if _, ok := lr.IndexedAttributes[banned]; ok {
			t.Fatalf("metadata PII %q leaked into indexed attrs", banned)
		}
	}
}

// TestStripMetadataTraceID: a metadata sub-key designated as the trace-id source has its UUID value
// parsed into the OTLP LogRecord.TraceID (16 bytes) AND is still emitted as a content-free record attr
// (the raw value stays queryable). The trace-id field is auto-lifted even if not named in the record set.
func TestStripMetadataTraceID(t *testing.T) {
	p := defaultLogFieldPolicy().withMetadataFields(nil, "correlation_id")
	line := `{"trace_id":"t","metadata":{"correlation_id":"00112233-4455-6677-8899-aabbccddeeff"}}`
	lr := p.strip(rawRecord(t, line), time.Unix(0, 0).UTC())
	want := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	if !slices.Equal(lr.TraceID, want) {
		t.Fatalf("TraceID=%x want %x", lr.TraceID, want)
	}
	if lr.RecordAttributes["correlation_id"] != "00112233-4455-6677-8899-aabbccddeeff" {
		t.Fatalf("trace-id source should also be a record attr, got %v", lr.RecordAttributes)
	}
}

// TestStripMetadataTraceIDUnparseable: a non-UUID trace-id value leaves TraceID unset but the raw value
// is still preserved as a record attr (best-effort enrichment — never lose the data).
func TestStripMetadataTraceIDUnparseable(t *testing.T) {
	p := defaultLogFieldPolicy().withMetadataFields(nil, "correlation_id")
	lr := p.strip(rawRecord(t, `{"metadata":{"correlation_id":"not-a-uuid"}}`), time.Unix(0, 0).UTC())
	if lr.TraceID != nil {
		t.Fatalf("TraceID should be unset for a non-UUID value, got %x", lr.TraceID)
	}
	if lr.RecordAttributes["correlation_id"] != "not-a-uuid" {
		t.Fatalf("value should still be preserved as a record attr, got %v", lr.RecordAttributes)
	}
}

// TestStripMetadataDropsNonScalarAndHardDenied: a named metadata sub-key that is an object/array is
// skipped (never flattened); the strip never lifts a hard-denied content sub-key even if the value
// were somehow scalar (defence in depth beyond the config-time reject).
func TestStripMetadataDropsNonScalar(t *testing.T) {
	p := defaultLogFieldPolicy().withMetadataFields([]string{"correlation_id", "nested"}, "")
	lr := p.strip(rawRecord(t, `{"metadata":{"correlation_id":"ok","nested":{"a":"b"}}}`), time.Unix(0, 0).UTC())
	if lr.RecordAttributes["correlation_id"] != "ok" {
		t.Fatalf("scalar correlation_id should lift, got %v", lr.RecordAttributes)
	}
	if _, ok := lr.RecordAttributes["nested"]; ok {
		t.Fatal("a nested object under a named metadata sub-key must be skipped, not stringified")
	}
}

// TestStripMetadataAbsent: no metadata object (or a non-object metadata) is handled gracefully — no
// panic, no attr, no trace id.
func TestStripMetadataAbsent(t *testing.T) {
	p := defaultLogFieldPolicy().withMetadataFields([]string{"correlation_id"}, "correlation_id")
	lr := p.strip(rawRecord(t, `{"trace_id":"t"}`), time.Unix(0, 0).UTC())
	if _, ok := lr.RecordAttributes["correlation_id"]; ok {
		t.Fatal("no metadata object ⇒ no correlation_id attr")
	}
	if lr.TraceID != nil {
		t.Fatal("no metadata object ⇒ no trace id")
	}
}

// TestStripTopLevelTraceIDField: a TOP-LEVEL export field designated as the trace-id source (the
// Portkey-native trace_id path, complementing the metadata-sub-key path) has its UUID value parsed into
// the OTLP LogRecord.TraceID (16 bytes) AND still ships as a content-free record attr. A NON-default
// field name proves withTraceIDField auto-unions it into the record allow-list (else it would be dropped).
func TestStripTopLevelTraceIDField(t *testing.T) {
	p := defaultLogFieldPolicy().withTraceIDField("request_id")
	lr := p.strip(rawRecord(t, `{"request_id":"00112233-4455-6677-8899-aabbccddeeff","ai_model":"m"}`), time.Unix(0, 0).UTC())
	want := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	if !slices.Equal(lr.TraceID, want) {
		t.Fatalf("TraceID=%x want %x", lr.TraceID, want)
	}
	if lr.RecordAttributes["request_id"] != "00112233-4455-6677-8899-aabbccddeeff" {
		t.Fatalf("trace-id source field should also be a record attr, got %v", lr.RecordAttributes)
	}
}

// TestStripTopLevelTraceIDFieldUnparseable: a non-UUID top-level trace-id value leaves TraceID unset but
// the raw value still ships as a record attr (best-effort enrichment — never lose the data).
func TestStripTopLevelTraceIDFieldUnparseable(t *testing.T) {
	p := defaultLogFieldPolicy().withTraceIDField("trace_id")
	lr := p.strip(rawRecord(t, `{"trace_id":"not-a-uuid"}`), time.Unix(0, 0).UTC())
	if lr.TraceID != nil {
		t.Fatalf("TraceID should be unset for a non-UUID value, got %x", lr.TraceID)
	}
	if lr.RecordAttributes["trace_id"] != "not-a-uuid" {
		t.Fatalf("value should still be preserved as a record attr, got %v", lr.RecordAttributes)
	}
}

// TestStripTimestampFromCreatedAt: created_at parses to the record Timestamp; an unparseable value
// falls back to the supplied time (a valid timestamp is always produced).
func TestStripTimestampFromCreatedAt(t *testing.T) {
	fallback := time.Unix(999, 0).UTC()
	got := defaultLogFieldPolicy().strip(rawRecord(t, `{"created_at":"2026-06-18T14:23:45Z","ai_model":"m"}`), fallback)
	if !got.Timestamp.Equal(time.Date(2026, 6, 18, 14, 23, 45, 0, time.UTC)) {
		t.Fatalf("ts=%v want parsed created_at", got.Timestamp)
	}
	gotFallback := defaultLogFieldPolicy().strip(rawRecord(t, `{"created_at":"not a date","ai_model":"m"}`), fallback)
	if !gotFallback.Timestamp.Equal(fallback) {
		t.Fatalf("ts=%v want fallback %v", gotFallback.Timestamp, fallback)
	}
}

// TestStripStampsSourceAttribute asserts every stripped record carries the producer-identity
// `source` record attribute (= "portkey") so portkey vs langsmith log data is distinguishable in Loki
// (structured metadata; option B). Set unconditionally, even on a minimal record.
func TestStripStampsSourceAttribute(t *testing.T) {
	lr := defaultLogFieldPolicy().strip(rawRecord(t, `{"id":"r1"}`), time.Unix(0, 0).UTC())
	if got := lr.RecordAttributes["source"]; got != "portkey" {
		t.Fatalf(`RecordAttributes["source"]=%q want "portkey" (record=%v)`, got, lr.RecordAttributes)
	}
}
