// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana-ps/aip-oi/internal/checkpoint/file"
	"github.com/grafana-ps/aip-oi/internal/coordinate"
	"github.com/grafana-ps/aip-oi/internal/schedule"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// nIndexedFields returns n synthetic content-free field names, comma-joined — operator-promoted indexed
// fields for the extra_indexed_fields knob (not floor/hard-denied keys, so the floor check passes and
// only the stream-label budget governs).
func nIndexedFields(n int) string {
	xs := make([]string, n)
	for i := range xs {
		xs[i] = fmt.Sprintf("ix%d", i)
	}
	return strings.Join(xs, ",")
}

// TestBuildRejectsIndexedFieldsOverLokiStreamLabelBudget: a logs loop whose product identity resource
// attrs (3 — service.name/service.namespace/deployment.environment.name, all in the GC Loki default-promoted
// set) plus its indexed attrs (base 2 + extra_indexed_fields) would exceed
// governance.max_stream_label_keys (GC Loki max_label_names_per_series default 15) is rejected fail-fast.
// Loki REJECTS a stream over that limit, so the records would silently never land — the operationally-
// honest rule forbids that silent loss, so the misconfig must fail at Build, not at emit time.
func TestBuildRejectsIndexedFieldsOverLokiStreamLabelBudget(t *testing.T) {
	cfg := logsExportConfig("http://127.0.0.1:1", "127.0.0.1")
	lp := cfg.Sources[0].Loops["logs_export"]
	lp.Settings["extra_indexed_fields"] = nIndexedFields(10) // 3 identity + 3 base + 10 = 16 > 15
	cfg.Sources[0].Loops["logs_export"] = lp
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	_, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err == nil || !strings.Contains(err.Error(), "max_stream_label_keys") {
		t.Fatalf("want a Loki stream-label budget rejection naming max_stream_label_keys, got %v", err)
	}
	if !strings.Contains(err.Error(), "logs_export") {
		t.Fatalf("budget error must name the offending loop, got %v", err)
	}
}

// TestBuildAcceptsIndexedFieldsAtBudgetBoundary: identity(3) + base(3) + 9 extras = 15 == the default
// budget ⇒ accepted (the limit is inclusive; 16 is rejected above).
func TestBuildAcceptsIndexedFieldsAtBudgetBoundary(t *testing.T) {
	cfg := logsExportConfig("http://127.0.0.1:1", "127.0.0.1")
	lp := cfg.Sources[0].Loops["logs_export"]
	lp.Settings["extra_indexed_fields"] = nIndexedFields(9) // 3 + 3 + 9 = 15 == default budget
	cfg.Sources[0].Loops["logs_export"] = lp
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if _, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{}); err != nil {
		t.Fatalf("at-budget (15) config must build, got %v", err)
	}
}

// TestBuildAcceptsDefaultLogsLoopWithinBudget: the default logs loop (identity 3 + base 3 = 6, no extras)
// is comfortably within budget — regression guard that the new check never trips a vanilla deployment.
func TestBuildAcceptsDefaultLogsLoopWithinBudget(t *testing.T) {
	cfg := logsExportConfig("http://127.0.0.1:1", "127.0.0.1")
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if _, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{}); err != nil {
		t.Fatalf("default logs loop must build, got %v", err)
	}
}

// TestStreamLabelBudgetKnobRaises: an operator who raised the Loki max_label_names_per_series limit (a GS
// ticket) raises governance.max_stream_label_keys to match — the previously-rejected config then builds.
func TestStreamLabelBudgetKnobRaises(t *testing.T) {
	cfg := logsExportConfig("http://127.0.0.1:1", "127.0.0.1")
	cfg.Governance.MaxStreamLabelKeys = 30
	lp := cfg.Sources[0].Loops["logs_export"]
	lp.Settings["extra_indexed_fields"] = nIndexedFields(10) // 16 ≤ 30
	cfg.Sources[0].Loops["logs_export"] = lp
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if _, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{}); err != nil {
		t.Fatalf("raised budget (30) must accept 16 labels, got %v", err)
	}
}
