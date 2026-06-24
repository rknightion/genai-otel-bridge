// SPDX-License-Identifier: AGPL-3.0-only

package schedule

import "time"

// Metrics is the self-observability seam (selfobs implements it; cmd wires it in). Defined here
// so schedule does not import selfobs (no cycle).
type Metrics interface {
	EmittedSamples(loop string, n int)
	EmittedLogs(loop string, n int) // log records successfully emitted (distinct from metric samples)
	SamplesSkipped(loop, reason string, n int)
	EmitError(loop, kind string) // kind: bad_encoding | retryable_exhausted
	GuardDropped(loop string, n int)
	BucketRevisedAfterSettle(loop string)
	QueueDepth(loop string, depth int)
	LastSuccess(loop string, t time.Time)
	WindowLag(loop string, lag time.Duration)
	NewLabelValue(series string)
	SamplesCapped(loop string, n int)          // samples suppressed by the DPM cap (reason="dpm")
	SourceGraphUnavailable(loop, graph string) // a configured graph skipped (404 capability/absence) this poll
	AuthError(loop, source string)             // upstream responded 401/403 — credential failure, own alertable signal
}

// NoopMetrics is the default (tests, dev).
type NoopMetrics struct{}

func (NoopMetrics) EmittedSamples(string, int)            {}
func (NoopMetrics) EmittedLogs(string, int)               {}
func (NoopMetrics) SamplesSkipped(string, string, int)    {}
func (NoopMetrics) EmitError(string, string)              {}
func (NoopMetrics) GuardDropped(string, int)              {}
func (NoopMetrics) BucketRevisedAfterSettle(string)       {}
func (NoopMetrics) QueueDepth(string, int)                {}
func (NoopMetrics) LastSuccess(string, time.Time)         {}
func (NoopMetrics) WindowLag(string, time.Duration)       {}
func (NoopMetrics) NewLabelValue(string)                  {}
func (NoopMetrics) SamplesCapped(string, int)             {}
func (NoopMetrics) SourceGraphUnavailable(string, string) {}
func (NoopMetrics) AuthError(string, string)              {}
