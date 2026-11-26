// SPDX-License-Identifier: Apache-2.0

package app

import (
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

// telemetryAdapter wraps any schedule.Metrics as a PipelineTelemetry whose three request observers
// record nothing, which is what a test that passed a bare schedule.Metrics to Build saw before the
// factories existed (no emit observer on a hand-built emitter, no upstream observer in empty Deps).
type telemetryAdapter struct {
	schedule.Metrics
}

func (telemetryAdapter) ObserveUpstreamRequest(string, string, int, error, time.Duration) {}
func (telemetryAdapter) ObserveEmitRequest(string, int, error, time.Duration)             {}
func (telemetryAdapter) ObserveEmitPartialReject(string, int64)                           {}

// ScopeState and ScopeAssertion forward to the wrapped metrics when they have them, as SeedLoop does.
func (a telemetryAdapter) ScopeState(state string) {
	if s, ok := a.Metrics.(interface{ ScopeState(string) }); ok {
		s.ScopeState(state)
	}
}

func (a telemetryAdapter) ScopeAssertion(result string) {
	if s, ok := a.Metrics.(interface{ ScopeAssertion(string) }); ok {
		s.ScopeAssertion(result)
	}
}

// SeedScope forwards the optional scope-assertion seeding, as SeedLoop does.
func (a telemetryAdapter) SeedScope() {
	if s, ok := a.Metrics.(interface{ SeedScope() }); ok {
		s.SeedScope()
	}
}

// SeedLoop forwards to the wrapped metrics when they have the optional capability, so a recorder such
// as seed_test.go's seedRecorder still sees Build's seeding through the adapter.
func (a telemetryAdapter) SeedLoop(id schedule.LoopID, hookLoop string) {
	if s, ok := a.Metrics.(interface {
		SeedLoop(id schedule.LoopID, hookLoop string)
	}); ok {
		s.SeedLoop(id, hookLoop)
	}
}

// fixedFactories wraps a fixed emitter and metrics object as the two factories Build takes: every
// call returns the same emitter and the same (adapted) metrics, whatever the spec or pipeline.
func fixedFactories(em emit.Emitter, m schedule.Metrics) (EmitterFactory, TelemetryFactory) {
	t := telemetryAdapter{Metrics: m}
	return func(EmitterSpec, PipelineTelemetry) emit.Emitter { return em },
		func(string, string) PipelineTelemetry { return t }
}
