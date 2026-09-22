// SPDX-License-Identifier: Apache-2.0
package signal

import "testing"

func TestSignalSortKey(t *testing.T) {
	s := Signal{Plane: PlaneProduct, Type: KindMetric, Name: "portkey_api_requests"}
	if got := s.SortKey(); got != "product\x00metric\x00portkey_api_requests" {
		t.Fatalf("SortKey = %q", got)
	}
}
