// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// Grafana rejects a recording rule whose spec.metric is not a valid Prometheus name, and an alert or
// recording rule whose metadata.name (its UID) is longer than 40 characters. Neither is checked
// server-side before a push (no rule dry run), so the bundled manifests are checked here.
var promMetricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

const maxRuleUIDLen = 40

func TestBundledRuleNamesAreAcceptedByGrafana(t *testing.T) {
	files, err := filepath.Glob("../../deploy/grafana/self-obs/*rule-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no bundled rule manifests found")
	}
	var recording int
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Metric string `yaml:"metric"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if m.Kind != "AlertRule" && m.Kind != "RecordingRule" {
			continue
		}
		if n := len(m.Metadata.Name); n == 0 || n > maxRuleUIDLen {
			t.Errorf("%s: metadata.name %q is %d characters, want 1 to %d", f, m.Metadata.Name, n, maxRuleUIDLen)
		}
		if m.Kind == "RecordingRule" {
			recording++
			if !promMetricNameRe.MatchString(m.Spec.Metric) {
				t.Errorf("%s: spec.metric %q does not match %s", f, m.Spec.Metric, promMetricNameRe)
			}
		}
	}
	if recording == 0 {
		t.Fatal("no recording rules found: the glob or kind match is broken")
	}
}
