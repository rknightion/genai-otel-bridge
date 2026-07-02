// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// The usage loop's pure derive: per-project platform cost drivers — traces (billable unit, from
// session.run_count) + optional spans (storage driver, from runs/stats) — each carrying the
// retention_tier label (the billing multiplier). Aggregate-now, minute-truncated stamp.
func TestUsageDeriveTracesWithTier(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 30, 0, time.UTC) // 30s past the minute → stamp truncates to :00
	sessions := []usageSession{{ID: "s1", Name: "proj-a", RunCount: new(100), TraceTier: "longlived"}}

	got := usageDerive(sessions, nil, usageDeriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	if len(got) != 1 {
		t.Fatalf("samples=%d want 1 (traces only; spans nil): %+v", len(got), got)
	}
	s := got[0]
	if s.Name != "langsmith_usage_traces" {
		t.Fatalf("name=%q want langsmith_usage_traces", s.Name)
	}
	if s.Kind != model.Gauge || s.Value != 100 {
		t.Fatalf("kind=%v value=%v want Gauge/100", s.Kind, s.Value)
	}
	if s.Labels["session"] != "s1" || s.Labels["retention_tier"] != "longlived" {
		t.Fatalf("labels=%v want session=s1 retention_tier=longlived", s.Labels)
	}
	if !s.Timestamp.Equal(now.Truncate(time.Minute)) {
		t.Fatalf("timestamp=%v want minute-truncated %v", s.Timestamp, now.Truncate(time.Minute))
	}
}

func TestUsageDeriveSpansAndUnknownTier(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	sessions := []usageSession{{ID: "s1", RunCount: new(100), TraceTier: ""}} // empty tier → "unknown"
	spans := map[string]int{"s1": 800}

	got := usageDerive(sessions, spans, usageDeriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	byName := map[string]model.Sample{}
	for _, s := range got {
		byName[s.Name] = s
	}
	tr, ok := byName["langsmith_usage_traces"]
	if !ok || tr.Value != 100 {
		t.Fatalf("traces=%v want 100", tr)
	}
	sp, ok := byName["langsmith_usage_spans"]
	if !ok || sp.Value != 800 {
		t.Fatalf("spans=%v want 800", sp)
	}
	if tr.Labels["retention_tier"] != "unknown" || sp.Labels["retention_tier"] != "unknown" {
		t.Fatalf("nil/empty tier must map to 'unknown', got traces=%q spans=%q",
			tr.Labels["retention_tier"], sp.Labels["retention_tier"])
	}
}

// A nil RunCount emits no traces gauge; a session absent from the spans map emits no spans gauge
// (a failed/disabled span call must not fabricate a zero). Deterministic order by session id.
func TestUsageDeriveNilAndOrdering(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	sessions := []usageSession{
		{ID: "s2", RunCount: new(5), TraceTier: "shortlived"},
		{ID: "s1", RunCount: nil, TraceTier: "longlived"}, // no run_count → no traces sample
	}
	spans := map[string]int{"s2": 40} // s1 absent → no spans sample for s1

	got := usageDerive(sessions, spans, usageDeriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	if len(got) != 2 { // s2 traces + s2 spans; s1 contributes nothing
		t.Fatalf("samples=%d want 2: %+v", len(got), got)
	}
	if got[0].Labels["session"] != "s2" || got[1].Labels["session"] != "s2" {
		t.Fatalf("expected only s2 samples in id order, got %+v", got)
	}
}

func TestUsageDeriveUseName(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	sessions := []usageSession{{ID: "s1", Name: "proj-a", RunCount: new(3), TraceTier: "longlived"}}

	got := usageDerive(sessions, nil, usageDeriveConfig{prefix: "langsmith", sessionLabelKey: "session", useName: true}, now)
	if got[0].Labels["session"] != "proj-a" {
		t.Fatalf("useName: session label=%q want proj-a", got[0].Labels["session"])
	}
}
