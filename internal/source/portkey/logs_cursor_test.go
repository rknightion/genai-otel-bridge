// SPDX-License-Identifier: Apache-2.0

package portkey

import "testing"

func TestExportCursorRoundTrip(t *testing.T) {
	// Round-trip a cursor in each lifecycle phase (the cursor must survive the checkpoint store verbatim
	// so a leader change resumes the in-flight job at the right step).
	for _, phase := range []string{phaseIdle, phaseCreated, phasePolling, phaseDownloading} {
		c := exportCursor{
			Phase: phase, JobID: "job-123", WinMin: "2026-06-18T00:00:00Z", WinMax: "2026-06-18T01:00:00Z",
			Page: 2, Pages: 21, TotalRecords: 1022784, PageOffsetDone: 15000, PollDeadline: "2026-06-18T01:10:00Z",
		}
		if got := decodeCursor(c.encode()); got != c {
			t.Fatalf("round-trip mismatch (%s):\n got %+v\nwant %+v", phase, got, c)
		}
	}
}

func TestExportCursorEmptyIsIdle(t *testing.T) {
	if c := decodeCursor(""); c.Phase != phaseIdle || c.JobID != "" {
		t.Fatalf("empty cursor must be idle, got %+v", c)
	}
}

func TestExportCursorCorruptIsIdle(t *testing.T) {
	for _, bad := range []string{"{not json", `{"page":1}`, "null", "42"} {
		if c := decodeCursor(bad); c.Phase != phaseIdle {
			t.Fatalf("corrupt/phase-less cursor %q must reset to idle, got %+v", bad, c)
		}
	}
}

// TestExportCursorLegacyEncodingRoundTrip proves that checkpoints written before self-APM lifecycle
// correlation remain valid. The added trace/span identifiers are optional checkpoint state and must not
// change a legacy cursor merely by loading and saving it during a leader change.
func TestExportCursorLegacyEncodingRoundTrip(t *testing.T) {
	const legacy = `{"phase":"polling","job_id":"job-123","win_min":"2026-06-18T00:00:00Z","win_max":"2026-06-18T01:00:00Z","pages":21,"total_records":1022784,"poll_deadline":"2026-06-18T01:10:00Z"}`
	got := decodeCursor(legacy)
	if got.LifecycleTraceID != "" || got.LifecycleSpanID != "" {
		t.Fatalf("legacy cursor unexpectedly acquired lifecycle identity: %+v", got)
	}
	if got.Phase != phasePolling || got.JobID != "job-123" || got.Pages != 21 || got.TotalRecords != 1022784 {
		t.Fatalf("legacy cursor decoded incorrectly: %+v", got)
	}
	if encoded := got.encode(); encoded != legacy {
		t.Fatalf("legacy cursor changed on current-code round-trip:\n got %s\nwant %s", encoded, legacy)
	}
}
