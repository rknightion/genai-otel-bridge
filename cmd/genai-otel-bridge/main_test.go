// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

// TestLivenessThreshold pins the /healthz staleness derivation [CP-C5] and, critically, that only
// ENABLED sources/loops contribute [#88]: a disabled/parked slow loop must NOT inflate the threshold
// (else a wedged leader would pass liveness for hours). threshold == max(DegradedBackoff, slowest
// ENABLED cadence) + emitRetryBudget(2m) + livenessMargin(4m).
func TestLivenessThreshold(t *testing.T) {
	const budget = 6 * time.Minute // emitRetryBudget(2m) + livenessMargin(4m)
	dur := func(d time.Duration) config.Duration { return config.Duration(d) }
	loop := func(enabled bool, cadence time.Duration) config.LoopConfig {
		return config.LoopConfig{Enabled: enabled, Cadence: dur(cadence)}
	}

	cases := []struct {
		name string
		cfg  *config.Config
		want time.Duration
	}{
		{
			// Only fast ENABLED loops (< DegradedBackoff) ⇒ base floors at DegradedBackoff.
			name: "fast enabled loops floor at DegradedBackoff",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: true, Loops: map[string]config.LoopConfig{"analytics": loop(true, time.Minute)}},
			}},
			want: schedule.DegradedBackoff + budget,
		},
		{
			// [#88] A DISABLED slow loop (6h) alongside an enabled fast loop must be ignored — the
			// threshold stays at the DegradedBackoff floor, NOT 6h+6m.
			name: "disabled slow loop does not inflate threshold",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: true, Loops: map[string]config.LoopConfig{
					"analytics": loop(true, time.Minute),
					"eval":      loop(false, 6*time.Hour),
				}},
			}},
			want: schedule.DegradedBackoff + budget,
		},
		{
			// An ENABLED slow loop (> DegradedBackoff) DOES raise the base.
			name: "enabled slow loop raises base",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: true, Loops: map[string]config.LoopConfig{"eval": loop(true, 30*time.Minute)}},
			}},
			want: 30*time.Minute + budget,
		},
		{
			// A DISABLED SOURCE excludes its (even enabled) loops entirely.
			name: "disabled source excludes its loops",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: false, Loops: map[string]config.LoopConfig{"eval": loop(true, 6*time.Hour)}},
				{Enabled: true, Loops: map[string]config.LoopConfig{"analytics": loop(true, time.Minute)}},
			}},
			want: schedule.DegradedBackoff + budget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := livenessThreshold(tc.cfg); got != tc.want {
				t.Errorf("livenessThreshold = %v, want %v", got, tc.want)
			}
		})
	}
}
