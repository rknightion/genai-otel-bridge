// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"encoding/json"
	"testing"
)

// cost fields are a JSON number in LangSmith 0.13.5 but a quoted decimal string in the 0.16.5 spec;
// null and "" mean "no value". money must accept all four and distinguish set-vs-unset (so an unset
// cost is skipped, not emitted as 0).
func TestMoneyUnmarshal(t *testing.T) {
	type wrap struct {
		C money `json:"c"`
	}
	cases := []struct {
		name    string
		raw     string // the value for "c"; "ABSENT" ⇒ field omitted
		wantSet bool
		wantVal float64
	}{
		{"number", `42.5`, true, 42.5},
		{"number_zero", `0`, true, 0},
		{"string_decimal", `"0.00"`, true, 0},
		{"string_value", `"1.2345"`, true, 1.2345},
		{"null", `null`, false, 0},
		{"empty_string", `""`, false, 0},
		{"absent", "ABSENT", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc string
			if tc.raw == "ABSENT" {
				doc = `{}`
			} else {
				doc = `{"c":` + tc.raw + `}`
			}
			var w wrap
			if err := json.Unmarshal([]byte(doc), &w); err != nil {
				t.Fatalf("unmarshal %s: %v", doc, err)
			}
			if w.C.set != tc.wantSet {
				t.Fatalf("set=%v want %v (raw %s)", w.C.set, tc.wantSet, tc.raw)
			}
			if w.C.set && w.C.v != tc.wantVal {
				t.Fatalf("v=%v want %v (raw %s)", w.C.v, tc.wantVal, tc.raw)
			}
		})
	}
}

// A malformed cost (non-numeric string) must error, not silently zero.
func TestMoneyUnmarshalRejectsGarbage(t *testing.T) {
	var w struct {
		C money `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"c":"not-a-number"}`), &w); err == nil {
		t.Fatal("want error for non-numeric cost string, got nil")
	}
}
