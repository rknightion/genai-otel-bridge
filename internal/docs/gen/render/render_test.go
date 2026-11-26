// SPDX-License-Identifier: Apache-2.0
package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
)

func TestCatalogueDeterministicAndGrouped(t *testing.T) {
	sigs := []signal.Signal{
		{Plane: signal.PlaneProduct, Type: signal.KindMetric, Source: "portkey", Name: "{p}_requests", Instrument: "gauge", Unit: "1", Description: "req", DependsOn: "graphs includes requests"},
		{Plane: signal.PlaneSelf, Type: signal.KindMetric, Source: "selfobs", Name: "genai_otel_bridge_emitted_total", Instrument: "counter", Unit: "1", Description: "emitted", Attributes: []string{"loop"}},
	}
	a := Catalogue(sigs)
	b := Catalogue(sigs) // determinism: same input → identical bytes
	if !bytes.Equal(a, b) {
		t.Fatal("Catalogue is not deterministic")
	}
	s := string(a)
	if !strings.Contains(s, Begin) || !strings.Contains(s, End) {
		t.Fatal("missing markers")
	}
	if !strings.Contains(s, "genai_otel_bridge_emitted_total") || !strings.Contains(s, "{p}_requests") {
		t.Fatal("missing rows")
	}
}

func TestPrometheusName(t *testing.T) {
	self := func(name, inst, unit string) signal.Signal {
		return signal.Signal{Plane: signal.PlaneSelf, Type: signal.KindMetric, Name: name, Instrument: inst, Unit: unit}
	}
	cases := []struct {
		name string
		sig  signal.Signal
		want string
	}{
		{"unit-1 gauge gains _ratio", self("genai_otel_bridge_loop_degraded", "gauge", "1"), "genai_otel_bridge_loop_degraded_ratio"},
		{"unit-1 gauge, queue depth", self("genai_otel_bridge_queue_depth", "gauge", "1"), "genai_otel_bridge_queue_depth_ratio"},
		{"seconds gauge already suffixed", self("genai_otel_bridge_window_lag_seconds", "gauge", "s"), "genai_otel_bridge_window_lag_seconds"},
		{"unitless counter keeps _total", self("genai_otel_bridge_emitted_total", "counter", ""), "genai_otel_bridge_emitted_total"},
		{"histogram unchanged", self("genai_otel_bridge_emit_request_duration_seconds", "histogram", "s"), "genai_otel_bridge_emit_request_duration_seconds"},
		{"product gauge unchanged (empty wire unit)", signal.Signal{Plane: signal.PlaneProduct, Type: signal.KindMetric, Name: "{p}_requests", Instrument: "gauge", Unit: "1"}, "{p}_requests"},
		{"log has none", signal.Signal{Plane: signal.PlaneSelf, Type: signal.KindLog, Name: "x"}, ""},
	}
	for _, c := range cases {
		if got := PrometheusName(c.sig); got != c.want {
			t.Errorf("%s: PrometheusName = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCatalogueShowsPrometheusName(t *testing.T) {
	out := string(Catalogue([]signal.Signal{
		{Plane: signal.PlaneSelf, Type: signal.KindMetric, Name: "genai_otel_bridge_loop_degraded", Instrument: "gauge", Unit: "1"},
	}))
	if !strings.Contains(out, "| Prometheus name |") || !strings.Contains(out, "`genai_otel_bridge_loop_degraded_ratio`") {
		t.Fatalf("catalogue lacks the Prometheus name column:\n%s", out)
	}
}

func TestSpliceReplacesRegion(t *testing.T) {
	doc := []byte("# Telemetry\n\nintro\n\n" + Begin + "\nOLD\n" + End + "\n\nfooter\n")
	region := []byte(Begin + "\nNEW\n" + End + "\n")
	out, err := Splice(doc, region)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "NEW") || strings.Contains(string(out), "OLD") {
		t.Fatalf("splice failed:\n%s", out)
	}
	if !strings.Contains(string(out), "intro") || !strings.Contains(string(out), "footer") {
		t.Fatal("splice clobbered surrounding prose")
	}
}
