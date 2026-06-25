// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TracingConfig configures opt-in, default-off self-APM tracing of the integrator's OWN poll/emit
// pipeline (NOT the Portkey/LangSmith data). It is selfobs-owned (decoupled — selfobs does not import
// `config`; `main.go` maps the YAML into it, exactly as for ProviderConfig/ProfilingConfig). The
// endpoint/creds/identity intentionally MIRROR the metric provider's: traces ride the SAME Grafana
// Cloud OTLP gateway (→ Tempo) on the same `-meta` self identity — there is no separate egress channel.
type TracingConfig struct {
	Endpoint, InstanceID, Token string
	ServiceNamespace            string // DISTINCT from product (H4), e.g. "decant-meta"
	Environment                 string
	Instance                    string // [CP-H8] per-replica id (POD_NAME)
}

// NewTracerProvider builds an OTLP-HTTP TracerProvider on the self endpoint. The CALLER invokes this
// only when tracing is enabled (default-off lives in config/main); otherwise the OTel global stays the
// no-op tracer, so an un-instrumented build pays nothing. The sampler is AlwaysSample: our own pipeline
// is low-volume (a span per loop tick per loop, cadence ≥ 10s), so head-sampling everything is cheap and
// gives complete causal traces when an operator turns it on to chase a latency mystery.
//
// [CP-H9] `endpoint` is the BASE (e.g. https://otlp-gateway-xxx.grafana.net/otlp); /v1/traces is
// appended here — IDENTICAL to the metric provider's /v1/metrics, so product and self never diverge on
// path. [CP-M7] otlpAuthHeaders omits Authorization entirely when creds are empty (the token-less
// in-cluster cleartext hop), and an http:// endpoint URL selects the insecure transport by scheme.
func NewTracerProvider(ctx context.Context, cfg TracingConfig) (*sdktrace.TracerProvider, func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(strings.TrimRight(cfg.Endpoint, "/")+"/v1/traces"),
		otlptracehttp.WithHeaders(otlpAuthHeaders(cfg.InstanceID, cfg.Token)),
	)
	if err != nil {
		return nil, nil, err
	}
	// [AR-C5] NewSchemaless + raw OTLP key strings (NOT resource.Merge(resource.Default(), …), which can
	// return ErrSchemaURLConflict) — the same minimal distinct self identity as the metric provider (H4).
	res := resource.NewSchemaless(
		attribute.String("service.namespace", cfg.ServiceNamespace),
		attribute.String("service.name", "decant"),
		attribute.String("deployment.environment.name", cfg.Environment),
		attribute.String("service.instance.id", cfg.Instance),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	return tp, tp.Shutdown, nil
}
