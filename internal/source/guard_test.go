// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
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

// TestGuardDeniesContentFloorPrefix (#97): the guard denies a content-floor key regardless of the
// configured deny map, including flattened gen_ai content attrs matched by PREFIX — so a
// gen_ai.completion.0.content label is dropped even though it exact-matches nothing on the deny list.
func TestGuardDeniesContentFloorPrefix(t *testing.T) {
	guard := NewGuard(GuardConfig{AllowLabelKeys: []string{"ai_model", "gen_ai.completion.0.content"}, PerSeriesBudget: 100})
	in := model.Batch{Samples: []model.Sample{
		g(map[string]string{"ai_model": "claude"}),                  // ok
		g(map[string]string{"gen_ai.completion.0.content": "leak"}), // floor prefix → drop (even if allow-listed)
		g(map[string]string{"gen_ai.prompt": "leak"}),               // exact floor → drop
	}}
	out, dropped := guard.Sanitize(in)
	if len(out.Samples) != 1 || dropped != 2 {
		t.Fatalf("kept=%d dropped=%d want 1/2", len(out.Samples), dropped)
	}
}

// TestLabelSigInjective (#99): distinct label sets must never share a signature. Without escaping, a value
// containing ';'/'=' could forge another set's signature ({a:"1;b=2"} vs {a:"1",b:"2"}).
func TestLabelSigInjective(t *testing.T) {
	a := labelSig(map[string]string{"a": "1;b=2"})
	b := labelSig(map[string]string{"a": "1", "b": "2"})
	if a == b {
		t.Fatalf("labelSig collision: %q == %q (#99)", a, b)
	}
	// stable + still injective for a plain value
	if labelSig(map[string]string{"a": "1"}) == labelSig(map[string]string{"a": "2"}) {
		t.Fatal("labelSig must distinguish distinct values")
	}
}

// TestGuardBudgetNoSignatureCollision (#99): a distinct label set whose NAIVE concatenation collides with
// an already-seen set must still consume its own budget slot and be dropped when over budget — not admitted
// as "already seen". Pre-fix, the colliding set slipped through with the budget full.
func TestGuardBudgetNoSignatureCollision(t *testing.T) {
	guard := NewGuard(GuardConfig{AllowLabelKeys: []string{"a", "b"}, PerSeriesBudget: 1})
	in := model.Batch{Samples: []model.Sample{
		g(map[string]string{"a": "1;b=2"}),       // fills the budget (1 distinct signature)
		g(map[string]string{"a": "1", "b": "2"}), // distinct set, naive-colliding sig → over budget → drop
	}}
	out, dropped := guard.Sanitize(in)
	if len(out.Samples) != 1 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d want 1/1 (colliding distinct set must be counted separately)", len(out.Samples), dropped)
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
