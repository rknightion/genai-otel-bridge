// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics implements schedule.Metrics over the OTel-Go SDK (its sweet spot — instrumenting live
// code), distinct from the hand-encoded emitter used for republished external series.
type Metrics struct {
	emitted, emittedLogs, skipped, emitErr, guardDropped, revised, newLabel, capped, srcGraphUnavail, authErr metric.Int64Counter
	lastSuccess, windowLag, queueDepth                                                                        metric.Float64Gauge
	upstreamDur, revisedAge                                                                                   metric.Float64Histogram
}

func NewMetrics(mp metric.MeterProvider) (*Metrics, error) {
	me := mp.Meter("decant/selfobs")
	var err error
	m := &Metrics{}
	mk := func(n, desc string) metric.Int64Counter {
		c, e := me.Int64Counter("decant_"+n, metric.WithDescription(desc))
		if e != nil {
			err = e
		}
		return c
	}
	mg := func(n, desc, unit string) metric.Float64Gauge {
		g, e := me.Float64Gauge("decant_"+n, metric.WithDescription(desc), metric.WithUnit(unit))
		if e != nil {
			err = e
		}
		return g
	}
	mh := func(n, desc, unit string, bounds []float64) metric.Float64Histogram {
		h, e := me.Float64Histogram("decant_"+n, metric.WithDescription(desc), metric.WithUnit(unit), metric.WithExplicitBucketBoundaries(bounds...))
		if e != nil {
			err = e
		}
		return h
	}
	m.emitted = mk("emitted_total", "samples emitted")
	m.emittedLogs = mk("emitted_logs_total", "log records emitted (logs-export loop)")
	m.skipped = mk("samples_skipped_total", "data points or log records skipped with a counted gap")
	m.emitErr = mk("emit_errors_total", "emit errors by kind")
	m.guardDropped = mk("guard_dropped_total", "data points or log records dropped by the governance guard")
	m.revised = mk("bucket_revised_after_settle_total", "settled buckets observed to change value after settle (late arrival beyond bucket_settle)")
	m.newLabel = mk("new_label_values_total", "new label-value combinations seen per series")
	m.capped = mk("samples_capped_total", "samples suppressed by the DPM cap (coalesced last-write-wins per series-minute)")
	m.srcGraphUnavail = mk("source_graph_unavailable_total", "configured source graph skipped on a poll due to a 404 (capability detection / permission / absence) — steady increments ⇒ permanently absent, intermittent ⇒ flapping")
	m.authErr = mk("auth_errors_total", "upstream source API responded 401/403 — a credential failure (wrong/expired key, missing scope) distinct from a slow/erroring endpoint; alert on rate(...) > 0")
	m.lastSuccess = mg("last_success_timestamp_seconds", "unix time of last successful emit", "s")
	m.windowLag = mg("window_lag_seconds", "now minus the watermark frontier", "s")
	m.queueDepth = mg("queue_depth", "per-loop queue depth", "1")
	// Upstream-API request latency (self-obs): how slow/erroring the APIs we POLL are — distinct from
	// the latency the product plane republishes. Bucketed by {target,method,status_class}. Boundaries
	// are in SECONDS (the default OTel boundaries are ms-shaped and wrong for a _seconds histogram).
	m.upstreamDur = mh("upstream_request_duration_seconds", "outbound request latency to upstream source APIs (time to response headers)", "s",
		[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})
	// How LATE each post-settle bucket revision is (now − bucketEnd, SECONDS). Pairs with the
	// bucket_revised_after_settle_total counter: the counter says how often, this says how late, so
	// bucket_settle can be tuned to data (e.g. p95 of this) instead of a guess. Age is ≥ bucket_settle
	// by construction and bounded by the detection band (≈2×settle), so boundaries span ~5m–1h.
	m.revisedAge = mh("bucket_revised_after_settle_age_seconds", "age (now − bucketEnd) of a settled bucket observed to change after bucket_settle — how late the late arrival is", "s",
		[]float64{300, 600, 900, 1200, 1500, 1800, 2400, 3000, 3600})
	return m, err
}

func loopAttr(loop string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("loop", loop))
}

func (m *Metrics) EmittedSamples(loop string, n int) {
	m.emitted.Add(context.Background(), int64(n), loopAttr(loop))
}
func (m *Metrics) EmittedLogs(loop string, n int) {
	m.emittedLogs.Add(context.Background(), int64(n), loopAttr(loop))
}
func (m *Metrics) SamplesSkipped(loop, reason string, n int) {
	m.skipped.Add(context.Background(), int64(n), metric.WithAttributes(attribute.String("loop", loop), attribute.String("reason", reason)))
}
func (m *Metrics) EmitError(loop, kind string) {
	m.emitErr.Add(context.Background(), 1, metric.WithAttributes(attribute.String("loop", loop), attribute.String("kind", kind)))
}
func (m *Metrics) GuardDropped(loop string, n int) {
	m.guardDropped.Add(context.Background(), int64(n), loopAttr(loop))
}
func (m *Metrics) BucketRevisedAfterSettle(loop string, age time.Duration) {
	m.revised.Add(context.Background(), 1, loopAttr(loop))
	m.revisedAge.Record(context.Background(), age.Seconds(), loopAttr(loop))
}
func (m *Metrics) NewLabelValue(series string) {
	m.newLabel.Add(context.Background(), 1, metric.WithAttributes(attribute.String("series", series)))
}
func (m *Metrics) SamplesCapped(loop string, n int) {
	m.capped.Add(context.Background(), int64(n), metric.WithAttributes(
		attribute.String("loop", loop), attribute.String("reason", "dpm")))
}
func (m *Metrics) SourceGraphUnavailable(loop, graph string) {
	m.srcGraphUnavail.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("loop", loop), attribute.String("graph", graph)))
}
func (m *Metrics) AuthError(loop, source string) {
	m.authErr.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("loop", loop), attribute.String("source", source)))
}
func (m *Metrics) QueueDepth(loop string, depth int) {
	m.queueDepth.Record(context.Background(), float64(depth), loopAttr(loop))
}
func (m *Metrics) LastSuccess(loop string, t time.Time) {
	m.lastSuccess.Record(context.Background(), float64(t.Unix()), loopAttr(loop))
}
func (m *Metrics) WindowLag(loop string, lag time.Duration) {
	m.windowLag.Record(context.Background(), lag.Seconds(), loopAttr(loop))
}

// ObserveUpstreamRequest records one outbound upstream-API request: its duration bucketed by
// {target host, method, status_class}. status_class is the response class (2xx/3xx/4xx/5xx), or
// "error" when no response was received — low-cardinality on purpose (never the raw code or path).
// Wired from httpx.Observer at the composition root, so selfobs stays decoupled from the HTTP client.
func (m *Metrics) ObserveUpstreamRequest(target, method string, statusCode int, err error, d time.Duration) {
	class := "error"
	if err == nil && statusCode > 0 {
		class = fmt.Sprintf("%dxx", statusCode/100)
	}
	m.upstreamDur.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String("target", target),
		attribute.String("method", method),
		attribute.String("status_class", class),
	))
}
