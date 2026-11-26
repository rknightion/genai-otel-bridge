// SPDX-License-Identifier: Apache-2.0

package source

import (
	"fmt"
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

// budgetPlane drives one cardinality-budget plane (metric samples or log records) with a single label
// set per call, so the active-series tests below run unchanged against Sanitize and SanitizeLogs (#383).
type budgetPlane struct {
	name   string
	series string // the key onNew reports
	send   func(g *Guard, value string) (dropped int)
}

var budgetPlanes = []budgetPlane{
	{name: "samples", series: "m", send: func(g *Guard, v string) int {
		_, d := g.Sanitize(model.Batch{Samples: []model.Sample{{Name: "m", Kind: model.Gauge, Labels: map[string]string{"session": v}, Timestamp: time.Unix(1, 0)}}})
		return d
	}},
	{name: "logs", series: "logs:inst/loop/f", send: func(g *Guard, v string) int {
		_, d := g.SanitizeLogs("inst/loop/f", []model.LogRecord{logRecord(map[string]string{"session": v}, nil, "")})
		return d
	}},
}

// fakeClock is a settable clock for GuardConfig.Now.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time       { return c.t }
func (c *fakeClock) step(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock            { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }
func budgetGuard(c *fakeClock, budget int, onNew func(string)) *Guard {
	return NewGuard(GuardConfig{AllowLabelKeys: []string{"session"}, PerSeriesBudget: budget, OnNewLabelValue: onNew, Now: c.now})
}

// TestGuardBudgetIsActiveNotLifetime (#383): the budget bounds ACTIVE label sets. With exactly one
// active label set per tick and the clock stepping past the idle window between ticks, a churning
// dimension three times the budget is never dropped, every admission still fires the new-label-value
// hook, and the tracked state never grows past the budget. Pre-fix, the set of label sets ever seen
// filled after budget ticks and every later one was dropped for the life of the process.
func TestGuardBudgetIsActiveNotLifetime(t *testing.T) {
	for _, p := range budgetPlanes {
		t.Run(p.name, func(t *testing.T) {
			const budget = 5
			clock := newFakeClock()
			warned := map[string]int{}
			g := budgetGuard(clock, budget, func(s string) { warned[s]++ })
			dropped := 0
			for i := 0; i < 3*budget; i++ {
				dropped += p.send(g, fmt.Sprintf("s%d", i))
				clock.step(seenIdleWindow + time.Minute)
			}
			if dropped != 0 {
				t.Fatalf("1 active label set per tick against a budget of %d, yet %d of %d were dropped", budget, dropped, 3*budget)
			}
			if warned[p.series] != 3*budget {
				t.Fatalf("new-label-value hook fired %d times for %q, want %d (one per admission)", warned[p.series], p.series, 3*budget)
			}
			if n := len(g.seen[p.series].last); n > budget {
				t.Fatalf("tracked %d label sets, want at most the budget %d (memory must stay bounded)", n, budget)
			}
		})
	}
}

// TestGuardBudgetStillDropsWhenFull (#383): idle eviction must not turn the budget into no budget.
// budget+1 label sets active at once drop exactly one, and a label set last seen just inside the idle
// window still holds its slot; only once it has been idle for the whole window does a new set get in.
func TestGuardBudgetStillDropsWhenFull(t *testing.T) {
	for _, p := range budgetPlanes {
		t.Run(p.name, func(t *testing.T) {
			const budget = 5
			clock := newFakeClock()
			warned := 0
			g := budgetGuard(clock, budget, func(string) { warned++ })
			dropped := 0
			for i := 0; i <= budget; i++ { // budget+1 simultaneously active label sets
				dropped += p.send(g, fmt.Sprintf("s%d", i))
			}
			if dropped != 1 {
				t.Fatalf("budget+1 active label sets: dropped %d, want exactly 1", dropped)
			}
			if warned != budget+1 {
				t.Fatalf("new-label-value hook fired %d times, want %d (each admission plus the over-budget drop)", warned, budget+1)
			}
			// The admitted sets keep arriving, so they stay active and a new set is still refused.
			clock.step(seenIdleWindow / 2)
			for i := 0; i < budget; i++ {
				if d := p.send(g, fmt.Sprintf("s%d", i)); d != 0 {
					t.Fatalf("active label set s%d dropped=%d, want 0", i, d)
				}
			}
			clock.step(seenIdleWindow - time.Second) // idle just under the window: still counted
			if d := p.send(g, "late"); d != 1 {
				t.Fatalf("new label set with the budget full of sets idle %s: dropped=%d, want 1", seenIdleWindow-time.Second, d)
			}
			clock.step(time.Second) // now idle for the whole window: evicted, the new set is admitted
			if d := p.send(g, "late"); d != 0 {
				t.Fatalf("new label set once the full set had been idle %s: dropped=%d, want 0", seenIdleWindow, d)
			}
		})
	}
}
