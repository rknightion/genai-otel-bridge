// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
)

// fieldPolicy is the content-free ALLOW-LIST applied to every downloaded export record before a
// model.LogRecord is built. This is the mandatory client-side strip the logs-export PoC (§3) proved is
// required: `requested_data` is NOT an egress filter — Portkey injects `metadata` (customer PII: owner
// names, ticket ids, data_classification) and `portkeyHeaders` (gateway config) into the payload
// regardless of what was requested. Default-deny: any key not in indexed∪record is DROPPED, so
// metadata/portkeyHeaders/prompt/request/response and any unrecognised future field never reach a
// LogRecord. Pairs with source.Guard.SanitizeLogs (defence-in-depth chokepoint) + the content-leak
// conformance release gate (Cdx-C7).
type fieldPolicy struct {
	indexed map[string]bool // → LogRecord.IndexedAttributes (OTLP resource attrs → Loki stream labels via GS1)
	record  map[string]bool // → LogRecord.RecordAttributes (OTLP log attrs → Loki structured metadata)
	// useCase is the resolved api_key_use_case slug for the fan-out instance that owns this policy. Set
	// AFTER the withExtra*/withMetadataFields chain completes (those builders return fresh literals that
	// do not copy this field). Zero value ⇒ stampUseCaseRecord is a no-op (legacy/unlabelled path).
	useCase string
	// metadataRecord names sub-keys to LIFT OUT of the otherwise hard-denied `metadata` object into RECORD
	// attributes (bare name). This is the ONLY sanctioned path into `metadata`: only operator-named,
	// content-free sub-keys (e.g. a correlation_id) are extracted; every other sub-key (owner names,
	// ticket numbers, data classification) stays dropped. Empty ⇒ metadata is dropped wholesale as before.
	metadataRecord map[string]bool
	// metadataTraceID, if set, names the ONE metadata sub-key whose value is also parsed as a UUID and
	// placed on LogRecord.TraceID (OTLP trace_id) for logs↔traces correlation. It is auto-lifted into the
	// record tier too, so the raw value is always queryable even when only the trace mapping is configured.
	metadataTraceID string
}

// defaultLogFieldPolicy is the §3 content-free field set. Indexed = the low-cardinality routing
// identity promoted to stream labels (GS1: ai_org, ai_model, response_status_code). Record =
// content-free per-record context kept as structured metadata. `id`/`trace_id` are per-record and high
// cardinality → record (structured metadata), NEVER indexed (§8). Everything else is dropped.
//
// Field names CONFIRMED against the live instance 2026-06-21 (followup §9 probe), replacing the
// earlier schema guesses that don't exist on this instance:
//   - ai_org (3 distinct, 100% populated — a low-cardinality provider/org identity) replaces the dead
//     `ai_provider` as the indexed routing label.
//   - prompt_version_id + prompt_id replace the dead `prompt_version` (real saved-prompt identifiers;
//     populated alongside prompt_slug on the same requests — the S12 per-prompt usage signal).
//   - dropped `status_code` / `request_tokens` / `response_tokens` / `currency`: not real columns here
//     (response_status_code + total_units/req_units/res_units carry that data; cost has no currency field).
//
// A name that is wrong on some OTHER Portkey deployment is still harmless — the default-deny strip drops
// the unknown key and requested_data silently ignores it (no leak, no error).
//
// prompt_slug / prompt_version_id / prompt_id are content-free Portkey PROMPT IDENTIFIERS (the saved-prompt
// slug + its version + the prompt id — NOT prompt content) carrying the S12 per-prompt usage signal
// (followup §9): they ship by default in the RECORD tier so per-prompt cost/token/latency correlation
// works without an extra_record_fields opt-in. Record (not indexed): per-prompt context, not routing
// identity, and prompt_slug/prompt_id are higher cardinality than a stream label should carry.
func defaultLogFieldPolicy() fieldPolicy {
	return fieldPolicy{
		indexed: set("ai_org", "ai_model", "response_status_code"),
		record: set("id", "trace_id", "created_at", "response_time",
			"total_units", "req_units", "res_units", "cost",
			"prompt_slug", "prompt_version_id", "prompt_id"),
	}
}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// withExtraRecordFields returns a copy of the policy with operator-opted-in fields added to the RECORD
// (structured-metadata) allow-list (settings.extra_record_fields). The content-free default set is
// preserved; the extras layer on. (The INDEXED-tier opt-in is withExtraIndexedFields below.) Mirrors the
// langsmith runs strip. NOTE: unlike the langsmith strip, this one does NOT render arrays as csv — no
// Portkey export field is a scalar array worth opting in, and arrays under an allow-listed key stay
// DROPPED (defensive: never flatten).
func (p fieldPolicy) withExtraRecordFields(extra []string) fieldPolicy {
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
	return fieldPolicy{indexed: p.indexed, record: rec}
}

// withExtraIndexedFields returns a copy with operator-opted-in fields added to the INDEXED (Loki
// stream-label) allow-list (settings.extra_indexed_fields). The default set is preserved; the extras
// layer on. strip checks `indexed` BEFORE `record`, so a key that is also a default record field is
// routed to indexed. Callers validate the extras exclude hard-denied content fields; the composition root
// auto-allow-lists these keys in the guard (so an indexed promotion can't be silently dropped). Enabled
// now that the guard AllowLabelKeys is config-driven (Lane 3); still GS1-gated to be queryable as a label.
func (p fieldPolicy) withExtraIndexedFields(extra []string) fieldPolicy {
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
	return fieldPolicy{indexed: idx, record: p.record}
}

