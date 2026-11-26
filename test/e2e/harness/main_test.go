// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// The sink must read back exactly what the bridge's own encoder writes, or the e2e data checks would
// compare against a mis-decoded record.
func TestDecodeExportRoundTripsTheEmitterEncoding(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	body, err := otlp.Encode(map[string]string{"service.name": "x"}, []model.Sample{
		{Name: "portkey_api_requests", Kind: model.Gauge, Value: 7, Timestamp: ts},
		{Name: "portkey_api_latency_seconds", Kind: model.Gauge, Labels: map[string]string{"quantile": "p50"}, Value: 0.5, Timestamp: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	pts, err := decodeExport(body)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]point{}
	for _, p := range pts {
		got[p.Series] = p
	}
	if len(pts) != 2 {
		t.Fatalf("decoded %d points, want 2: %+v", len(pts), pts)
	}
	if p := got["portkey_api_requests"]; p.Value != 7 || !p.Time.Equal(ts) || p.Labels != "" {
		t.Fatalf("requests point = %+v", p)
	}
	if p := got["portkey_api_latency_seconds"]; p.Value != 0.5 || p.Labels != "quantile=p50" {
		t.Fatalf("latency point = %+v", p)
	}
}

// Every minute bucket the window touches gets a point, so a settled bucket is never missing from a
// response and the suite can treat a missing bucket as the bridge's gap.
func TestGraphPointsCoverEveryTouchedBucket(t *testing.T) {
	lo := time.Date(2026, 1, 2, 3, 4, 30, 0, time.UTC)
	hi := time.Date(2026, 1, 2, 3, 7, 10, 0, time.UTC)
	pts := graphPoints("requests", lo, hi)
	want := []string{"2026-01-02T03:04:00.000Z", "2026-01-02T03:05:00.000Z", "2026-01-02T03:06:00.000Z", "2026-01-02T03:07:00.000Z"}
	if len(pts) != len(want) {
		t.Fatalf("got %d points, want %d: %v", len(pts), len(want), pts)
	}
	for i, p := range pts {
		if p["timestamp"] != want[i] {
			t.Fatalf("point %d timestamp %v, want %s", i, p["timestamp"], want[i])
		}
		b, _ := time.Parse(time.RFC3339, want[i])
		if p["total"] != pointValue(b) {
			t.Fatalf("point %d total %v, want %v", i, p["total"], pointValue(b))
		}
	}
	tok := graphPoints("tokens", lo, lo)[0]
	if tok["total"] != tok["total_request_units"].(float64)+tok["total_response_units"].(float64) {
		t.Fatalf("tokens total must equal input+output: %v", tok)
	}
}
