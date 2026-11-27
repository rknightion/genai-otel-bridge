// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// rowFilterServer filters rows to [min,max] then buckets per wall-clock minute at one request per
// second, so a bucket's value is the number of seconds of it covered by the request (a cut bucket
// reads low, like a vendor that filters rows at second precision).
func rowFilterServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lo, _ := time.Parse(time.RFC3339, r.URL.Query().Get("time_of_generation_min"))
		hi, _ := time.Parse(time.RFC3339, r.URL.Query().Get("time_of_generation_max"))
		var pts []string
		for b := lo.Truncate(time.Minute); b.Before(hi); b = b.Add(time.Minute) {
			s, e := b, b.Add(time.Minute)
			if s.Before(lo) {
				s = lo
			}
			if e.After(hi) {
				e = hi
			}
			pts = append(pts, fmt.Sprintf(`{"timestamp":%q,"total":%d}`, b.UTC().Format("2006-01-02T15:04:05.000Z"), int(e.Sub(s).Seconds())))
		}
		_, _ = w.Write([]byte(`{"data_points":[` + strings.Join(pts, ",") + `],"is_quota_exceeded":false,"object":"analytics-graph"}`))
	}))
}

func alignmentLoop(t *testing.T, baseURL string, onRevised func(string, string, time.Duration)) *analyticsLoop {
	t.Helper()
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: baseURL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
			BucketSettle: config.Duration(10 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
			MaxBackfill: config.Duration(90 * time.Minute), MetricPrefix: "portkey_api",
			Graphs: []string{"requests"},
		}},
	}
	src, err := New(cfg, source.Deps{OnBucketRevised: onRevised})
	if err != nil {
		t.Fatal(err)
	}
	return src.Loops()[0].(*analyticsLoop)
}

// TestAnalyticsCatchupBoundaryBucketEmittedOnce: a catch-up across several windows from a mid-minute
// checkpoint emits every (series, bucket_end) exactly once, whole, with no gap between windows.
func TestAnalyticsCatchupBoundaryBucketEmittedOnce(t *testing.T) {
	srv := rowFilterServer()
	defer srv.Close()
	lp := alignmentLoop(t, srv.URL, nil)
	now := time.Date(2026, 6, 18, 14, 0, 0, 0, time.UTC)
	lp.now = func() time.Time { return now }

	wm := model.Watermark{Time: time.Date(2026, 6, 18, 12, 45, 30, 0, time.UTC)}
	seen := map[time.Time]int{}
	for i := 0; i < 10; i++ {
		b, err := lp.Collect(context.Background(), wm)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Samples) == 0 {
			break
		}
		if b.Watermark.Time != b.Watermark.Time.Truncate(time.Minute) {
			t.Fatalf("watermark %v is not a whole minute", b.Watermark.Time)
		}
		for _, s := range b.Samples {
			seen[s.Timestamp]++
			if s.Value != 60 {
				t.Errorf("bucket_end %v emitted partial: %v", s.Timestamp, s.Value)
			}
		}
		wm = b.Watermark
	}
	for ts, n := range seen {
		if n != 1 {
			t.Errorf("bucket_end %v emitted %d times", ts, n)
		}
	}
	// No gap: contiguous minutes from the first bucket after the checkpoint to the settle cutoff.
	first := time.Date(2026, 6, 18, 12, 46, 0, 0, time.UTC)
	last := now.Add(-10 * time.Minute)
	if want := int(last.Sub(first)/time.Minute) + 1; len(seen) != want {
		t.Fatalf("emitted %d distinct buckets, want %d (first %v last %v)", len(seen), want, first, last)
	}
	for ts := first; !ts.After(last); ts = ts.Add(time.Minute) {
		if seen[ts] != 1 {
			t.Errorf("bucket_end %v: seen %d times", ts, seen[ts])
		}
	}
	if !wm.Time.Equal(last) {
		t.Errorf("final watermark %v, want %v", wm.Time, last)
	}
}

// TestAnalyticsNoFalseRevisionOnMidMinutePoll: steady-state polls at an off-minute step against
// constant traffic must not report any settled bucket as revised.
func TestAnalyticsNoFalseRevisionOnMidMinutePoll(t *testing.T) {
	srv := rowFilterServer()
	defer srv.Close()
	var ages []time.Duration
	lp := alignmentLoop(t, srv.URL, func(_, _ string, age time.Duration) { ages = append(ages, age) })
	now := time.Date(2026, 6, 18, 14, 0, 17, 0, time.UTC)
	lp.now = func() time.Time { return now }

	var wm model.Watermark
	for i := 0; i < 60; i++ {
		b, err := lp.Collect(context.Background(), wm)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Samples) > 0 && b.Watermark.Time != b.Watermark.Time.Truncate(time.Minute) {
			t.Fatalf("poll %d: watermark %v is not a whole minute", i, b.Watermark.Time)
		}
		for _, s := range b.Samples {
			if s.Value != 60 {
				t.Fatalf("poll %d: partial bucket %v = %v", i, s.Timestamp, s.Value)
			}
		}
		wm = b.Watermark
		now = now.Add(63 * time.Second)
	}
	if len(ages) != 0 {
		t.Fatalf("%d false bucket_revised_after_settle events, ages %v", len(ages), ages)
	}
}
