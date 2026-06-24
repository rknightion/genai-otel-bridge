// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
)

func g(labels map[string]string) model.Sample {
	return model.Sample{Name: "portkey_api_tokens", Kind: model.Gauge, Labels: labels, Timestamp: time.Unix(1, 0)}
}

func TestGuardDropsDisallowedKeyAndContent(t *testing.T) {
	guard := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}, DenyFieldKeys: []string{"gen_ai.prompt"}, PerSeriesBudget: 100})
	in := model.Batch{Samples: []model.Sample{
		g(map[string]string{"ai_model": "claude"}),      // ok
		g(map[string]string{"request_id": "uuid-here"}), // disallowed key → drop
		g(map[string]string{"gen_ai.prompt": "leak"}),   // denylisted content → drop
	}}
	out, dropped := guard.Sanitize(in)
	if len(out.Samples) != 1 || dropped != 2 {
		t.Fatalf("kept=%d dropped=%d", len(out.Samples), dropped)
	}
}

func TestGuardCardinalityBudget(t *testing.T) {
	var warned int
	guard := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model"}, PerSeriesBudget: 2, OnNewLabelValue: func(string) { warned++ }})
	in := model.Batch{Samples: []model.Sample{
		g(map[string]string{"ai_model": "a"}),
		g(map[string]string{"ai_model": "b"}),
		g(map[string]string{"ai_model": "c"}), // 3rd distinct combo > budget 2 → drop
	}}
	out, dropped := guard.Sanitize(in)
	if len(out.Samples) != 2 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d", len(out.Samples), dropped)
	}
	if warned == 0 {
		t.Fatal("new-label-value hook never fired")
	}
}

func TestGuardEmptyAllowlistDeniesLabels(t *testing.T) {
	// CP-C6: empty allowlist MUST deny all label keys (v1 no-label policy)
	guard := NewGuard(GuardConfig{PerSeriesBudget: 100})
	in := model.Batch{Samples: []model.Sample{
		g(map[string]string{"any_key": "any_value"}),
		g(nil), // no labels → should pass
	}}
	out, dropped := guard.Sanitize(in)
	if dropped != 1 {
		t.Fatalf("empty allowlist: dropped=%d want 1", dropped)
	}
	if len(out.Samples) != 1 {
		t.Fatalf("empty allowlist: kept=%d want 1 (no-label sample)", len(out.Samples))
	}
}
