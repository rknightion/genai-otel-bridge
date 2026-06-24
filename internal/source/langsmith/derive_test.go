// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
)

func TestDeriveRunCount(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	rc := 450
	sessions := []tracerSession{{ID: "s1", Name: "proj-a", RunCount: &rc}}

	got := derive(sessions, deriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	if len(got) != 1 {
		t.Fatalf("samples=%d want 1", len(got))
	}
	s := got[0]
	if s.Name != "langsmith_runs" {
		t.Fatalf("name=%q want langsmith_runs", s.Name)
	}
	if s.Kind != model.Gauge {
		t.Fatalf("kind=%v want Gauge", s.Kind)
	}
	if s.Value != 450 {
		t.Fatalf("value=%v want 450", s.Value)
	}
	if s.Labels["session"] != "s1" {
		t.Fatalf("session label=%q want s1 (id)", s.Labels["session"])
	}
	if !s.Timestamp.Equal(now) {
		t.Fatalf("timestamp=%v want now=%v", s.Timestamp, now)
	}
}

func TestDeriveLatencyQuantiles(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	p50, p99 := 0.495, 56.35091 // live: SECONDS, no ms conversion
	sessions := []tracerSession{{ID: "s1", LatencyP50: &p50, LatencyP99: &p99}}

	got := derive(sessions, deriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	byQ := map[string]model.Sample{}
	for _, s := range got {
		if s.Name != "langsmith_latency_seconds" {
			continue
		}
		byQ[s.Labels["quantile"]] = s
	}
	if len(byQ) != 2 {
		t.Fatalf("latency quantile samples=%d want 2 (p50,p99): %+v", len(byQ), got)
	}
	if byQ["p50"].Value != 0.495 {
		t.Fatalf("p50=%v want 0.495 (seconds, no conversion)", byQ["p50"].Value)
	}
	if byQ["p99"].Value != 56.35091 {
		t.Fatalf("p99=%v want 56.35091", byQ["p99"].Value)
	}
	if byQ["p50"].Labels["session"] != "s1" {
		t.Fatalf("missing session label on quantile sample")
	}
}

// Only NUMERIC feedback (avg != null) becomes a gauge. Categorical / id-like keys (avg null) — which
// is where high-cardinality identifiers like portkey_trace_id / user_id live — must produce NO sample.
// This is the content-minimisation + cardinality rule, enforced structurally.
func TestDeriveNumericFeedbackOnly(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	avg := 0.838
	n := 450
	sessions := []tracerSession{{ID: "s1", FeedbackStats: map[string]feedbackStat{
		"algo_accuracy":    {N: &n, Avg: &avg}, // numeric ⇒ emit
		"portkey_trace_id": {N: &n, Avg: nil},  // categorical/high-card ⇒ skip
		"user_id":          {N: &n, Avg: nil},  // PII-adjacent ⇒ skip
	}}}

	got := derive(sessions, deriveConfig{prefix: "langsmith", sessionLabelKey: "session", emitFeedback: true}, now)

	var score, count *model.Sample
	for i := range got {
		s := &got[i]
		if k := s.Labels["feedback_key"]; k == "portkey_trace_id" || k == "user_id" {
			t.Fatalf("emitted id-like feedback key %q (cardinality/content leak)", k)
		}
		switch s.Name {
		case "langsmith_feedback_score":
			if s.Labels["feedback_key"] == "algo_accuracy" {
				score = s
			}
		case "langsmith_feedback_count":
			if s.Labels["feedback_key"] == "algo_accuracy" {
				count = s
			}
		}
	}
	if score == nil || score.Value != 0.838 {
		t.Fatalf("feedback_score{algo_accuracy} = %v want 0.838", score)
	}
	if count == nil || count.Value != 450 {
		t.Fatalf("feedback_count{algo_accuracy} = %v want 450", count)
	}
}

// EmitFeedback=false suppresses the whole feedback facet (scalar facets still emit).
func TestDeriveFeedbackDisabled(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	avg := 0.5
	rc := 1
	sessions := []tracerSession{{ID: "s1", RunCount: &rc, FeedbackStats: map[string]feedbackStat{"acc": {Avg: &avg}}}}

	got := derive(sessions, deriveConfig{prefix: "langsmith", sessionLabelKey: "session", emitFeedback: false}, now)

	for _, s := range got {
		if s.Name == "langsmith_feedback_score" || s.Name == "langsmith_feedback_count" {
			t.Fatalf("feedback emitted with EmitFeedback=false: %+v", s)
		}
	}
	if len(got) != 1 {
		t.Fatalf("samples=%d want 1 (run_count only)", len(got))
	}
}

// derive must be deterministic regardless of input session order (sessions sorted by id) — the
// emitter relies on byte-identical encode.
func TestDeriveDeterministicSessionOrder(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	rc := 1
	sessions := []tracerSession{{ID: "b-sess", RunCount: &rc}, {ID: "a-sess", RunCount: &rc}}

	got := derive(sessions, deriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	var order []string
	for _, s := range got {
		order = append(order, s.Labels["session"])
	}
	if len(order) != 2 || order[0] != "a-sess" || order[1] != "b-sess" {
		t.Fatalf("sessions not emitted in id order: %v", order)
	}
}

func TestDeriveScalarFacets(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tt, pt, ct := 16513488, 15634481, 879007
	er, sr := 0.0, 0.5
	sessions := []tracerSession{{
		ID:               "s1",
		TotalTokens:      &tt,
		PromptTokens:     &pt,
		CompletionTokens: &ct,
		TotalCost:        money{v: 0.12, set: true},
		PromptCost:       money{v: 0.10, set: true},
		CompletionCost:   money{}, // unset ⇒ must be skipped
		ErrorRate:        &er,     // real 0 ⇒ emitted
		StreamingRate:    &sr,
	}}

	got := derive(sessions, deriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	val := map[string]float64{}
	for _, s := range got {
		val[s.Name] = s.Value
	}
	want := map[string]float64{
		"langsmith_tokens":            16513488,
		"langsmith_prompt_tokens":     15634481,
		"langsmith_completion_tokens": 879007,
		"langsmith_cost_usd":          0.12,
		"langsmith_prompt_cost_usd":   0.10,
		"langsmith_error_rate":        0.0, // real zero, NOT skipped
		"langsmith_streaming_rate":    0.5,
	}
	for name, w := range want {
		if v, ok := val[name]; !ok || v != w {
			t.Fatalf("%s = %v (present=%v) want %v", name, v, ok, w)
		}
	}
	if _, ok := val["langsmith_completion_cost_usd"]; ok {
		t.Fatal("emitted completion_cost_usd from an unset money (want skip)")
	}
}

// first_token_p50/p99 are null when the session isn't streaming — a null must be SKIPPED, never
// emitted as 0 (null = absent, 0.0 = a real measured value).
func TestDeriveSkipsNullStats(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	rc := 10
	sessions := []tracerSession{{ID: "s1", RunCount: &rc}} // latency + first_token nil

	got := derive(sessions, deriveConfig{prefix: "langsmith", sessionLabelKey: "session"}, now)

	for _, s := range got {
		switch s.Name {
		case "langsmith_latency_seconds", "langsmith_first_token_seconds":
			t.Fatalf("emitted %q from a nil stat (want skip): %+v", s.Name, s)
		}
	}
	if len(got) != 1 { // only run_count
		t.Fatalf("samples=%d want 1 (run_count only; nils skipped)", len(got))
	}
}
