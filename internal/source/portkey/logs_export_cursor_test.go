// SPDX-License-Identifier: Apache-2.0

package portkey

import "testing"

// TestExportCursorGolden pins every persisted key of the logs-export cursor against a literal JSON
// string. The cursor rides the checkpoint across releases and leader changes, so a renamed key loses the
// resume position and re-drains the window.
func TestExportCursorGolden(t *testing.T) {
	const golden = `{"phase":"downloading","job_id":"job-1","win_min":"2026-06-19T00:00:00Z","win_max":"2026-06-19T01:00:00Z",` +
		`"page":2,"pages":5,"total_records":900,"page_offset_done":40,"page_byte_offset":8192,` +
		`"poll_deadline":"2026-06-19T01:05:00.5Z","lifecycle_trace_id":"0af7651916cd43dd8448eb211c80319c","lifecycle_span_id":"b7ad6b7169203331"}`
	want := exportCursor{
		Phase: phaseDownloading, JobID: "job-1", WinMin: "2026-06-19T00:00:00Z", WinMax: "2026-06-19T01:00:00Z",
		Page: 2, Pages: 5, TotalRecords: 900, PageOffsetDone: 40, PageByteOffset: 8192,
		PollDeadline: "2026-06-19T01:05:00.5Z", LifecycleTraceID: "0af7651916cd43dd8448eb211c80319c", LifecycleSpanID: "b7ad6b7169203331",
	}
	if got := decodeCursor(golden); got != want {
		t.Fatalf("decode = %+v want %+v", got, want)
	}
	if got := want.encode(); got != golden {
		t.Fatalf("encode = %s want %s", got, golden)
	}
}
