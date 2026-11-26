// SPDX-License-Identifier: Apache-2.0

package source

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// GuardConfig configures the governance guard.
type GuardConfig struct {
	AllowLabelKeys []string
	DenyFieldKeys  []string
	// DenyFieldKeysByLoop is the PER-LOOP effective content denylist for the logs path (#130), keyed by the
	// loop-identity string the runner passes to SanitizeLogs (model.CheckpointKey.String()). A logs loop that
	// opts a subtractable "gray" backstop field into its record allow-list has THAT field (and only that
	// field) removed from its OWN entry — so an opt-in on loop A can never widen the allowed set of loop B.
	// A loop with no entry (every metrics loop, plus any logs loop that opted nothing in) falls back to the
	// global DenyFieldKeys, which is the full floor+gray backstop. Never-subtractable content-floor keys are
	// denied regardless of this map (IsContentFloorKey), so a per-loop entry can only release gray fields.
	DenyFieldKeysByLoop map[string][]string
	// PerSeriesBudget caps the ACTIVE label sets per metric name (and per logs loop): a label set counts
	// against the budget until it has been idle for seenIdleWindow (#383). 0 or less means unlimited.
	PerSeriesBudget int
	OnNewLabelValue func(series string) // early-warning hook → new_label_values{series} self-metric
	// Now is the clock the budget's idle eviction reads. nil means time.Now; tests inject a fake.
	Now func() time.Time
}

// seenIdleWindow is how long a label set stays counted against PerSeriesBudget after it was last
// admitted (#383). The budget bounds ACTIVE label sets, as the downstream active-series limit does, so a
// churning dimension (per-experiment sessions, prompt ids) is not starved by label sets that stopped
// arriving long ago. Fixed, not a config knob: it only needs to exceed every loop's poll cadence by a
// margin, and one hour is far above the 60s default.
const seenIdleWindow = time.Hour

// seriesSeen is the cardinality state of one metric name (or one logs loop): every admitted label-set
// signature with the time it was last admitted. It never holds more than budget entries: a signature is
// only inserted while len(last) < budget, so memory stays bounded by budget x signature size per series.
type seriesSeen struct {
	last map[string]time.Time
	// nextExpiry is the earliest time any entry could have gone idle (oldest last-seen at the previous
	// sweep + seenIdleWindow). Refreshes only move entries later, so before nextExpiry a sweep cannot evict
	// anything and is skipped: the O(budget) sweep runs only when the set is full, a new label set
	// arrives and at least one entry may have expired.
	nextExpiry time.Time
}

// Guard sits between derive and emit: drops samples with disallowed or deny-listed label keys,
// and enforces a per-series cardinality budget. [CP-C6] Empty AllowLabelKeys denies all label keys.
type Guard struct {
	allow  map[string]bool
	deny   map[string]bool
	budget int
	onNew  func(string)
	// denyByLoop is the #130 per-loop effective content denylist for the logs path, keyed by the loop
	// identity SanitizeLogs receives. A loop present here uses its own map; a loop absent falls back to
	// `deny`. Written only at construction (read-only after), like allow/deny — no lock needed.
	denyByLoop map[string]map[string]bool
	// [ext-review-8] One *Guard is shared across all loop runners of a group (the composition root builds
	// one per group of rows sharing a stack and an identity; a legacy install is one group), each running
	// its own emit-worker goroutine, so the mutable cardinality state must be locked. allow/deny/denyByLoop are
	// written only at construction (read-only after) so they need no lock; seen is read+written here.
	mu   sync.Mutex
	seen map[string]*seriesSeen // series → admitted labelset signatures with their last-seen time
	now  func() time.Time
}

