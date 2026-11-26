// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// A window shorter than the 1m granularity floors back onto the watermark and never advances, so
// construction must reject it rather than accept a config that silently stalls.
func TestNewRejectsAnalyticsWindowShorterThanGranularity(t *testing.T) {
	mk := func(w time.Duration) error {
		_, err := New(config.SourceConfig{
			Type: "portkey", Enabled: true, BaseURL: "http://127.0.0.1:1", SourceInstance: "pk-test",
			Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
			RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
			HTTP:      config.HTTPConfig{AllowPrivate: true},
			Loops: map[string]config.LoopConfig{"analytics": {
				Enabled: true, Cadence: config.Duration(10 * time.Second), Window: config.Duration(w),
				BucketSettle: config.Duration(time.Second), BootstrapLookback: config.Duration(10 * time.Minute),
				MaxBackfill: config.Duration(time.Hour), MetricPrefix: "portkey_api",
				Graphs: []string{"requests"},
			}},
		}, source.Deps{})
		return err
	}
	err := mk(30 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "window") || !strings.Contains(err.Error(), "granularity") {
		t.Fatalf("window 30s: want a granularity error naming the window, got %v", err)
	}
	if err := mk(time.Minute); err != nil {
		t.Fatalf("window 1m must be accepted: %v", err)
	}
}