// withMetadataFields returns a copy of the policy that lifts named sub-keys out of the `metadata` object
// (settings.metadata_record_fields) into the RECORD tier, and optionally maps one sub-key's UUID to
// LogRecord.TraceID (settings.metadata_trace_id_field). The trace-id field is auto-unioned into the
// record set so its raw value is always emitted as a content-free attr too. Callers validate both
// against hardDeniedLogFields; strip enforces the floor again (defence in depth) and only ever lifts
// scalars. Empty args ⇒ the policy is unchanged (metadata stays dropped wholesale).
func (p fieldPolicy) withMetadataFields(record []string, traceID string) fieldPolicy {
	if len(record) == 0 && traceID == "" {
		return p
	}
	mr := make(map[string]bool, len(record)+1)
	for _, k := range record {
		mr[k] = true
	}
	if traceID != "" {
		mr[traceID] = true // always emit the trace-id value as a record attr too
	}
	return fieldPolicy{indexed: p.indexed, record: p.record, metadataRecord: mr, metadataTraceID: traceID}
}

// strip maps a raw export record to a content-free LogRecord. Body is never set (FR10). The Timestamp
// is parsed from created_at when possible, else the supplied fallback (a valid timestamp is always set).
func (p fieldPolicy) strip(raw map[string]json.RawMessage, fallbackTS time.Time) model.LogRecord {
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
			continue // not a scalar (object/array/null) — skip, never stringify nested content
		}
		if *dst == nil {
			*dst = map[string]string{}
		}
		(*dst)[k] = s
	}
	p.liftMetadata(raw["metadata"], &lr)
	if v, ok := raw["created_at"]; ok {
		if ts, ok := parseExportTime(v); ok {
			lr.Timestamp = ts
		}
	}
	stampSource(&lr)
	stampUseCaseRecord(&lr, p.useCase)
	return lr
}

// liftMetadata extracts the operator-named sub-keys from the (otherwise dropped) `metadata` object into
// record attributes, and parses the designated trace-id sub-key into lr.TraceID. Only scalar, non-hard-
// denied sub-keys are lifted; a non-object/absent metadata value is a no-op. A top-level attr of the
// same name is never clobbered. A non-parseable trace-id value leaves TraceID unset but the raw value
// still ships as a record attr (best-effort enrichment — the data is never lost).
func (p fieldPolicy) liftMetadata(metaRaw json.RawMessage, lr *model.LogRecord) {
	if len(p.metadataRecord) == 0 || len(metaRaw) == 0 {
		return
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(metaRaw, &meta) != nil {
		return // metadata absent / null / not an object — nothing to lift
	}
	for k := range p.metadataRecord {
		if hardDeniedLogFields[k] {
			continue // never lift a hard-denied content sub-key (defence beyond the config-time reject)
		}
		s, ok := scalarToString(meta[k])
		if !ok {
			continue // absent / non-scalar — skip, never flatten nested content
		}
		if lr.RecordAttributes == nil {
			lr.RecordAttributes = map[string]string{}
		}
		if _, exists := lr.RecordAttributes[k]; !exists { // don't clobber a top-level field of the same name
			lr.RecordAttributes[k] = s
		}
		if k == p.metadataTraceID {
			if tid, ok := parseTraceID(s); ok {
				lr.TraceID = tid
			}
		}
	}
}

// parseTraceID converts a 36-char hyphenated UUID (or a bare 32-hex string) into a 16-byte OTLP trace
// id. Returns ok=false for any non-conforming value or an all-zero id (invalid per the OTLP spec) — the
// caller then leaves TraceID unset.
func parseTraceID(s string) ([]byte, bool) {
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return nil, false
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, false
	}
	if bytes.Equal(b, make([]byte, 16)) {
		return nil, false // all-zero trace id is invalid
	}
	return b, true
}

// sourceAttrKey / sourceAttrValue: producer-identity record attribute stamped on every emitted log so
// portkey vs langsmith data is distinguishable downstream (Loki structured metadata). Content-free
// constant (the vendor type), not derived from record data. Mirrors the langsmith runs strip.
const (
	sourceAttrKey   = "source"
	sourceAttrValue = "portkey"
)

func stampSource(lr *model.LogRecord) {
	if lr.RecordAttributes == nil {
		lr.RecordAttributes = map[string]string{}
	}
	lr.RecordAttributes[sourceAttrKey] = sourceAttrValue
}

// scalarToString renders a JSON scalar (string/number/bool) as a string. Non-scalars (object/array/
// null) return ok=false so the caller skips them — a nested structure under an allow-listed key could
// hide content, so it is never flattened into an attribute value.
func scalarToString(raw json.RawMessage) (string, bool) {
	// JSON null = absent → drop (unmarshalling null into a *string succeeds with "", so guard explicitly;
	// otherwise a null allow-listed field — e.g. a sometimes-null opted-in field — would emit "").
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

// parseExportTime best-effort parses a Portkey created_at value. The control plane has been observed to
// stamp both ISO-8601 (RFC3339) and a JS `Date.toString()` form ("Thu Jun 18 2026 14:23:45 GMT+0000
// (Coordinated Universal Time)"); try both, stripping the trailing " (…)" zone-name the JS form appends.
func parseExportTime(raw json.RawMessage) (time.Time, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	js := s
	if i := strings.Index(js, " ("); i >= 0 {
		js = js[:i] // drop " (Coordinated Universal Time)" — Go can't match the parenthesised zone name
	}
	if t, err := time.Parse("Mon Jan 02 2006 15:04:05 GMT-0700", js); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
