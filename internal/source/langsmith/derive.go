// SPDX-License-Identifier: AGPL-3.0-only

// Package langsmith is the eval-platform source: the sessions loop derives per-session aggregate
// gauges (run counts, latency/cost/token aggregates, numeric feedback scores) from LangSmith's
// `GET /sessions?include_stats=true` endpoint. Aggregate-now (rolling snapshot), NOT time-bucketed —
// see docs/superpowers/specs/langsmith-poc.md. The runs/run-index → Loki logs loop is out of scope
// (content-leak release gate). Vendor specifics stay in this package (decoupling hard rule).
package langsmith

import (
	"sort"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// tracerSession decodes ONLY the fields we emit. Free-form/content + high-cardinality id fields
// (extra, description, run_facets, and every feedback stat's `values` map) are DELIBERATELY ABSENT:
// the JSON decoder skips unmapped fields, so those identifiers never enter process memory — content
// minimisation by construction (the no-content release gate), not by post-hoc filtering.
type tracerSession struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	RunCount      *int     `json:"run_count"`
	LatencyP50    *float64 `json:"latency_p50"` // SECONDS (live-confirmed); no ms conversion
	LatencyP99    *float64 `json:"latency_p99"`
	FirstTokenP50 *float64 `json:"first_token_p50"` // SECONDS; null when not streaming
	FirstTokenP99 *float64 `json:"first_token_p99"`

	TotalTokens      *int `json:"total_tokens"`
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`

	TotalCost      money `json:"total_cost"` // number (0.13.5) or string (0.16.5); null ⇒ unset
	PromptCost     money `json:"prompt_cost"`
	CompletionCost money `json:"completion_cost"`

	ErrorRate     *float64 `json:"error_rate"`     // ratio 0..1; real 0 is meaningful
	StreamingRate *float64 `json:"streaming_rate"` // ratio 0..1

	FeedbackStats map[string]feedbackStat `json:"feedback_stats"`
}

// feedbackStat decodes ONLY the numeric aggregate of a feedback key. The `values` map (raw category
// values — where ids like user_id / request_id / portkey_trace_id live), `type`, `stdev`, `errors`,
// and `contains_thread_feedback` are DELIBERATELY ABSENT, so the JSON decoder skips them and those
// identifiers never enter process memory. A nil Avg ⇒ categorical/id-like key ⇒ not emitted.
type feedbackStat struct {
	N   *int     `json:"n"`
	Avg *float64 `json:"avg"`
}

// deriveConfig is the pure derive's decoupled input: naming + label policy. No I/O, no clock.
type deriveConfig struct {
	prefix          string          // metric name prefix (e.g. "langsmith")
	sessionLabelKey string          // label key for the per-session dimension (e.g. "session")
	useName         bool            // label value = session name when true, else session id (default: id)
	emitFeedback    bool            // emit numeric feedback_stats facets
	feedbackAllow   map[string]bool // optional allow-list of feedback keys (nil/empty ⇒ all numeric)
}

// derive is PURE: (sessions, naming/label policy, poll time) → forward Gauge samples, all stamped at
// the minute-truncated `now` (aggregate-now snapshot model). Deterministic order (sessions by id, fixed
// facet order) for the emitter's byte-identical encode.
func derive(sessions []tracerSession, cfg deriveConfig, now time.Time) []model.Sample {
	// [review-M3] Stamp at MINUTE resolution. This is a snapshot loop: it emits a fresh point each poll,
	// so two polls in the same wall-clock minute (sub-minute cadence, or the ~60s+jitter edge) would emit
	// two DISTINCT sub-minute timestamps and push >1 point/series/minute past Mimir (CoalesceDPM is
	// per-batch, can't dedup across polls), violating 1DPM. Truncating makes same-minute re-polls share a
	// timestamp (LWW/duplicate-ts dedup ⇒ exactly 1DPM); the watermark liveness cursor stays precise
	// `now` (set in Collect). Mirrors the portkey groups loop. (followup §0 1DPM / §8 review-M3.)
	stamp := now.Truncate(time.Minute)
	ordered := append([]tracerSession(nil), sessions...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	var out []model.Sample
	for _, s := range ordered {
		sessVal := s.label(cfg.useName)
		gauge := func(suffix string, v float64, extra map[string]string) {
			labels := map[string]string{cfg.sessionLabelKey: sessVal}
			for k, val := range extra {
				labels[k] = val
			}
			out = append(out, model.Sample{
				Name:      cfg.prefix + "_" + suffix,
				Kind:      model.Gauge,
				Labels:    labels,
				Value:     v,
				Timestamp: stamp,
			})
		}
		quantiles := func(suffix string, p50, p99 *float64) {
			if p50 != nil {
				gauge(suffix, *p50, map[string]string{"quantile": "p50"})
			}
			if p99 != nil {
				gauge(suffix, *p99, map[string]string{"quantile": "p99"})
			}
		}
		intGauge := func(suffix string, p *int) {
			if p != nil {
				gauge(suffix, float64(*p), nil)
			}
		}
		floatGauge := func(suffix string, p *float64) {
			if p != nil {
				gauge(suffix, *p, nil)
			}
		}
		costGauge := func(suffix string, m money) {
			if m.set {
				gauge(suffix, m.v, nil)
			}
		}
		intGauge("runs", s.RunCount)
		quantiles("latency_seconds", s.LatencyP50, s.LatencyP99)
		quantiles("first_token_seconds", s.FirstTokenP50, s.FirstTokenP99)
		intGauge("tokens", s.TotalTokens)
		intGauge("prompt_tokens", s.PromptTokens)
		intGauge("completion_tokens", s.CompletionTokens)
		costGauge("cost_usd", s.TotalCost)
		costGauge("prompt_cost_usd", s.PromptCost)
		costGauge("completion_cost_usd", s.CompletionCost)
		floatGauge("error_rate", s.ErrorRate)
		floatGauge("streaming_rate", s.StreamingRate)

		if cfg.emitFeedback {
			keys := make([]string, 0, len(s.FeedbackStats))
			for k := range s.FeedbackStats {
				keys = append(keys, k)
			}
			sort.Strings(keys) // deterministic order for byte-identical encode
			for _, k := range keys {
				fs := s.FeedbackStats[k]
				if fs.Avg == nil { // categorical / id-like (avg null) ⇒ never emitted
					continue
				}
				if len(cfg.feedbackAllow) > 0 && !cfg.feedbackAllow[k] {
					continue // outside the configured allow-list
				}
				gauge("feedback_score", *fs.Avg, map[string]string{"feedback_key": k})
				if fs.N != nil {
					gauge("feedback_count", float64(*fs.N), map[string]string{"feedback_key": k})
				}
			}
		}
	}
	return out
}

// label returns the session's label value per policy: name when useName, else the stable id.
func (s tracerSession) label(useName bool) string {
	if useName {
		return s.Name
	}
	return s.ID
}
