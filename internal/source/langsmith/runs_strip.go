// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// runTimeLayouts: LangSmith 0.13.5 stamps NAIVE timestamps (no tz) with or without fractional seconds
// (live-probed, e.g. "2026-06-19T13:22:24.560930") — treat as UTC. The zoned forms are defensive (a
// different version might append Z/offset). The run's start_time IS the LogRecord timestamp (the runs
// loop is a forward-only log, unlike the sessions snapshot which stamps at poll-now).
var runTimeLayouts = []string{
	"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05",
	time.RFC3339Nano, time.RFC3339,
}

// parseRunTime best-effort parses a run timestamp value (naive UTC or zoned). ok=false on null/non-string/
// unparseable, so the caller falls back to a known-good timestamp (a valid ts is always set on a LogRecord).
func parseRunTime(raw json.RawMessage) (time.Time, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return time.Time{}, false
	}
	for _, layout := range runTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// severityFor maps a run status to an OTLP log severity. Content-free (status is a low-card enum).
func severityFor(status string) string {
	switch status {
	case "error", "timeout", "interrupted":
		return "ERROR"
	default:
		return "INFO"
	}
}

// runsFieldPolicy is the content-free default-deny ALLOW-LIST applied to every run record before a
// model.LogRecord is built. The live probe proved `select` does NOT TRIM content on 0.13.5 (the full
// field set returns regardless of select) — so this strip is the AUTHORITATIVE egress control: any key not in
// indexed∪record is DROPPED, so inputs/outputs/inputs_preview/outputs_preview/events/extra/serialized/
// messages/error/name and any unrecognised future field never reach a LogRecord. Pairs with
// source.Guard.SanitizeLogs (defence-in-depth chokepoint) + the content-leak conformance release gate.
type runsFieldPolicy struct {
	indexed map[string]bool // → LogRecord.IndexedAttributes (low-card; GS1 Loki stream labels)
	record  map[string]bool // → LogRecord.RecordAttributes (structured metadata; content-free)
}

// defaultRunsFieldPolicy is the content-free field set. Indexed = the low-card routing/query identity
// promoted to stream labels via GS1 (run_type/status). Record = content-free per-run context
// kept as structured metadata. High-cardinality per-record ids (id/trace_id/session_id/parent_run_id —
// session_id is a project UUID, 100+ on the real instance) are RECORD, NEVER indexed (model.LogRecord
// forbids high-card ids in IndexedAttributes). `name` is excluded (may embed content / ephemeral hashes).
func defaultRunsFieldPolicy() runsFieldPolicy {
	return runsFieldPolicy{
		indexed: set("run_type", "status"),
		record: set("id", "trace_id", "session_id", "parent_run_id", "start_time", "end_time",
			"first_token_time", "total_tokens", "prompt_tokens", "completion_tokens",
			"total_cost", "prompt_cost", "completion_cost", "dotted_order", "thread_id"),
	}
}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// withExtraRecordFields returns a copy of the policy with the operator-opted-in fields added to the
// RECORD (structured-metadata) allow-list. The content-free default set is preserved; the extras layer
// on. (The INDEXED-tier opt-in is withExtraIndexedFields below.)
func (p runsFieldPolicy) withExtraRecordFields(extra []string) runsFieldPolicy {
	if len(extra) == 0 {
		return p
	}
	rec := make(map[string]bool, len(p.record)+len(extra))
	for k := range p.record {
		rec[k] = true
	}
	for _, k := range extra {
		rec[k] = true
	}
	return runsFieldPolicy{indexed: p.indexed, record: rec}
}

// withExtraIndexedFields returns a copy with operator-opted-in fields added to the INDEXED (Loki
// stream-label) allow-list (settings.extra_indexed_fields). The default set is preserved; the extras
// layer on. strip checks `indexed` BEFORE `record`, so a key that is also a default record field is
// routed to indexed. Callers validate the extras exclude hard-denied content fields; the composition root
// auto-allow-lists these keys in the guard (so an indexed promotion can't be silently dropped). Now
// enabled because the guard AllowLabelKeys is config-driven (Lane 3); still GS1-gated to be queryable.
func (p runsFieldPolicy) withExtraIndexedFields(extra []string) runsFieldPolicy {
	if len(extra) == 0 {
		return p
	}
	idx := make(map[string]bool, len(p.indexed)+len(extra))
	for k := range p.indexed {
		idx[k] = true
	}
	for _, k := range extra {
		idx[k] = true
	}
	return runsFieldPolicy{indexed: idx, record: p.record}
}

