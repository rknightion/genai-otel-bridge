// SPDX-License-Identifier: AGPL-3.0-only
package langsmith

import (
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
)

func TestLangSmithSignalsCoverLabels(t *testing.T) {
	sigs := Signals()
	if len(sigs) == 0 {
		t.Fatal("Signals() is empty")
	}
	labelSeen := map[string]bool{}
	for _, s := range sigs {
		if s.Plane != signal.PlaneProduct {
			t.Errorf("signal %q has plane %q, want product", s.Name, s.Plane)
		}
		for _, a := range s.Attributes {
			labelSeen[a] = true
		}
	}
	for _, k := range AllowedLabelKeys() {
		if !labelSeen[k] {
			t.Errorf("allowed label key %q is not documented on any signal", k)
		}
	}
}
