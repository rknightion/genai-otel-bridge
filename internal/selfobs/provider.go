// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
	"encoding/base64"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

type ProviderConfig struct {
	Endpoint, InstanceID, Token string
	ServiceNamespace            string // DISTINCT from product (H4), e.g. "genai-otel-bridge-meta"
	Environment                 string
	Instance                    string // [CP-H8] per-replica id (POD_NAME) — diagnose leader overlap/disappearance
	Interval                    time.Duration
	MaxDPM                      int // self-plane DPM cap: the reader interval is clamped to ≥ 60s/MaxDPM. <1 ⇒ 1.
}

// NewProvider builds an OTLP-HTTP MeterProvider with a self-distinct resource identity and
// returns it plus a shutdown func. Self-telemetry may target a different endpoint than product.
func NewProvider(ctx context.Context, cfg ProviderConfig) (*metric.MeterProvider, func(context.Context) error, error) {
	// [CP-H9] Treat the config `endpoint` as the BASE (e.g. https://otlp-gateway-xxx.grafana.net/otlp)
	// and append /v1/metrics — IDENTICAL to the product emitter (otlp.New), so product and self
	// telemetry never diverge on path. Document that `endpoint` excludes /v1/metrics.
	exp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(strings.TrimRight(cfg.Endpoint, "/")+"/v1/metrics"),
		otlpmetrichttp.WithHeaders(otlpAuthHeaders(cfg.InstanceID, cfg.Token)),
	)
	if err != nil {
		return nil, nil, err
	}
	// [AR-C5] Build the resource with NewSchemaless + raw attribute keys. Do NOT
	// resource.Merge(resource.Default(), …): differing schema URLs return ErrSchemaURLConflict +
	// a blank resource → main.go fatal() → guaranteed startup crash. NewSchemaless also needs no
	// semconv import, sidestepping semconv-version churn (the keys are stable OTLP convention
	// strings). This is the minimal distinct self identity we want (H4).
	res := resource.NewSchemaless(
		attribute.String("service.namespace", cfg.ServiceNamespace),
		attribute.String("service.name", "genai-otel-bridge"),
		attribute.String("deployment.environment.name", cfg.Environment),
		attribute.String("service.instance.id", cfg.Instance), // [CP-H8] per-replica identity
	)
	floor := minSelfInterval(cfg.MaxDPM)
	interval := effectiveSelfInterval(cfg.Interval, cfg.MaxDPM)
	if cfg.Interval != 0 && cfg.Interval < floor {
		slog.Warn("selfobs: metric_interval below the DPM floor — clamped",
			"configured", cfg.Interval, "floor", floor, "max_dpm", cfg.MaxDPM)
	}
	mp := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exp, metric.WithInterval(interval))),
	)
	return mp, mp.Shutdown, nil
}

// minSelfInterval is the smallest export period that keeps the self plane at ≤ maxDPM points/minute:
// 60s / maxDPM. The PeriodicReader emits one point per metric per interval, so clamping the configured
// interval up to this floor ENFORCES the cap (vs merely relying on config being ≥60s — followup §0).
func minSelfInterval(maxDPM int) time.Duration {
	if maxDPM < 1 {
		maxDPM = 1
	}
	return time.Minute / time.Duration(maxDPM)
}

// effectiveSelfInterval resolves the reader interval: unset (0) ⇒ the floor; a configured value below
// the floor is clamped up (caller logs the clamp); at/above the floor is honoured.
func effectiveSelfInterval(configured time.Duration, maxDPM int) time.Duration {
	floor := minSelfInterval(maxDPM)
	if configured == 0 || configured < floor {
		return floor
	}
	return configured
}

// basicAuth builds the OTLP gateway Authorization header (instance:token, base64).
func basicAuth(id, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(id+":"+token))
}

// otlpAuthHeaders returns the exporter headers for the self-telemetry endpoint. [CP-M7] When BOTH
// instance_id and token are empty (the token-less in-cluster cleartext hop — the collector holds the
// real Grafana Cloud credentials), it returns no headers, so no credential-shaped value rides the link.
func otlpAuthHeaders(id, token string) map[string]string {
	if id == "" && token == "" {
		return nil
	}
	return map[string]string{"Authorization": basicAuth(id, token)}
}

// SetMemoryLimit sets GOMEMLIMIT to a fraction of the container limit so Go GC applies
// backpressure before the cgroup OOM-kills (DESIGN §5). No-op if inputs are non-positive.
func SetMemoryLimit(fraction float64, containerLimitBytes int64) {
	if fraction <= 0 || containerLimitBytes <= 0 {
		return
	}
	debug.SetMemoryLimit(int64(float64(containerLimitBytes) * fraction))
}
