// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"strings"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// logsOptInYAML is a legacy portkey source with a groups loop and a complete logs_export block (both
// required settings present, so the only thing between it and a built loop is the enabled line).
// enabledLine is spliced in as the block's first key; "" omits enabled entirely.
func logsOptInYAML(enabledLine string) string {
	return `sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: pk
    auth: {header: x-portkey-api-key, value: k}
    loops:
      groups:
        enabled: true
        cadence: 60s
        settings: {window_span: 1h, settle: 10m}
      logs_export:
` + enabledLine + `        cadence: 60s
        settings:
          workspace_id: ws-test
          signed_url_allow_hosts: signed-url-host.example.com
`
}

// countBuilt loads raw through config.LoadBytes (the path a hand-written config and the chart's
// configOverride take) and builds its one source, returning how many logs_export and groups loops New
// built.
func countBuilt(t *testing.T, raw string) (logs, groups int) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(raw))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	src, err := New(cfg.Sources[0], source.Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, lp := range src.Loops() {
		switch lp.(type) {
		case *logsExportLoop:
			logs++
		case *groupsLoop:
			groups++
		}
	}
	return logs, groups
}

// TestLogsExportIsOptIn pins #585 (logs_export runs only when explicitly enabled): a logs_export block
// that omits enabled, or sets it false, builds no logs loop, while the metric loop beside it is built
// unchanged. Load applies no default to a loop's enabled (the chart's helm:"default=true" tag only
// renders the generated analytics block), and New builds logs_export only on Enabled. The explicit
// enabled: true case is the positive control: the same block does build a loop, so the omitted case
// cannot pass because the block is otherwise unbuildable.
func TestLogsExportIsOptIn(t *testing.T) {
	cases := []struct {
		name        string
		enabledLine string
		wantLogs    int
	}{
		{"enabled omitted", "", 0},
		{"enabled false", "        enabled: false\n", 0},
		{"enabled true", "        enabled: true\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := logsOptInYAML(tc.enabledLine)
			if tc.enabledLine == "" && strings.Contains(raw, "logs_export:\n        enabled") {
				t.Fatal("fixture still writes enabled for the omitted case")
			}
			logs, groups := countBuilt(t, raw)
			if logs != tc.wantLogs {
				t.Fatalf("logs_export loops built = %d, want %d", logs, tc.wantLogs)
			}
			if groups != 1 {
				t.Fatalf("groups loops built = %d, want 1 (logs_export opt-in must not change the metric loops)", groups)
			}
		})
	}
}

// TestLogsExportOptInTenantDefaults is the same rule for the tenant shape: a defaults.portkey
// logs_export block that omits enabled loads as disabled, so the plan's copy of it (tenancy.copyLoops
// copies Enabled as written) builds no logs loop either.
func TestLogsExportOptInTenantDefaults(t *testing.T) {
	cfg, err := config.LoadBytes([]byte(`defaults:
  portkey:
    loops:
      logs_export:
        cadence: 60s
        settings: {workspace_id: ws-test, signed_url_allow_hosts: signed-url-host.example.com}
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	lc, ok := cfg.Defaults["portkey"].Loops["logs_export"]
	if !ok {
		t.Fatal("defaults.portkey.loops.logs_export did not load")
	}
	if lc.Enabled {
		t.Fatal("a defaults logs_export block that omits enabled loaded as enabled")
	}
}
