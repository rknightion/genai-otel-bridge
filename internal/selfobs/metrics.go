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
	lastSuccess, windowLag, queueDepth, loopDegraded                                                          metric.Float64Gauge
	upstreamDur, revisedAge, emitDur                                                                          metric.Float64Histogram
}

func NewMetrics(mp metric.MeterProvider) (*Metrics, error) {
	me := mp.Meter("genai-otel-bridge/selfobs")
	var err error
	m := &Metrics{}
	mk := func(n, desc string) metric.Int64Counter {
		c, e := me.Int64Counter("genai_otel_bridge_"+n, metric.WithDescription(desc))
		if e != nil {
			err = e
		}
		return c
	}
	mg := func(n, desc, unit string) metric.Float64Gauge {
		g, e := me.Float64Gauge("genai_otel_bridge_"+n, metric.WithDescription(desc), metric.WithUnit(unit))
		if e != nil {
			err = e
		}
		return g
	}
	mh := func(n, desc, unit string, bounds []float64) metric.Float64Histogram {
		h, e := me.Float64Histogram("genai_otel_bridge_"+n, metric.WithDescription(desc), metric.WithUnit(unit), metric.WithExplicitBucketBoundaries(bounds...))
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
	// [#120] Degraded-state gauge: 1 while a loop is in the degraded state (with the degrade reason as an
	// attribute), 0 after the clearing commit. The state machine previously only logged + flipped an
	// internal bool, so "which loop has been degraded, since when, for what reason" was inferable only by
	// correlating log lines — and the 10m DegradedBackoff meant the triggering error counter ticked ≤1 per
	// 10m, making an increase[10m] alert flap. A 0/1 gauge answers it in one query and supports a
	// non-flapping `== 1 for 15m` alert. Bounded cardinality: the reason attr is the fixed enterDegraded set.
	m.loopDegraded = mg("loop_degraded", "1 while a loop is degraded (reason attribute), 0 after the clearing commit", "1")
	// Upstream-API request latency (self-obs): how slow/erroring the APIs we POLL are — distinct from
	// the latency the product plane republishes. Bucketed by {target,method,status_class}. Boundaries
	// are in SECONDS (the default OTel boundaries are ms-shaped and wrong for a _seconds histogram).
	// [#121] Top boundaries extended past 10s to 20/30/60: the LangSmith client timeout is 30s (and its
	// include_stats calls are documented-slow), and the portkey logs_export DOWNLOAD client (5m timeout)
	// shares this instrument — so a 11s→29s degradation, one step from timeout, used to be invisible
	// (everything above 10s pinned in +Inf, so histogram_quantile p95/p99 could not resolve it). Two/three
	// extra buckets × the existing label set is negligible cardinality. (Downloads' 5-minute regime is only
	// coarsely covered by the 60s ceiling; a dedicated coarser download instrument is a tracked follow-up.)
	m.upstreamDur = mh("upstream_request_duration_seconds", "outbound request latency to upstream source APIs (time to response headers)", "s",
		[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60})
	// [#60] Emit-leg POST latency (encode+gzip+POST+retry attempt), bucketed by {plane,status_class}.
	// The emit POST uses a plain http.Client (30s timeout), NOT the httpx chokepoint feeding upstreamDur,
	// so without this the entire downstream half of the pipeline had zero latency observability — a
	// slowly-degrading gateway (creeping p99 that still succeeds in-budget) was invisible until it crossed
	// the retry budget and flipped to an error. Recorded via an observer injected at the composition root
	// (mirroring httpx.Observer) so emit and selfobs stay decoupled. Second-shaped buckets to 30s (the
	// emit client timeout), matching upstreamDur.
	m.emitDur = mh("emit_request_duration_seconds", "outbound OTLP emit request latency (per POST attempt to /v1/metrics or /v1/logs)", "s",
		[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30})
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

// LoopDegraded reports the loop's degraded state as a 0/1 gauge with the degrade reason as an
// attribute. [#120] The caller (schedule.LoopRunner) records 1 with the reason on entering degraded
// and 0 with the SAME reason on the clearing commit, so the {loop,reason} series returns to 0 rather
// than sticking at 1. reason is the bounded enterDegraded set (terminal emit reject / checkpoint-save
// failures) — never unbounded free text.
func (m *Metrics) LoopDegraded(loop, reason string, degraded bool) {
	v := 0.0
	if degraded {
		v = 1.0
	}
	m.loopDegraded.Record(context.Background(), v, metric.WithAttributes(
		attribute.String("loop", loop),
		attribute.String("reason", reason),
	))
}

// ObserveEmitRequest records one outbound emit POST attempt: its latency bucketed by {plane,
// status_class}. plane is "metrics" (/v1/metrics) or "logs" (/v1/logs). status_class is the response
// class (2xx/3xx/4xx/5xx), or "error" when no response was received — low-cardinality on purpose.
// [#60] Wired from the emitter via an injected observer at the composition root, mirroring
// ObserveUpstreamRequest, so emit and selfobs stay decoupled (neither imports the other).
func (m *Metrics) ObserveEmitRequest(plane string, statusCode int, err error, d time.Duration) {
	class := "error"
	if err == nil && statusCode > 0 {
		class = fmt.Sprintf("%dxx", statusCode/100)
	}
	m.emitDur.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String("plane", plane),
		attribute.String("status_class", class),
	))
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
