// SPDX-License-Identifier: Apache-2.0
package signal

import "testing"

// TestSignalSortKeyOrdering pins the catalogue ordering (plane, then type, then name) by comparing
// signals that differ in exactly one field, without depending on the key's encoding.
func TestSignalSortKeyOrdering(t *testing.T) {
	base := Signal{Plane: PlaneProduct, Type: KindMetric, Name: "b"}
	cases := []struct {
		name         string
		lower, upper Signal
	}{
		{"plane", Signal{Plane: PlaneProduct, Type: KindMetric, Name: "z"}, Signal{Plane: PlaneSelf, Type: KindLog, Name: "a"}},
		{"type", Signal{Plane: base.Plane, Type: KindLog, Name: "z"}, Signal{Plane: base.Plane, Type: KindMetric, Name: "a"}},
		{"name", Signal{Plane: base.Plane, Type: base.Type, Name: "a"}, Signal{Plane: base.Plane, Type: base.Type, Name: "b"}},
	}
	for _, c := range cases {
		if !(c.lower.SortKey() < c.upper.SortKey()) {
			t.Errorf("%s: want %+v to sort before %+v", c.name, c.lower, c.upper)
		}
		if c.upper.SortKey() < c.lower.SortKey() {
			t.Errorf("%s: reverse order holds", c.name)
		}
	}
}
