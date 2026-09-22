// SPDX-License-Identifier: Apache-2.0

package emit

import "testing"

func TestRejectAdvancesPast(t *testing.T) {
	for r, want := range map[RejectReason]bool{
		ReasonDuplicateTimestamp: true,
		ReasonTooOld:             true,
		ReasonPayloadTooLarge:    true,
		ReasonUnknown:            false, // [CP-C7] unrecognised request-level 4xx HALTS (degrade), never silent advance
		ReasonBadEncoding:        false, // a real bug — halt + alert, must NOT silently advance (F9)
	} {
		if (&RejectError{Reason: r}).AdvancesPast() != want {
			t.Errorf("reason %v: AdvancesPast=%v want %v", r, !want, want)
		}
	}
}
