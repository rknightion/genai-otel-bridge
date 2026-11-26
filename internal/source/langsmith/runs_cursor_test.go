// SPDX-License-Identifier: Apache-2.0

package langsmith

import "testing"

// TestRunsCursorGolden pins every persisted key of the runs cursor against a literal JSON string. The
// cursor rides the checkpoint across releases, so a renamed key makes the next release misread it.
func TestRunsCursorGolden(t *testing.T) {
	const golden = `{"win_min":"2026-06-19T00:00:00Z","win_max":"2026-06-19T01:00:00Z","next":"tok-1","page":3}`
	want := runsCursor{WinMin: "2026-06-19T00:00:00Z", WinMax: "2026-06-19T01:00:00Z", Next: "tok-1", Page: 3}
	if got := decodeRunsCursor(golden); got != want {
		t.Fatalf("decode = %+v want %+v", got, want)
	}
	if got := want.encode(); got != golden {
		t.Fatalf("encode = %s want %s", got, golden)
	}
}
