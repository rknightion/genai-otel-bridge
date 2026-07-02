// SPDX-License-Identifier: AGPL-3.0-only

// The `usage` loop derives LangSmith PLATFORM cost-driver gauges (distinct from the sessions loop's
// eval/LLM metrics). LangSmith Enterprise bills on TRACES INGESTED × retention tier (+ storage), NOT on
// token cost — so this loop emits the billing DRIVERS per project so dashboards can attribute and convert
// to $ (the $ figure itself is not in the API — payment_enabled=false; billed out-of-band via Metronome):
//   - {prefix}_usage_traces  — traces (root runs) = the billable unit (from session.run_count).
//   - {prefix}_usage_spans   — all runs (spans) = the storage/"excessive spans" driver (from runs/stats;
//     only when emit_span_counts is on, since it costs one extra call per project).
//
// Both carry the retention_tier label (longlived=400d = the expensive tier; shortlived=14d) — the billing
// multiplier. Aggregate-now snapshot (rolling window), minute-truncated stamp (1DPM), like the sessions
// loop. Content-free by construction: only counts + a low-card enum tier + the session dimension.
package langsmith

import (
	"sort"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// usageSession decodes ONLY the fields the usage loop emits (run_count + trace_tier + the session
// dimension). Every other session field — including all content/free-form/id fields — is DELIBERATELY
// ABSENT so the JSON decoder skips it and it never enters process memory (content-safety by construction).
type usageSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RunCount  *int   `json:"run_count"`  // traces (root runs) over the stats window; nil ⇒ no traces sample
	TraceTier string `json:"trace_tier"` // longlived|shortlived; "" ⇒ emitted as retention_tier="unknown"
}

// label returns the per-session dimension value: id (default, bounded) or name (human, but ephemeral/
// high-card — same caveat as the sessions loop). Mirrors tracerSession.label.
func (s usageSession) label(useName bool) string {
	if useName && s.Name != "" {
		return s.Name
	}
	return s.ID
}

// usageDeriveConfig is the pure derive's decoupled naming/label policy. No I/O, no clock.
type usageDeriveConfig struct {
	prefix          string // metric name prefix (e.g. "langsmith")
	sessionLabelKey string // label key for the per-session dimension (e.g. "session")
	useName         bool   // label value = session name when true, else session id (default: id)
}

// usageDerive is PURE: (sessions, per-session span counts, naming policy, poll time) → forward Gauge
// samples stamped at the minute-truncated `now` (aggregate-now snapshot; two same-minute polls share a
// timestamp so Mimir LWW dedups to 1DPM — see the sessions loop's review-M3). Deterministic order
// (sessions by id) for the emitter's byte-identical encode. `spans` maps session id → span count; a
// session ABSENT from the map emits no spans gauge (a disabled/failed span call must not fabricate a 0).
func usageDerive(sessions []usageSession, spans map[string]int, cfg usageDeriveConfig, now time.Time) []model.Sample {
	stamp := now.Truncate(time.Minute)
	ordered := append([]usageSession(nil), sessions...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	var out []model.Sample
	for _, s := range ordered {
		tier := s.TraceTier
		if tier == "" {
			tier = "unknown" // never drop the count; bucket unknown-tier so it stays visible/attributable
		}
		labels := func() map[string]string {
			return map[string]string{cfg.sessionLabelKey: s.label(cfg.useName), "retention_tier": tier}
		}
		if s.RunCount != nil {
			out = append(out, model.Sample{
				Name: cfg.prefix + "_usage_traces", Kind: model.Gauge,
				Labels: labels(), Value: float64(*s.RunCount), Timestamp: stamp,
			})
		}
		if v, ok := spans[s.ID]; ok {
			out = append(out, model.Sample{
				Name: cfg.prefix + "_usage_spans", Kind: model.Gauge,
				Labels: labels(), Value: float64(v), Timestamp: stamp,
			})
		}
	}
	return out
}
