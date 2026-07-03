// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func logRecord(indexed, record map[string]string, body string) model.LogRecord {
	return model.LogRecord{Timestamp: time.Unix(1, 0).UTC(), Body: body, IndexedAttributes: indexed, RecordAttributes: record}
}

// TestSanitizeLogsAllowListIndexed: the allow-list polices the INDEXED tier (stream labels) exactly like
// Sample.Labels — a non-allow-listed indexed key drops the record (default-deny); an allow-listed one is kept.
func TestSanitizeLogsAllowListIndexed(t *testing.T) {
	g := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model", "ai_provider"}})
	in := []model.LogRecord{
		logRecord(map[string]string{"ai_model": "gpt-5"}, nil, ""),                  // ok
		logRecord(map[string]string{"ai_model": "gpt-5", "trace_id": "x"}, nil, ""), // trace_id not allow-listed (indexed) → drop
	}
	kept, dropped := g.SanitizeLogs("logs_export", in)
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d want 1/1", len(kept), dropped)
	}
	if kept[0].IndexedAttributes["ai_model"] != "gpt-5" {
		t.Fatalf("wrong record kept: %+v", kept[0])
	}
}

// TestSanitizeLogsDenyDropsRecord: a content deny-listed key in EITHER attribute map drops the whole
// record (content-safety, matches sample policing — a record carrying a content field is suspect).
func TestSanitizeLogsDenyDropsRecord(t *testing.T) {
	g := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}, DenyFieldKeys: []string{"metadata", "prompt"}})
	in := []model.LogRecord{
		logRecord(map[string]string{"ai_model": "gpt-5"}, map[string]string{"trace_id": "t"}, ""),   // ok
		logRecord(map[string]string{"ai_model": "gpt-5"}, map[string]string{"metadata": "pii"}, ""), // denied in record attrs → drop
		logRecord(map[string]string{"ai_model": "gpt-5", "prompt": "leak"}, nil, ""),                // denied in indexed → drop
	}
	kept, dropped := g.SanitizeLogs("logs_export", in)
	if len(kept) != 1 || dropped != 2 {
		t.Fatalf("kept=%d dropped=%d want 1/2", len(kept), dropped)
	}
}

// TestSanitizeLogsStripsBody: FR10 — content never egresses via the log body; a non-empty Body is
// forced empty on the kept record (defence — loops keep it empty anyway).
func TestSanitizeLogsStripsBody(t *testing.T) {
	g := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}})
	kept, dropped := g.SanitizeLogs("logs_export", []model.LogRecord{
		logRecord(map[string]string{"ai_model": "gpt-5"}, nil, "this is content and must never egress"),
	})
	if len(kept) != 1 || dropped != 0 {
		t.Fatalf("kept=%d dropped=%d want 1/0", len(kept), dropped)
	}
	if kept[0].Body != "" {
		t.Fatalf("Body not stripped: %q", kept[0].Body)
	}
}

// TestSanitizeLogsRecordAttrsNotAllowListed: RecordAttributes are non-indexed structured metadata, NOT
// cardinality-policed — an arbitrary (non-denied) record-attr key is kept, unlike the indexed tier.
func TestSanitizeLogsRecordAttrsNotAllowListed(t *testing.T) {
	g := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}})
	kept, dropped := g.SanitizeLogs("logs_export", []model.LogRecord{
		logRecord(map[string]string{"ai_model": "gpt-5"}, map[string]string{"trace_id": "t", "response_time": "1485"}, ""),
	})
	if len(kept) != 1 || dropped != 0 {
		t.Fatalf("kept=%d dropped=%d want 1/0 (record attrs are not allow-list policed)", len(kept), dropped)
	}
	if kept[0].RecordAttributes["trace_id"] != "t" {
		t.Fatalf("record attrs should pass through: %+v", kept[0].RecordAttributes)
	}
}

// TestSanitizeLogsDeniesContentFloorPrefix (#97): a flattened gen_ai content attr in EITHER attr map
// drops the whole record via the floor prefix rule, even though it exact-matches nothing on the deny list.
func TestSanitizeLogsDeniesContentFloorPrefix(t *testing.T) {
	g := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}})
	in := []model.LogRecord{
		logRecord(map[string]string{"ai_model": "gpt-5"}, nil, ""),                                                  // ok
		logRecord(map[string]string{"ai_model": "gpt-5"}, map[string]string{"gen_ai.prompt.0.content": "leak"}, ""), // record-attr floor prefix → drop
	}
	kept, dropped := g.SanitizeLogs("logs_export", in)
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d want 1/1 (flattened gen_ai content must drop the record)", len(kept), dropped)
	}
}

