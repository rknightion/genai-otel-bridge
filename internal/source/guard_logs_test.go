// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
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
