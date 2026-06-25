// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"sort"
	"strings"
	"sync"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// GuardConfig configures the governance guard.
type GuardConfig struct {
	AllowLabelKeys  []string
	DenyFieldKeys   []string
	PerSeriesBudget int
	OnNewLabelValue func(series string) // early-warning hook → new_label_values{series} self-metric
}

// Guard sits between derive and emit: drops samples with disallowed or deny-listed label keys,
// and enforces a per-series cardinality budget. [CP-C6] Empty AllowLabelKeys denies all label keys.
type Guard struct {
	allow  map[string]bool
	deny   map[string]bool
	budget int
	onNew  func(string)
	// [ext-review-8] One *Guard is shared across all loop runners (composition root), each running its
	// own emit-worker goroutine, so the mutable cardinality state must be locked. allow/deny are
	// written only at construction (read-only after) so they need no lock; seen is read+written here.
	mu   sync.Mutex
	seen map[string]map[string]struct{} // series → set of distinct labelset signatures
}

// NewGuard builds a Guard from config.
func NewGuard(cfg GuardConfig) *Guard {
	g := &Guard{
		allow:  map[string]bool{},
		deny:   map[string]bool{},
		budget: cfg.PerSeriesBudget,
		onNew:  cfg.OnNewLabelValue,
		seen:   map[string]map[string]struct{}{},
	}
	for _, k := range cfg.AllowLabelKeys {
		g.allow[k] = true
	}
	for _, k := range cfg.DenyFieldKeys {
		g.deny[k] = true
	}
	return g
}

// Sanitize drops any sample whose label keys are not allow-listed, are deny-listed, or that
// would exceed the per-series cardinality budget; returns the kept batch + drop count. It never
// errors (a violation is a counted drop + alert, never a poison-pill) and never mutates content.
func (g *Guard) Sanitize(b model.Batch) (model.Batch, int) {
	kept := b.Samples[:0:0]
	dropped := 0
	for _, s := range b.Samples {
		if !g.ok(s) {
			dropped++
			continue
		}
		kept = append(kept, s)
	}
	out := b
	out.Samples = kept
	return out, dropped
}

// SanitizeLogs is the log-record analogue of Sanitize (the same chokepoint, the same counted-drop/
// no-poison-pill contract). It returns the kept records + a drop count:
//   - Body is forced empty (FR10 — content never egresses via the body; loops keep it empty, this is
//     defence-in-depth at the guard chokepoint).
//   - The content DENY-list applies to BOTH attribute maps: a denied key in IndexedAttributes OR
//     RecordAttributes drops the whole record (a record carrying a content field is suspect — matches
//     how Sanitize drops a Sample with a denied label).
//   - The ALLOW-list + per-loop cardinality budget police the INDEXED tier only (IndexedAttributes →
//     OTLP resource attrs → Loki stream labels — the cardinality-dangerous tier, default-deny like
//     Sample.Labels). RecordAttributes are non-indexed structured metadata: deny-policed but NOT
//     allow-list/budget policed (they don't create streams).
func (g *Guard) SanitizeLogs(loop string, logs []model.LogRecord) ([]model.LogRecord, int) {
	kept := logs[:0:0]
	dropped := 0
	for _, lr := range logs {
		lr.Body = "" // FR10 — strip any content before the allow/deny checks
		if !g.okLog(loop, lr) {
			dropped++
			continue
		}
		kept = append(kept, lr)
	}
	return kept, dropped
}

func (g *Guard) okLog(loop string, lr model.LogRecord) bool {
	// Content deny-list across both maps: any denied key ⇒ drop the record (loud-counted, never emitted).
	for _, m := range []map[string]string{lr.IndexedAttributes, lr.RecordAttributes} {
		for k := range m {
			if g.deny[k] {
				return false
			}
		}
	}
	// Allow-list on the indexed (stream-label) tier only — default-deny, exactly like Sample.Labels.
	for k := range lr.IndexedAttributes {
		if !g.allow[k] { // [CP-C6] empty allow-list ⇒ deny all indexed keys
			return false
		}
	}
	if g.budget <= 0 {
		return true
	}
	// Per-loop cardinality budget on the distinct INDEXED signature (= distinct Loki streams). Keyed by a
	// synthetic per-loop series ("logs:<loop>") since a LogRecord has no metric Name to key on.
	g.mu.Lock()
	defer g.mu.Unlock()
	series := "logs:" + loop
	sig := labelSig(lr.IndexedAttributes)
	set := g.seen[series]
	if set == nil {
		set = map[string]struct{}{}
		g.seen[series] = set
	}
	if _, known := set[sig]; known {
		return true
	}
	if len(set) >= g.budget {
		if g.onNew != nil {
			g.onNew(series)
		}
		return false
	}
	set[sig] = struct{}{}
	if g.onNew != nil {
		g.onNew(series)
	}
	return true
}

func (g *Guard) ok(s model.Sample) bool {
	for k := range s.Labels {
		if g.deny[k] {
			return false
		}
		if !g.allow[k] { // [CP-C6] empty allowlist ⇒ DENY all label keys (v1 no-label policy); NEVER allow-any
			return false
		}
	}
	if g.budget <= 0 {
		return true
	}
	g.mu.Lock() // [ext-review-8] guards the shared per-series cardinality state
	defer g.mu.Unlock()
	sig := labelSig(s.Labels)
	set := g.seen[s.Name]
	if set == nil {
		set = map[string]struct{}{}
		g.seen[s.Name] = set
	}
	if _, known := set[sig]; known {
		return true
	}
	if len(set) >= g.budget {
		if g.onNew != nil {
			g.onNew(s.Name) // budget exceeded — alert before the series explodes
		}
		return false
	}
	set[sig] = struct{}{}
	if g.onNew != nil {
		g.onNew(s.Name)
	}
	return true
}

// labelSig returns a deterministic string signature for a label set, used for cardinality tracking.
func labelSig(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte(';')
	}
	return b.String()
}
