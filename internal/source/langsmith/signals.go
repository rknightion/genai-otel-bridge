// SPDX-License-Identifier: AGPL-3.0-only

// Signals enumerates the LangSmith product telemetry this source can emit. Names are config-derived
// (the metric prefix and session label key are configurable), so descriptors use {placeholder}
// templates. Coverage of AllowedLabelKeys is enforced by TestLangSmithSignalsCoverLabels.
package langsmith

import "github.com/rknightion/genai-otel-bridge/internal/docs/signal"

// Verified against derive.go (the sessions-loop gauges) and labels.go AllowedLabelKeys (quantile,
// session, feedback_key, run_type, status). NOTE: the per-session label key is FIXED to "session"
// (settings.go: session_label_key is deliberately NOT settable) — render the literal, not a template.
func Signals() []signal.Signal {
	const px = "{loops.sessions.metric_prefix}" // configurable, default "langsmith"
	const sess = "session"                      // FIXED, not configurable
	const dep = "loops.sessions.enabled=true"
	m := func(suffix, unit, desc string, attrs ...string) signal.Signal {
		return signal.Signal{
			Plane: signal.PlaneProduct, Type: signal.KindMetric, Source: "langsmith",
			Name: px + "_" + suffix, Instrument: "gauge", Unit: unit, Description: desc,
			DependsOn: dep, Attributes: append([]string{sess}, attrs...),
		}
	}
	return []signal.Signal{
		m("runs", "1", "per-session run count (aggregate-now snapshot)"),
		m("latency_seconds", "s", "per-session run latency; one series per quantile (p50/p99)", "quantile"),
		m("first_token_seconds", "s", "per-session time-to-first-token; one series per quantile (p50/p99); absent when not streaming", "quantile"),
		m("tokens", "1", "per-session total token count"),
		m("prompt_tokens", "1", "per-session prompt (input) token count"),
		m("completion_tokens", "1", "per-session completion (output) token count"),
		m("cost_usd", "USD", "per-session total cost (US dollars)"),
		m("prompt_cost_usd", "USD", "per-session prompt cost (US dollars)"),
		m("completion_cost_usd", "USD", "per-session completion cost (US dollars)"),
		m("error_rate", "1", "per-session error rate (ratio)"),
		m("streaming_rate", "1", "per-session streaming rate (ratio)"),
		{
			Plane: signal.PlaneProduct, Type: signal.KindMetric, Source: "langsmith",
			Name: px + "_feedback_score", Instrument: "gauge", Unit: "1",
			Description: "per-session numeric feedback aggregate; one series per feedback_key",
			DependsOn:   "loops.sessions settings.emit_feedback=true", Attributes: []string{sess, "feedback_key"},
		},
		{
			Plane: signal.PlaneProduct, Type: signal.KindMetric, Source: "langsmith",
			Name: px + "_feedback_count", Instrument: "gauge", Unit: "1",
			Description: "per-session numeric feedback sample count; one series per feedback_key",
			DependsOn:   "loops.sessions settings.emit_feedback=true", Attributes: []string{sess, "feedback_key"},
		},
		{
			Plane: signal.PlaneProduct, Type: signal.KindLog, Source: "langsmith",
			Name: "langsmith runs record", Description: "one content-free OTLP log per run: type/status/latency/tokens/cost as structured metadata; no inputs/outputs",
			DependsOn:  "loops.runs.enabled=true",
			Attributes: []string{"run_type", "status"},
		},
		{
			Plane: signal.PlaneProduct, Type: signal.KindMetric, Source: "langsmith",
			Name: "{loops.usage.metric_prefix}_usage_traces", Instrument: "gauge", Unit: "1",
			Description: "PLATFORM cost driver: traces (root runs) ingested per project = the LangSmith billing unit; one series per retention_tier",
			DependsOn:   "loops.usage.enabled=true", Attributes: []string{"session", "retention_tier"},
		},
		{
			Plane: signal.PlaneProduct, Type: signal.KindMetric, Source: "langsmith",
			Name: "{loops.usage.metric_prefix}_usage_spans", Instrument: "gauge", Unit: "1",
			Description: "PLATFORM cost driver: spans (all runs) ingested per project = the storage/volume driver; one series per retention_tier",
			DependsOn:   "loops.usage settings.emit_span_counts=true", Attributes: []string{"session", "retention_tier"},
		},
	}
}
