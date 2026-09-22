// SPDX-License-Identifier: Apache-2.0

package source

// CapabilityState is the closed set of states a source capability can report.
type CapabilityState string

// endpoint-absent and plan-unsupported are enumerated but producerless because one 404 cannot
// distinguish permanent absence, plan gate, or transient failure; steady/intermittent increments
// remain a query-time property.
const (
	CapabilityEndpointAbsent   CapabilityState = "endpoint-absent"
	CapabilityPlanUnsupported  CapabilityState = "plan-unsupported"
	CapabilityPermissionDenied CapabilityState = "permission-denied"
	CapabilityNoData           CapabilityState = "no-data"
	CapabilityTransient404     CapabilityState = "transient-404"
	CapabilitySchemaChanged    CapabilityState = "schema-changed"
)

// AllCapabilityStates contains every valid CapabilityState in stable order.
var AllCapabilityStates = []CapabilityState{
	CapabilityEndpointAbsent,
	CapabilityPlanUnsupported,
	CapabilityPermissionDenied,
	CapabilityNoData,
	CapabilityTransient404,
	CapabilitySchemaChanged,
}

// Valid reports whether s is one of the enumerated capability states.
func (s CapabilityState) Valid() bool {
	switch s {
	case CapabilityEndpointAbsent,
		CapabilityPlanUnsupported,
		CapabilityPermissionDenied,
		CapabilityNoData,
		CapabilityTransient404,
		CapabilitySchemaChanged:
		return true
	default:
		return false
	}
}

// IncompleteReason is the closed set of reasons a source or export can report incomplete data.
type IncompleteReason string

const (
	IncompleteWindowTruncated      IncompleteReason = "window_truncated"
	IncompleteSessionsTruncated    IncompleteReason = "sessions_truncated"
	IncompleteBackfillSkipped      IncompleteReason = "backfill_skipped"
	IncompleteWindowOversize       IncompleteReason = "window_oversize"
	IncompleteSpanStatsUnavailable IncompleteReason = "span_stats_unavailable"
	IncompleteDuplicateDimension   IncompleteReason = "duplicate_dimension"
	IncompleteTraceIDUnparsed      IncompleteReason = "trace_id_unparsed"
	IncompleteLineOversize         IncompleteReason = "line_oversize"
	IncompleteLineUnparseable      IncompleteReason = "line_unparseable"
	IncompleteExportStuck          IncompleteReason = "export_stuck"
	IncompleteExportFailed         IncompleteReason = "export_failed"
)

// AllIncompleteReasons contains every valid IncompleteReason in stable order.
var AllIncompleteReasons = []IncompleteReason{
	IncompleteWindowTruncated,
	IncompleteSessionsTruncated,
	IncompleteBackfillSkipped,
	IncompleteWindowOversize,
	IncompleteSpanStatsUnavailable,
	IncompleteDuplicateDimension,
	IncompleteTraceIDUnparsed,
	IncompleteLineOversize,
	IncompleteLineUnparseable,
	IncompleteExportStuck,
	IncompleteExportFailed,
}

// Valid reports whether r is one of the enumerated incomplete-data reasons.
func (r IncompleteReason) Valid() bool {
	switch r {
	case IncompleteWindowTruncated,
		IncompleteSessionsTruncated,
		IncompleteBackfillSkipped,
		IncompleteWindowOversize,
		IncompleteSpanStatsUnavailable,
		IncompleteDuplicateDimension,
		IncompleteTraceIDUnparsed,
		IncompleteLineOversize,
		IncompleteLineUnparseable,
		IncompleteExportStuck,
		IncompleteExportFailed:
		return true
	default:
		return false
	}
}
