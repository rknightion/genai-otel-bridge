// SPDX-License-Identifier: AGPL-3.0-only

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
