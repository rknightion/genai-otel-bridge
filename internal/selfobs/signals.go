// SPDX-License-Identifier: AGPL-3.0-only

// Signals is the static catalogue of self-observability signals this package emits. It is the
// source the docs generator renders into docs/telemetry.md, and is held in lockstep with the live
// instruments in metrics.go by TestSelfObsSignalsParity (a new/changed instrument fails the gate
// until this list and the generated doc are updated).
package selfobs

import "github.com/rknightion/genai-otel-bridge/internal/docs/signal"

func selfMetric(name, instrument, unit, desc string, attrs ...string) signal.Signal {
	return signal.Signal{
		Plane: signal.PlaneSelf, Type: signal.KindMetric, Source: "selfobs",
		Name: "genai_otel_bridge_" + name, Instrument: instrument, Unit: unit,
		Description: desc, Attributes: attrs,
	}
}

func Signals() []signal.Signal {
	return []signal.Signal{
		selfMetric("emitted_total", "counter", "1", "samples emitted", "loop"),
		selfMetric("emitted_logs_total", "counter", "1", "log records emitted (logs-export loop)", "loop"),
		selfMetric("samples_skipped_total", "counter", "1", "data points or log records skipped with a counted gap", "loop", "reason"),
		selfMetric("emit_errors_total", "counter", "1", "emit errors by kind", "loop", "kind"),
		selfMetric("guard_dropped_total", "counter", "1", "data points or log records dropped by the governance guard", "loop"),
		selfMetric("bucket_revised_after_settle_total", "counter", "1", "settled buckets observed to change value after settle (late arrival beyond bucket_settle)", "loop"),
		selfMetric("new_label_values_total", "counter", "1", "new label-value combinations seen per series", "series"),
		selfMetric("samples_capped_total", "counter", "1", "samples suppressed by the DPM cap (coalesced last-write-wins per series-minute)", "loop", "reason"),
		selfMetric("source_graph_unavailable_total", "counter", "1", "configured source graph skipped on a poll due to a 404 (capability/permission/absence)", "loop", "graph"),
		selfMetric("auth_errors_total", "counter", "1", "upstream source API responded 401/403 — a credential failure", "loop", "source"),
		selfMetric("last_success_timestamp_seconds", "gauge", "s", "unix time of last successful emit", "loop"),
		selfMetric("window_lag_seconds", "gauge", "s", "now minus the watermark frontier", "loop"),
		selfMetric("queue_depth", "gauge", "1", "per-loop queue depth", "loop"),
		selfMetric("loop_degraded", "gauge", "1", "1 while a loop is degraded (reason attribute), 0 after the clearing commit", "loop", "reason"),
		selfMetric("upstream_request_duration_seconds", "histogram", "s", "outbound request latency to upstream source APIs (time to response headers)", "target", "method", "status_class"),
		selfMetric("emit_request_duration_seconds", "histogram", "s", "outbound OTLP emit request latency (per POST attempt to /v1/metrics or /v1/logs)", "plane", "status_class"),
		selfMetric("bucket_revised_after_settle_age_seconds", "histogram", "s", "age (now − bucketEnd) of a settled bucket observed to change after bucket_settle", "loop"),
		{
			Plane: signal.PlaneSelf, Type: signal.KindTrace, Source: "selfobs",
			Name:        "genai-otel-bridge/selfobs (tracer scope)",
			Description: "the bridge's own internal spans, exported via the self-observability OTLP TracerProvider (internal/selfobs/tracing.go)",
			DependsOn:   "self-observability tracing enabled in config",
		},
	}
}
