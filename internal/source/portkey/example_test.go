// SPDX-License-Identifier: AGPL-3.0-only

package portkey_test

import (
	"testing"

	"github.com/grafana-ps/aip-oi/internal/source"
	. "github.com/grafana-ps/aip-oi/internal/source/portkey"
)

// TestExampleSourceBuildable guards that ExampleSource() produces a SourceConfig that passes
// portkey.New construction — including the two REQUIRED logs_export settings (workspace_id,
// signed_url_allow_hosts) and no stray LoopConfig.Window set.
func TestExampleSourceBuildable(t *testing.T) {
	if _, err := New(ExampleSource(), source.Deps{}); err != nil {
		t.Fatalf("portkey ExampleSource must be buildable: %v", err)
	}
}
