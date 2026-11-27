// SPDX-License-Identifier: Apache-2.0

// Signals is the static catalogue of self-observability signals this package emits. It is the
// source the docs generator renders into docs/telemetry.md, and is held in lockstep with the live
// instruments in metrics.go by TestSelfObsSignalsParity (a new/changed instrument fails the gate
// until this list and the generated doc are updated).
package selfobs

import "github.com/rknightion/genai-otel-bridge/internal/docs/signal"

func selfMetric(name, instrument, unit, desc string, attrs ...string) signal.Signal {
	// metrics.go creates every counter with no unit (mk has no WithUnit), so a counter's wire unit is
	// always empty whatever the descriptor passes; record that rather than the "1" the callers write.
	if instrument == "counter" {
		unit = ""
	}
	return signal.Signal{
		Plane: signal.PlaneSelf, Type: signal.KindMetric, Source: "selfobs",
		Name: "genai_otel_bridge_" + name, Instrument: instrument, Unit: unit,
		Description: desc, Attributes: attrs,
	}
}

// Signals lists every self-metric with every label it can carry. tenant and pipeline appear only on the
// series of a (*Metrics).With copy, that is in the tenant shape; leader and the two memory gauges are
// per process and never carry them [#521].
func Signals() []signal.Signal {
	return []signal.Signal{
		selfMetric("emitted_total", "counter", "1", "samples emitted", "source", "loop", "tenant", "pipeline"),
		selfMetric("emitted_logs_total", "counter", "1", "log records emitted (logs-export loop)", "source", "loop", "tenant", "pipeline"),
		selfMetric("samples_skipped_total", "counter", "1", "data points or log records skipped with a counted gap", "source", "loop", "reason", "tenant", "pipeline"),
		selfMetric("emit_errors_total", "counter", "1", "emit errors by kind", "source", "loop", "kind", "tenant", "pipeline"),
		selfMetric("guard_dropped_total", "counter", "1", "data points or log records dropped by the governance guard", "source", "loop", "tenant", "pipeline"),
		selfMetric("bucket_revised_after_settle_total", "counter", "1", "settled buckets observed to change value after settle (late arrival beyond bucket_settle)", "source", "loop", "tenant", "pipeline"),
		selfMetric("new_label_values_total", "counter", "1", "new label-value combinations seen per series", "series", "tenant", "pipeline"),
		selfMetric("samples_capped_total", "counter", "1", "samples suppressed by the DPM cap (coalesced last-write-wins per series-minute)", "source", "loop", "reason", "tenant", "pipeline"),
		selfMetric("source_capability_total", "counter", "1", "source capability or permission condition by graph and closed state: endpoint-absent, plan-unsupported, permission-denied, no-data, transient-404, or schema-changed. endpoint-absent and plan-unsupported are enumerated but currently producerless because one 404 cannot distinguish them from a transient failure; steady versus intermittent increments remain a query-time judgement", "source", "loop", "graph", "state", "tenant", "pipeline"),
		selfMetric("source_data_incomplete_total", "counter", "1", "source data was incomplete for a closed reason: window_truncated, sessions_truncated, backfill_skipped, window_oversize, span_stats_unavailable, duplicate_dimension, trace_id_unparsed, line_oversize, line_unparseable, export_stuck, export_failed, or endpoint_unavailable", "source", "loop", "reason", "tenant", "pipeline"),
		selfMetric("auth_errors_total", "counter", "1", "upstream source API responded 401/403 — a credential failure", "loop", "source", "tenant", "pipeline"),
		selfMetric("emit_partial_success_rejected_total", "counter", "1", "data points or log records the gateway rejected via an OTLP 200 partial_success response (rejected_data_points/rejected_log_records)", "plane", "tenant", "pipeline"),
		selfMetric("last_success_timestamp_seconds", "gauge", "s", "unix time of the last fully handled window", "source", "loop", "tenant", "pipeline"),
		selfMetric("window_lag_seconds", "gauge", "s", "now minus the watermark frontier", "source", "loop", "tenant", "pipeline"),
		selfMetric("queue_depth", "gauge", "1", "1 while a batch is queued or being emitted, else 0", "source", "loop", "tenant", "pipeline"),
		selfMetric("loop_degraded", "gauge", "1", "1 while a loop is degraded (reason attribute), 0 after the clearing commit or when leadership ends", "source", "loop", "reason", "tenant", "pipeline"),
		selfMetric("scope_state", "gauge", "", "1 for the scope gate's current state (matched, unverified or mismatched) and 0 for the others; all 0 with no result, which is what a replica that is not leading reports; tenant shape only", "state", "tenant", "pipeline"),
		selfMetric("scope_assertion_total", "counter", "1", "scope assertion calls by result: matched, mismatch, unverifiable or error; tenant shape only", "result", "tenant", "pipeline"),
		selfMetric("tenant_blocked", "gauge", "", "1 while a tenant is not built for the reason (secret_unreadable: a secret file could not be read; duplicate_key: a target's source API key is also another target's), else 0; recorded at assembly by every replica from its own read of the secrets; tenant shape only, never labelled with pipeline", "tenant", "reason"),
		selfMetric("leader", "gauge", "1", "1 while this replica holds leadership, 0 while it is a standby; recorded by every replica from startup"),
		selfMetric("go_heap_inuse_bytes", "gauge", "By", "bytes in in-use heap spans (runtime.MemStats.HeapInuse)"),
		selfMetric("go_memory_limit_bytes", "gauge", "By", "the Go runtime soft memory limit (GOMEMLIMIT); 0 when no limit is set"),
		selfMetric("upstream_request_duration_seconds", "histogram", "s", "base2 exponential histogram: outbound request latency to upstream source APIs (time to response headers)", "target", "method", "status_class", "tenant", "pipeline"),
		selfMetric("emit_request_duration_seconds", "histogram", "s", "base2 exponential histogram: outbound OTLP emit request latency (per POST attempt to /v1/metrics or /v1/logs)", "plane", "status_class", "tenant", "pipeline"),
		selfMetric("bucket_revised_after_settle_age_seconds", "histogram", "s", "base2 exponential histogram: age (now − bucketEnd) of a settled bucket observed to change after bucket_settle", "loop", "tenant", "pipeline"),
		{
			Plane: signal.PlaneSelf, Type: signal.KindTrace, Source: "selfobs",
			Name:        "genai-otel-bridge/selfobs (tracer scope)",
			Description: "the bridge's own internal spans, exported via the self-observability OTLP TracerProvider (internal/selfobs/tracing.go)",
			DependsOn:   "self-observability tracing enabled in config",
		},
	}
}
