// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

// PipelineTelemetry is the self-telemetry one pipeline records through: the scheduler and runner seam
// plus the three request observers the emitter and the source HTTP client feed. *selfobs.Metrics (and
// its With copies) satisfies it, so this package still does not import selfobs. SeedLoop stays an
// optional capability: Build type-asserts it on the PipelineTelemetry value it is given.
type PipelineTelemetry interface {
	schedule.Metrics
	ObserveUpstreamRequest(target, method string, statusCode int, err error, d time.Duration)
	ObserveEmitRequest(plane string, statusCode int, err error, d time.Duration)
	ObserveEmitPartialReject(plane string, rejected int64)
}

// noopTelemetry records nothing. ValidateConfigFile passes it, so the config check builds the app
// without a self-telemetry provider.
type noopTelemetry struct{ schedule.NoopMetrics }

func (noopTelemetry) ObserveUpstreamRequest(string, string, int, error, time.Duration) {}
func (noopTelemetry) ObserveEmitRequest(string, int, error, time.Duration)             {}
func (noopTelemetry) ObserveEmitPartialReject(string, int64)                           {}

// TelemetryFactory returns the telemetry hooks for one tenant and pipeline. Both are empty in the
// legacy shape, where the hooks must record exactly today's label sets.
type TelemetryFactory func(tenant, pipeline string) PipelineTelemetry

// EmitterSpec is what one emitter is built from: the destination's endpoint and credentials, the
// product identity stamped on every resource, and the pipeline it serves. Tenant and Pipeline are
// empty for a legacy row.
type EmitterSpec struct {
	Endpoint, InstanceID, Token string
	Identity                    map[string]string
	Tenant, Pipeline            string
}

// EmitterFactory builds the emitter for one spec, reporting through that pipeline's telemetry.
type EmitterFactory func(EmitterSpec, PipelineTelemetry) emit.Emitter

// Options carries composition-root inputs that are neither config nor a factory. Empty for now; later
// units add fields (see the multi-tenancy design).
type Options struct{}

// partialRejectLimitKey is the rate-limit key of the partial-reject warning. With no pipeline it is
// today's per-plane key, so a legacy install throttles exactly as before; with one it carries the
// pipeline id, so one tenant's rejections cannot suppress another tenant's warning (design §8).
func partialRejectLimitKey(pipeline, plane string) string {
	if pipeline == "" {
		return "partial:" + plane
	}
	return "partial:" + pipeline + ":" + plane
}

// OTLPEmitterFactory is the production EmitterFactory: an OTLP/HTTP emitter per spec, with the emit-leg
// latency observer and the partial-success counter wired to the pipeline's telemetry, and the
// partial-reject warning rate-limited through warn. The emitter is loop-agnostic and owns neither the
// metric nor the log line, so it calls back here.
func OTLPEmitterFactory(maxBatchBytes int, retry otlp.RetryPolicy, warn *logging.Limiter) EmitterFactory {
	return func(spec EmitterSpec, t PipelineTelemetry) emit.Emitter {
		return otlp.New(otlp.Config{
			Endpoint: spec.Endpoint, InstanceID: spec.InstanceID, Token: spec.Token, MaxBytes: maxBatchBytes,
			// Single source of truth (config.IdentityConfig.ProductIdentity): Build counts the same keys
			// against governance.max_stream_label_keys (Loki stream-label budget), so they cannot drift.
			Identity: spec.Identity,
			Retry:    retry,
			// [#60] Emit-leg latency into self-telemetry (the emit client is outside the httpx observer
			// chokepoint).
			Observer: t.ObserveEmitRequest,
			// [#80] A 200 + partial_success response silently drops N items past the reject taxonomy.
			// Count them into an alertable self-metric AND emit a rate-limited WARN (a persistent
			// partial-reject would otherwise log every emit).
			OnPartialReject: func(plane string, rejected int64, msg string) {
				t.ObserveEmitPartialReject(plane, rejected)
				if warn.Allow(partialRejectLimitKey(spec.Pipeline, plane)) {
					attrs := []any{"plane", plane, "rejected", rejected, "gateway_message", msg}
					if spec.Pipeline != "" {
						attrs = append(attrs, "tenant", spec.Tenant, "pipeline", spec.Pipeline)
					}
					slog.Warn("OTLP gateway rejected part of an emit batch via a 200 partial_success response; the rejected data was NOT delivered (alert on genai_otel_bridge_emit_partial_success_rejected_total)",
						attrs...)
				}
			},
		})
	}
}