// NewGuard builds a Guard from config.
func NewGuard(cfg GuardConfig) *Guard {
	g := &Guard{
		allow:  map[string]bool{},
		deny:   map[string]bool{},
		budget: cfg.PerSeriesBudget,
		onNew:  cfg.OnNewLabelValue,
		seen:   map[string]*seriesSeen{},
		now:    cfg.Now,
	}
	if g.now == nil {
		g.now = time.Now
	}
	for _, k := range cfg.AllowLabelKeys {
		g.allow[k] = true
	}
	for _, k := range cfg.DenyFieldKeys {
		g.deny[k] = true
	}
	if len(cfg.DenyFieldKeysByLoop) > 0 {
		g.denyByLoop = make(map[string]map[string]bool, len(cfg.DenyFieldKeysByLoop))
		for loop, keys := range cfg.DenyFieldKeysByLoop {
			m := make(map[string]bool, len(keys))
			for _, k := range keys {
				m[k] = true
			}
			g.denyByLoop[loop] = m
		}
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
	// #130: use THIS loop's effective denylist if it has one (a logs loop that opted a gray field in);
	// otherwise the full global backstop. A per-loop entry only ever RELEASES gray fields — the
	// never-subtractable floor is still enforced below via IsContentFloorKey — so scoping can only make a
	// loop's egress tighter than another's, never widen one loop's allowed set from another's opt-in.
	deny := g.deny
	if d, ok := g.denyByLoop[loop]; ok {
		deny = d
	}
	// Content deny-list across both maps: any denied key ⇒ drop the record (loud-counted, never emitted).
	// A never-subtractable content-floor key is denied here regardless of the configured deny map AND by
	// PREFIX for the gen_ai content namespaces (IsContentFloorKey), so a flattened content attr like
	// gen_ai.prompt.0.content that the exact-match deny map would miss is still blocked at the chokepoint (#97).
	for _, m := range []map[string]string{lr.IndexedAttributes, lr.RecordAttributes} {
		for k := range m {
			if deny[k] || IsContentFloorKey(k) {
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
	return g.admit("logs:"+loop, labelSig(lr.IndexedAttributes))
}

func (g *Guard) ok(s model.Sample) bool {
	for k := range s.Labels {
		if g.deny[k] || IsContentFloorKey(k) { // floor is denied regardless of the deny map, incl. gen_ai prefix variants (#97)
			return false
		}
		if !g.allow[k] { // [CP-C6] empty allowlist ⇒ DENY all label keys (v1 no-label policy); NEVER allow-any
			return false
		}
	}
	if g.budget <= 0 {
		return true
	}
	return g.admit(s.Name, labelSig(s.Labels))
}

// admit applies the per-series cardinality budget to one label-set signature and reports whether it
// may be emitted. The budget counts ACTIVE label sets (#383): a known signature is admitted and its
// last-seen refreshed; a new one is admitted while fewer than budget signatures are held, and when the
// set is full the signatures idle for seenIdleWindow or longer are evicted first. onNew fires on every
// new admission and on every over-budget drop, as before.
func (g *Guard) admit(series, sig string) bool {
	g.mu.Lock() // [ext-review-8] guards the shared per-series cardinality state
	defer g.mu.Unlock()
	now := g.now()
	st := g.seen[series]
	if st == nil {
		st = &seriesSeen{last: map[string]time.Time{}}
		g.seen[series] = st
	}
	if last, known := st.last[sig]; known {
		if now.After(last) {
			st.last[sig] = now
		}
		return true
	}
	if len(st.last) >= g.budget {
		st.evictIdle(now)
	}
	if len(st.last) >= g.budget {
		if g.onNew != nil {
			g.onNew(series) // budget exceeded — alert before the series explodes
		}
		return false
	}
	st.last[sig] = now
	if g.onNew != nil {
		g.onNew(series)
	}
	return true
}

// evictIdle deletes every signature last admitted seenIdleWindow or more before now, and records when
// the oldest survivor can next expire so a sweep that would evict nothing is skipped.
func (st *seriesSeen) evictIdle(now time.Time) {
	if now.Before(st.nextExpiry) {
		return
	}
	var oldest time.Time
	survivors := false
	for sig, last := range st.last {
		if now.Sub(last) >= seenIdleWindow {
			delete(st.last, sig)
			continue
		}
		if !survivors || last.Before(oldest) {
			oldest, survivors = last, true
		}
	}
	if !survivors {
		st.nextExpiry = time.Time{}
		return
	}
	st.nextExpiry = oldest.Add(seenIdleWindow)
}

// labelSigReplacer escapes the signature's delimiter/escape characters so labelSig is INJECTIVE over
// label sets. Without it a label value containing '=' or ';' could forge a different label set's
// signature — e.g. {a:"1;b=2"} and {a:"1",b:"2"} both naively render "a=1;b=2;" — collapsing two distinct
// downstream series into one cardinality-budget slot (and admitting a colliding set as "already seen"
// even when the budget is full). Escaping '\' first keeps the encoding reversible/unambiguous (#99).
var labelSigReplacer = strings.NewReplacer(`\`, `\\`, `=`, `\=`, `;`, `\;`)

// labelSig returns a deterministic, injective string signature for a label set, used for cardinality
// tracking. Keys and values are individually escaped so no combination of characters in one label can
// reproduce the signature of a different label set (#99).
func labelSig(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(labelSigReplacer.Replace(k))
		b.WriteByte('=')
		b.WriteString(labelSigReplacer.Replace(m[k]))
		b.WriteByte(';')
	}
	return b.String()
}
