// SPDX-License-Identifier: Apache-2.0

package source

import "testing"

func TestCapabilityStateValues(t *testing.T) {
	cases := []struct {
		name string
		got  CapabilityState
		want CapabilityState
	}{
		{"CapabilityEndpointAbsent", CapabilityEndpointAbsent, "endpoint-absent"},
		{"CapabilityPlanUnsupported", CapabilityPlanUnsupported, "plan-unsupported"},
		{"CapabilityPermissionDenied", CapabilityPermissionDenied, "permission-denied"},
		{"CapabilityNoData", CapabilityNoData, "no-data"},
		{"CapabilityTransient404", CapabilityTransient404, "transient-404"},
		{"CapabilitySchemaChanged", CapabilitySchemaChanged, "schema-changed"},
	}
	if got := AllCapabilityStates; len(got) != len(cases) {
		t.Fatalf("AllCapabilityStates has %d values, want %d", len(got), len(cases))
	}
	for i, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
		if got := AllCapabilityStates[i]; got != tc.want {
			t.Errorf("AllCapabilityStates[%d] = %q, want %q", i, got, tc.want)
		}
		if !tc.got.Valid() {
			t.Errorf("%q.Valid() = false, want true", tc.got)
		}
	}
	if CapabilityState("unexpected").Valid() {
		t.Error("CapabilityState(\"unexpected\").Valid() = true, want false")
	}
}

func TestIncompleteReasonValues(t *testing.T) {
	cases := []struct {
		name string
		got  IncompleteReason
		want IncompleteReason
	}{
		{"IncompleteWindowTruncated", IncompleteWindowTruncated, "window_truncated"},
		{"IncompleteSessionsTruncated", IncompleteSessionsTruncated, "sessions_truncated"},
		{"IncompleteBackfillSkipped", IncompleteBackfillSkipped, "backfill_skipped"},
		{"IncompleteWindowOversize", IncompleteWindowOversize, "window_oversize"},
		{"IncompleteSpanStatsUnavailable", IncompleteSpanStatsUnavailable, "span_stats_unavailable"},
		{"IncompleteDuplicateDimension", IncompleteDuplicateDimension, "duplicate_dimension"},
		{"IncompleteTraceIDUnparsed", IncompleteTraceIDUnparsed, "trace_id_unparsed"},
		{"IncompleteLineOversize", IncompleteLineOversize, "line_oversize"},
		{"IncompleteLineUnparseable", IncompleteLineUnparseable, "line_unparseable"},
		{"IncompleteExportStuck", IncompleteExportStuck, "export_stuck"},
		{"IncompleteExportFailed", IncompleteExportFailed, "export_failed"},
	}
	if got := AllIncompleteReasons; len(got) != len(cases) {
		t.Fatalf("AllIncompleteReasons has %d values, want %d", len(got), len(cases))
	}
	for i, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
		if got := AllIncompleteReasons[i]; got != tc.want {
			t.Errorf("AllIncompleteReasons[%d] = %q, want %q", i, got, tc.want)
		}
		if !tc.got.Valid() {
			t.Errorf("%q.Valid() = false, want true", tc.got)
		}
	}
	if IncompleteReason("unexpected").Valid() {
		t.Error("IncompleteReason(\"unexpected\").Valid() = true, want false")
	}
}