// selectKeys is the content-free projection sent to runs/query: all allow-listed keys (indexed∪record),
// sorted for a deterministic request body across leaders. On 0.13.5 `select` does NOT trim content
// (probed — the strip is authoritative), but it IS enum-validated server-side: a value outside the
// server's accepted set 422s the WHOLE query (the 2026-06-21 execution_order outage), so every key here
// MUST be a valid select field (guarded by TestRunsSelectFieldsAreValidServerEnum). It mirrors the ACTIVE
// allow-list, so an opted-in extra_record_field is also requested.
func (p runsFieldPolicy) selectKeys() []string {
	out := make([]string, 0, len(p.indexed)+len(p.record))
	for k := range p.indexed {
		out = append(out, k)
	}
	for k := range p.record {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validLangsmithSelectEnum is the set of `select` values the live self-hosted LangSmith 0.13.5 accepts on
// POST /runs/query — captured verbatim from the server's 422 validation error (2026-06-21 live probe).
// `select` IS enum-validated server-side: ANY value outside this set 422s the WHOLE query, killing every
// run page (a single bad field takes down the entire runs loop). It is the SINGLE SOURCE OF TRUTH shared
// by two guards: (1) TestRunsSelectFieldsAreValidServerEnum pins the default+opt-in policy's projection
// to a subset of it, and (2) validateRunsSettings rejects an extra_record_fields/extra_indexed_fields
// opt-in outside it at config-load time (#65 — a typo would otherwise 422 every runs/query at runtime,
// a whole-loop outage from a one-character config error). Other LangSmith versions may accept more —
// refresh from a new 422 probe if the server changes.
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

// strip maps a raw run record to a content-free LogRecord. Body is NEVER set (FR10). Timestamp = parsed
// start_time (naive UTC), else fallbackTS. Severity derived from status. Nested objects/arrays under an
// allow-listed key (token/cost _details, feedback_stats, and any object/array value) are SKIPPED — never
// stringified, so content hidden under an allow-listed key can't leak.
func (p runsFieldPolicy) strip(raw map[string]json.RawMessage, fallbackTS time.Time) model.LogRecord {
	lr := model.LogRecord{Timestamp: fallbackTS}
	for k, v := range raw {
		var dst *map[string]string
		switch {
		case p.indexed[k]:
			dst = &lr.IndexedAttributes
		case p.record[k]:
			dst = &lr.RecordAttributes
		default:
			continue // default-deny: dropped (content/PII/unrecognised)
		}
		s, ok := scalarToString(v)
		if !ok {
			continue // object/array/null → skip, never flatten nested content
		}
		if *dst == nil {
			*dst = map[string]string{}
		}
		(*dst)[k] = s
	}
	if ts, ok := parseRunTime(raw["start_time"]); ok {
		lr.Timestamp = ts
	}
	lr.Severity = severityFor(lr.IndexedAttributes["status"])
	stampSource(&lr)
	return lr
}

// sourceAttrKey / sourceAttrValue: producer-identity record attribute stamped on every emitted log so
// portkey vs langsmith data is distinguishable downstream (Loki structured metadata). Content-free
// constant (the vendor type), not derived from record data. Mirrors the portkey logs_export strip.
const (
	sourceAttrKey   = "source"
	sourceAttrValue = "langsmith"
)

func stampSource(lr *model.LogRecord) {
	if lr.RecordAttributes == nil {
		lr.RecordAttributes = map[string]string{}
	}
	lr.RecordAttributes[sourceAttrKey] = sourceAttrValue
}

// scalarToString renders an allow-listed value as a string. A JSON scalar (string/number/bool) renders
// directly; a JSON ARRAY OF SCALARS renders as a comma-joined string (so operational arrays like
// tags/child_run_ids/parent_run_ids are opt-in-able via extra_record_fields). Anything else — a bare
// object, an array carrying a non-scalar element (object/nested array/null), an empty array, or JSON
// null — returns ok=false so the caller drops it: nested content is NEVER flattened into an attribute.
func scalarToString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var elems []json.RawMessage
		if json.Unmarshal(trimmed, &elems) != nil || len(elems) == 0 {
			return "", false // unparseable or empty array → drop (no misleading "")
		}
		parts := make([]string, 0, len(elems))
		for _, e := range elems {
			s, ok := scalarValue(e)
			if !ok {
				return "", false // a non-scalar element (object/nested array/null) → drop the WHOLE field
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ","), true
	}
	return scalarValue(trimmed)
}

// scalarValue renders a single JSON scalar (string/number/bool) as a string. `cost` returns as a number
// OR a quoted string across versions — both are scalars here (we emit the raw string; logs need no
// arithmetic, so no `money` parse). JSON null and any non-scalar (object/array) return ok=false.
func scalarValue(raw json.RawMessage) (string, bool) {
	// JSON null = absent → drop (unmarshalling null into a *string succeeds with "", so guard explicitly;
	// otherwise a null end_time/first_token_time/cost — or a null array element — would emit an empty string).
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatFloat(f, 'f', -1, 64), true
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return strconv.FormatBool(b), true
	}
	return "", false
}