// TestSanitizeLogsBudgetPerLoopIdentity (#98): the logs cardinality budget is keyed on the loop-identity
// string the caller passes, so two source instances passing DISTINCT identities (e.g. the full
// CheckpointKey "pk-eu/logs_export/f" vs "pk-us/logs_export/f") each get an INDEPENDENT budget and cannot
// starve each other; the SAME identity shares one budget. (Production wiring must pass an instance-distinct
// identity — the bare loop name conflates instances; see the runner change tracked by #98.)
func TestSanitizeLogsBudgetPerLoopIdentity(t *testing.T) {
	g := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}, PerSeriesBudget: 1})
	euA := []model.LogRecord{logRecord(map[string]string{"ai_model": "a"}, nil, "")}
	euB := []model.LogRecord{logRecord(map[string]string{"ai_model": "b"}, nil, "")}
	// instance eu: first stream fills its budget, a second distinct stream is over budget → dropped.
	if _, d := g.SanitizeLogs("pk-eu/logs_export/f", euA); d != 0 {
		t.Fatalf("eu first stream dropped=%d want 0", d)
	}
	if _, d := g.SanitizeLogs("pk-eu/logs_export/f", euB); d != 1 {
		t.Fatalf("eu second distinct stream dropped=%d want 1 (over its budget)", d)
	}
	// instance us: an INDEPENDENT budget — its first distinct stream is admitted, not starved by eu.
	if _, d := g.SanitizeLogs("pk-us/logs_export/f", euA); d != 0 {
		t.Fatalf("us first stream dropped=%d want 0 (independent per-instance budget)", d)
	}
	// same identity as eu, already-seen signature → still admitted (shared budget for one instance).
	if _, d := g.SanitizeLogs("pk-eu/logs_export/f", euA); d != 0 {
		t.Fatalf("eu already-seen stream dropped=%d want 0", d)
	}
}

// TestSanitizeLogsPerLoopDenyScoping (#130): the content denylist is scoped PER LOOP. A gray backstop
// field released for loop A (opted into A's record allow-list) must STILL be denied for loop B, whose own
// backstop nobody weakened — the defence-in-depth layer stays independent per loop. A loop with no
// per-loop entry falls back to the global DenyFieldKeys (the full floor+gray backstop).
func TestSanitizeLogsPerLoopDenyScoping(t *testing.T) {
	// Global (full) backstop denies both gray fields; loop "A" released only "error".
	g := NewGuard(GuardConfig{
		AllowLabelKeys: []string{"ai_model"},
		DenyFieldKeys:  []string{"error", "events"}, // metrics + fallback: full backstop
		DenyFieldKeysByLoop: map[string][]string{
			"A": {"events"}, // A opted "error" in → only "events" remains denied for A
		},
	})
	recWithError := []model.LogRecord{logRecord(map[string]string{"ai_model": "gpt-5"}, map[string]string{"error": "boom"}, "")}
	recWithEvents := []model.LogRecord{logRecord(map[string]string{"ai_model": "gpt-5"}, map[string]string{"events": "x"}, "")}

	// Loop A released "error" → the record is KEPT for A.
	if kept, d := g.SanitizeLogs("A", recWithError); len(kept) != 1 || d != 0 {
		t.Fatalf("loop A error-record: kept=%d dropped=%d want 1/0 (A opted error in)", len(kept), d)
	}
	// A did NOT release "events" → still denied for A.
	if kept, d := g.SanitizeLogs("A", recWithEvents); len(kept) != 0 || d != 1 {
		t.Fatalf("loop A events-record: kept=%d dropped=%d want 0/1 (A did not opt events in)", len(kept), d)
	}
	// Loop B has NO per-loop entry → falls back to the FULL global backstop: "error" is still denied.
	// This is the crux of #130 — A's opt-in must NOT widen B's allowed set.
	if kept, d := g.SanitizeLogs("B", recWithError); len(kept) != 0 || d != 1 {
		t.Fatalf("loop B error-record: kept=%d dropped=%d want 0/1 (A's opt-in must not release error for B)", len(kept), d)
	}
}

// TestSanitizeLogsBudget: distinct INDEXED signatures (= distinct Loki streams) are capped per loop.
func TestSanitizeLogsBudget(t *testing.T) {
	g := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}, PerSeriesBudget: 2})
	in := []model.LogRecord{
		logRecord(map[string]string{"ai_model": "a"}, nil, ""),
		logRecord(map[string]string{"ai_model": "b"}, nil, ""),
		logRecord(map[string]string{"ai_model": "c"}, nil, ""), // 3rd distinct stream → over budget → drop
		logRecord(map[string]string{"ai_model": "a"}, nil, ""), // already-seen signature → kept
	}
	kept, dropped := g.SanitizeLogs("logs_export", in)
	if dropped != 1 {
		t.Fatalf("dropped=%d want 1 (only the 3rd distinct stream)", dropped)
	}
	if len(kept) != 3 {
		t.Fatalf("kept=%d want 3 (a,b,a)", len(kept))
	}
}
