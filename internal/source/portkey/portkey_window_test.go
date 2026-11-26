// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

func spanLoop(w, settle, bootstrap, maxBackfill time.Duration, baseURL string) (*analyticsLoop, error) {
	src, err := New(config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: baseURL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(10 * time.Second), Window: config.Duration(w),
			BucketSettle: config.Duration(settle), BootstrapLookback: config.Duration(bootstrap),
			MaxBackfill: config.Duration(maxBackfill), MetricPrefix: "portkey_api",
			Graphs: []string{"requests"},
		}},
	}, source.Deps{})
	if err != nil {
		return nil, err
	}
	return src.Loops()[0].(*analyticsLoop), nil
}

// A window shorter than the 1m granularity floors back onto the watermark and never advances, so
// construction must reject it rather than accept a config that silently stalls.
func TestNewRejectsAnalyticsWindowShorterThanGranularity(t *testing.T) {
	_, err := spanLoop(30*time.Second, time.Second, 10*time.Minute, time.Hour, "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "window") || !strings.Contains(err.Error(), "granularity") {
		t.Fatalf("window 30s: want a granularity error naming the window, got %v", err)
	}
	if _, err := spanLoop(time.Minute, time.Second, 10*time.Minute, time.Hour, "http://127.0.0.1:1"); err != nil {
		t.Fatalf("window 1m must be accepted: %v", err)
	}
}

// bootstrap_lookback / max_backfill barely above bucket_settle leave a forward window only in the
// first second of each minute once until is floored. The bound is settle + granularity.
func TestNewRejectsLookbackBelowSettlePlusGranularity(t *testing.T) {
	for _, c := range []struct {
		name, key     string
		bootstrap, mb time.Duration
	}{
		{"bootstrap", "bootstrap_lookback", 61 * time.Second, 10 * time.Minute},
		{"max_backfill", "max_backfill", 10 * time.Minute, 61 * time.Second},
	} {
		_, err := spanLoop(5*time.Minute, time.Minute, c.bootstrap, c.mb, "http://127.0.0.1:1")
		if err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s 61s with settle 60s: want an error naming %s, got %v", c.name, c.key, err)
		}
	}
	if _, err := spanLoop(5*time.Minute, time.Minute, 2*time.Minute, 2*time.Minute, "http://127.0.0.1:1"); err != nil {
		t.Fatalf("exactly settle+granularity must be accepted: %v", err)
	}
}

// At the bound, every poll from a zero or stale watermark (start clamped to now-X) advances, at any
// sub-minute offset; one second below it the loop stalls on most polls (the rejected config).
func TestAnalyticsProgressAtLookbackBound(t *testing.T) {
	srv := rowFilterServer()
	defer srv.Close()
	run := func(x time.Duration) (advanced, polls int) {
		lp, err := spanLoop(5*time.Minute, time.Minute, 10*time.Minute, 10*time.Minute, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		lp.bootstrap, lp.maxBackfill = x, x // bypass the constructor to show the mechanism
		now := time.Date(2026, 6, 18, 14, 0, 7, 0, time.UTC)
		for i := 0; i < 30; i++ { // 5 simulated minutes at the 10s cadence
			lp.now = func() time.Time { return now }
			b, err := lp.Collect(context.Background(), model.Watermark{})
			if err != nil {
				t.Fatal(err)
			}
			polls++
			if len(b.Samples) > 0 {
				advanced++
			}
			now = now.Add(10 * time.Second)
		}
		return
	}
	if adv, polls := run(2 * time.Minute); adv != polls {
		t.Errorf("at settle+granularity: %d/%d polls advanced, want all", adv, polls)
	}
	if adv, polls := run(61 * time.Second); adv == polls {
		t.Errorf("below the bound every poll advanced (%d/%d): the bound is not tight", adv, polls)
	}
}
