// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/loopid"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// LoopID identifies one loop instance on the self plane [#387]: Source is the configured
// source_instance and Loop the loop's metrics name, Key().Loop or the MetricsLoopName of a loop whose
// instances share a Key().Loop (the logs_export use-case fan-out). Both come from configuration, never
// from upstream data, so the {source, loop} label set is bounded by configuration. It aliases the leaf
// loopid.ID so selfobs can implement Metrics without importing this package.
type LoopID = loopid.ID

// MetricsLoopID derives a loop's self-metrics identity.
func MetricsLoopID(l source.Loop) LoopID {
	k := l.Key()
	id := LoopID{Source: k.SourceInstance, Loop: k.Loop}
	if n, ok := l.(source.MetricsLoopNamer); ok {
		if name := n.MetricsLoopName(); name != "" {
			id.Loop = name
		}
	}
	return id
}

// Metrics is the self-observability seam (selfobs implements it; cmd wires it in). Defined here
// so schedule does not import selfobs (no cycle). The methods the runner and scheduler record take a
// LoopID, so two loop instances never share a series; the hooks the composition root wires to source
// packages (BucketRevisedAfterSettle, SourceCapability, SourceDataIncomplete, AuthError) take the
// same source instance and the source's bare loop name.
type Metrics interface {
	EmittedSamples(id LoopID, n int)
	EmittedLogs(id LoopID, n int) // log records successfully emitted (distinct from metric samples)
	SamplesSkipped(id LoopID, reason string, n int)
	EmitError(id LoopID, kind string) // kind: bad_encoding | retryable_exhausted | checkpoint_* | ...
	GuardDropped(id LoopID, n int)
	BucketRevisedAfterSettle(loop, source string, age time.Duration) // age = how late (now − bucketEnd); records count + age histogram
	QueueDepth(id LoopID, depth int)
	LastSuccess(id LoopID, t time.Time)
	WindowLag(id LoopID, lag time.Duration)
	NewLabelValue(series string)
	SamplesCapped(id LoopID, n int) // samples suppressed by the DPM cap (reason="dpm")
	SourceCapability(loop, source, graph, state string)
	SourceDataIncomplete(loop, source, reason string)
	AuthError(loop, source string)                        // upstream responded 401/403 — credential failure, own alertable signal
	LoopDegraded(id LoopID, reason string, degraded bool) // [#120] 0/1 gauge: loop entered/left degraded (non-flapping state)
}

// NoopMetrics is the default (tests, dev).
type NoopMetrics struct{}

func (NoopMetrics) EmittedSamples(LoopID, int)                             {}
func (NoopMetrics) EmittedLogs(LoopID, int)                                {}
func (NoopMetrics) SamplesSkipped(LoopID, string, int)                     {}
func (NoopMetrics) EmitError(LoopID, string)                               {}
func (NoopMetrics) GuardDropped(LoopID, int)                               {}
func (NoopMetrics) BucketRevisedAfterSettle(string, string, time.Duration) {}
func (NoopMetrics) QueueDepth(LoopID, int)                                 {}
func (NoopMetrics) LastSuccess(LoopID, time.Time)                          {}
func (NoopMetrics) WindowLag(LoopID, time.Duration)                        {}
func (NoopMetrics) NewLabelValue(string)                                   {}
func (NoopMetrics) SamplesCapped(LoopID, int)                              {}
func (NoopMetrics) SourceCapability(string, string, string, string)        {}
func (NoopMetrics) SourceDataIncomplete(string, string, string)            {}
func (NoopMetrics) AuthError(string, string)                               {}
func (NoopMetrics) LoopDegraded(LoopID, string, bool)                      {}
