// SPDX-License-Identifier: AGPL-3.0-only
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
