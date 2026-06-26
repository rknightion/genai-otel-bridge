// SPDX-License-Identifier: AGPL-3.0-only
package portkey

import (
	"strings"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
)

func TestPortkeySignalsCoverSuffixesAndLabels(t *testing.T) {
	sigs := Signals()

	// every metricSuffix value must appear as a metric descriptor name template suffix
	for graph, suffix := range metricSuffix {
		found := false
		for _, s := range sigs {
			if s.Type == signal.KindMetric && strings.HasSuffix(s.Name, "_"+suffix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("graph %q (suffix %q) has no metric descriptor", graph, suffix)
		}
	}

	// every allowed label key must be documented on some signal
	labelSeen := map[string]bool{}
	for _, s := range sigs {
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
