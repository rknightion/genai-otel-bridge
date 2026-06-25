// SPDX-License-Identifier: AGPL-3.0-only

package emit

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func g(name string, ts time.Time, v float64, labels map[string]string) model.Sample {
	return model.Sample{Name: name, Unit: "1", Labels: labels, Timestamp: ts, Value: v, Kind: model.Gauge}
}

func TestCoalesceDPM_NoOpOnOnePerMinute(t *testing.T) {
	// The current Portkey shape: one sample per (series, minute) at distinct minutes — must be unchanged.
	base := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	in := []model.Sample{
		g("portkey_api_requests", base, 1, nil),
		g("portkey_api_requests", base.Add(time.Minute), 2, nil),
		g("portkey_api_requests", base.Add(2*time.Minute), 3, nil),
	}
	out, capped := CoalesceDPM(in, 1)
	if capped != 0 || len(out) != 3 {
		t.Fatalf("expected no-op (capped=0,len=3); got capped=%d len=%d", capped, len(out))
	}
}

func TestCoalesceDPM_SubMinuteCollapsesLWW(t *testing.T) {
	// A future sub-minute source: three points in the SAME minute for ONE series → keep the newest.
	base := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	in := []model.Sample{
		g("m", base.Add(10*time.Second), 10, nil),
		g("m", base.Add(50*time.Second), 50, nil), // newest → survives (LWW)
		g("m", base.Add(20*time.Second), 20, nil),
	}
	out, capped := CoalesceDPM(in, 1)
	if capped != 2 || len(out) != 1 {
		t.Fatalf("expected capped=2,len=1; got capped=%d len=%d", capped, len(out))
	}
	if out[0].Value != 50 || !out[0].Timestamp.Equal(base.Add(50*time.Second)) {
		t.Fatalf("LWW must keep the newest sample (v=50 @ +50s); got v=%v @ %v", out[0].Value, out[0].Timestamp)
	}
}

func TestCoalesceDPM_BackfillDistinctMinutesSurvive(t *testing.T) {
	// Legitimate backfill: N distinct past minutes in one batch = still ≤1DPM each → all survive.
	base := time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)
	var in []model.Sample
	for i := 0; i < 30; i++ {
		in = append(in, g("m", base.Add(time.Duration(i)*time.Minute), float64(i), nil))
	}
	out, capped := CoalesceDPM(in, 1)
	if capped != 0 || len(out) != 30 {
		t.Fatalf("backfill of 30 distinct minutes must all survive; got capped=%d len=%d", capped, len(out))
	}
}

func TestCoalesceDPM_DistinctSeriesSameMinuteSurvive(t *testing.T) {
	// Different label sets = different series → each gets its own 1DPM allowance in the same minute.
	ts := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	in := []model.Sample{
		g("portkey_api_latency_seconds", ts, 0.1, map[string]string{"quantile": "p50"}),
		g("portkey_api_latency_seconds", ts, 0.2, map[string]string{"quantile": "p90"}),
		g("portkey_api_latency_seconds", ts, 0.3, map[string]string{"quantile": "p99"}),
	}
	out, capped := CoalesceDPM(in, 1)
	if capped != 0 || len(out) != 3 {
		t.Fatalf("distinct series must not coalesce; got capped=%d len=%d", capped, len(out))
	}
}

func TestCoalesceDPM_CollisionSafeKey(t *testing.T) {
	// {"a":"b;c=d"} and {"a":"b","c":"d"} are DIFFERENT series — a naive k=v; join would merge them
	// and drop one (silent loss). Length-prefixed key keeps them distinct.
	ts := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	in := []model.Sample{
		g("m", ts, 1, map[string]string{"a": "b;c=d"}),
		g("m", ts, 2, map[string]string{"a": "b", "c": "d"}),
	}
	out, capped := CoalesceDPM(in, 1)
	if capped != 0 || len(out) != 2 {
		t.Fatalf("collision-safe key must keep both series; got capped=%d len=%d", capped, len(out))
	}
}

func TestCoalesceDPM_MaxDPMGreaterThanOne(t *testing.T) {
	base := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	in := []model.Sample{
		g("m", base.Add(10*time.Second), 10, nil),
		g("m", base.Add(20*time.Second), 20, nil),
		g("m", base.Add(50*time.Second), 50, nil),
	}
	out, capped := CoalesceDPM(in, 2) // keep the 2 newest
	if capped != 1 || len(out) != 2 {
		t.Fatalf("maxDPM=2 over 3 same-minute → capped=1,len=2; got capped=%d len=%d", capped, len(out))
	}
}

func TestCoalesceDPM_Empty(t *testing.T) {
	out, capped := CoalesceDPM(nil, 1)
	if capped != 0 || len(out) != 0 {
		t.Fatalf("empty in → empty out; got capped=%d len=%d", capped, len(out))
	}
}
