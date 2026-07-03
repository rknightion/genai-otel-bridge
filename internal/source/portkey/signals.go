// SPDX-License-Identifier: AGPL-3.0-only

// Signals enumerates the Portkey product telemetry this source can emit. Names are config-derived,
// so descriptors carry {placeholder} templates and a DependsOn note rather than literal names. The
// metric set is held consistent with derive.go's metricSuffix map and labels.go's AllowedLabelKeys
// by TestPortkeySignalsCoverSuffixesAndLabels.
package portkey

import "github.com/rknightion/genai-otel-bridge/internal/docs/signal"

// Verified against derive.go (metricSuffix: requests/cost_usd/tokens/latency_seconds/errors/users),
// groups_derive.go (groupMetricName = <prefix>_<metric>_by_<dim>, cost in USD cents ÷100),
// groups.go (emit_cost / emit_prompts settings), and labels.go AllowedLabelKeys (quantile, token_type,
// ai_model, ai_org, response_status_code, metadata_key, metadata_value, prompt, api_key_use_case).
func Signals() []signal.Signal {
	const px = "{loops.analytics.metric_prefix}"
	m := func(name, instr, unit, desc, dep string, attrs ...string) signal.Signal {
		return signal.Signal{
			Plane: signal.PlaneProduct, Type: signal.KindMetric, Source: "portkey",
			Name: name, Instrument: instr, Unit: unit, Description: desc,
			DependsOn: dep, Attributes: attrs,
		}
	}
	return []signal.Signal{
		// analytics loop — one series per configured graph (metricSuffix). Names end in the suffix the
		// parity test checks for. The 'graphs includes X' DependsOn matches the loops.analytics.graphs list.
		m(px+"_requests", "gauge", "1", "request count per bucket", "loops.analytics.graphs includes 'requests'"),
		m(px+"_cost_usd", "gauge", "USD", "request cost per bucket (US dollars)", "loops.analytics.graphs includes 'cost'"),
		m(px+"_tokens", "gauge", "1", "token units per bucket; split by token_type (total/input/output) — do NOT bare-sum across token_type", "loops.analytics.graphs includes 'tokens'", "token_type"),
		m(px+"_latency_seconds", "gauge", "s", "request latency statistic per bucket; one series per quantile (avg/p50/p90/p99)", "loops.analytics.graphs includes 'latency'", "quantile"),
		m(px+"_errors", "gauge", "1", "error count per bucket", "loops.analytics.graphs includes 'errors'"),
		m(px+"_users", "gauge", "1", "distinct-user count per bucket", "loops.analytics.graphs includes 'users'"),
		// groups loop — per-dimension aggregates, name shape <prefix>_<metric>_by_<dim>. ai-models and
		// metadata dimensions; cost gauge gated by emit_cost; prompt dimension gated by emit_prompts.
		m(px+"_requests_by_model", "gauge", "1", "request count per AI model (groups ai-models dimension)", "loops.groups.enabled=true", "ai_model"),
		m(px+"_cost_usd_by_model", "gauge", "USD", "cost per AI model (÷100 from Portkey cents)", "loops.groups settings.emit_cost=true", "ai_model"),
		m(px+"_requests_by_metadata", "gauge", "1", "request count per metadata-dimension value", "loops.groups enabled with a metadata dimension", "metadata_key", "metadata_value"),
		m(px+"_cost_usd_by_metadata", "gauge", "USD", "cost per metadata-dimension value (÷100 from Portkey cents)", "loops.groups settings.emit_cost=true with a metadata dimension", "metadata_key", "metadata_value"),
		m(px+"_requests_by_prompt", "gauge", "1", "request count per saved-prompt id (content-free — the label is a prompt ID, not text)", "loops.groups settings.emit_prompts=true", "prompt"),
		// logs_export loop — one content-free OTLP log per request.
		{
			Plane: signal.PlaneProduct, Type: signal.KindLog, Source: "portkey",
			Name: "portkey logs_export record", Description: "one content-free OTLP log per request: status/latency/tokens/cost as structured metadata; no prompt/response bodies",
			DependsOn:  "loops.logs_export.enabled=true",
			Attributes: []string{"ai_org", "ai_model", "response_status_code", "api_key_use_case"},
		},
	}
}
