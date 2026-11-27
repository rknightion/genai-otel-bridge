// SPDX-License-Identifier: Apache-2.0

package portkey_test

import (
	"strings"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/source"
	. "github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

// TestExampleSourceBuildable guards that ExampleSource() produces a SourceConfig that passes
// portkey.New construction, and that its logs_export block, shipped off (#585), still builds once an
// operator opts in: the two REQUIRED logs_export settings (workspace_id, signed_url_allow_hosts) are
// present and no stray LoopConfig.Window is set.
func TestExampleSourceBuildable(t *testing.T) {
	if _, err := New(ExampleSource(), source.Deps{}); err != nil {
		t.Fatalf("portkey ExampleSource must be buildable: %v", err)
	}
	optedIn := ExampleSource()
	lc := optedIn.Loops["logs_export"]
	lc.Enabled = true
	optedIn.Loops["logs_export"] = lc
	if _, err := New(optedIn, source.Deps{}); err != nil {
		t.Fatalf("portkey ExampleSource with logs_export opted in must be buildable: %v", err)
	}
}

// TestExampleLogsExportShippedOff pins #585 (AC2) for the generated chart and ECS examples: the example
// shows logs_export off while the groups metric loop stays on, and the comment above the block says how
// to opt in and names what the loop needs.
func TestExampleLogsExportShippedOff(t *testing.T) {
	ex := ExampleSource()
	lc, ok := ex.Loops["logs_export"]
	if !ok {
		t.Fatal("the example must still document the logs_export block")
	}
	if lc.Enabled {
		t.Fatal("the example must ship logs_export off: it runs only when enabled: true is written")
	}
	if !ex.Loops["groups"].Enabled {
		t.Fatal("the example's groups loop must stay on")
	}
	c := ExampleLoopComments()["logs_export"]
	for _, want := range []string{"enabled: true", "workspace_id", "signed_url_allow_hosts", "export logs"} {
		if !strings.Contains(c, want) {
			t.Errorf("logs_export opt-in comment %q does not mention %q", c, want)
		}
	}
}
