// SPDX-License-Identifier: AGPL-3.0-only

package config_test

import (
	"os"
	"testing"

	"github.com/grafana-ps/aip-oi/internal/config/gen/helmgen"
	"github.com/grafana-ps/aip-oi/internal/source/langsmith"
	"github.com/grafana-ps/aip-oi/internal/source/portkey"
)

// TestHelmGeneratedExamplesUpToDate is the drift gate for the source-examples region of values.yaml
// (the commented-out per-source-type examples), mirroring TestHelmGeneratedConfigUpToDate. It is an
// EXTERNAL test package because it imports a source package (langsmith → config); an internal
// `package config` test cannot without an import cycle.
//
// Keep the example list in sync with internal/config/gen/main.go (the generator's RenderExampleBlock
// call) — both must render the same set of ExampleSource() values.
func TestHelmGeneratedExamplesUpToDate(t *testing.T) {
	const valuesPath = "../../deploy/helm/values.yaml"
	want, err := helmgen.RenderExampleBlock([]helmgen.Example{
		{Value: portkey.ExampleSource(), SettingsComments: portkey.ExampleSettingsComments()},
		{Value: langsmith.ExampleSource(), SettingsComments: langsmith.ExampleSettingsComments()},
	})
	if err != nil {
		t.Fatalf("render example block: %v", err)
	}
	values, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read %s: %v", valuesPath, err)
	}
	got, err := helmgen.ExtractExampleBlock(values)
	if err != nil {
		t.Fatalf("extract example block: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("values.yaml source-examples block is stale — run `make generate`\n\n--- committed ---\n%s\n--- generated ---\n%s",
			got, want)
	}
}
