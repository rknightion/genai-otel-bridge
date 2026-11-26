// SPDX-License-Identifier: Apache-2.0
package selfobs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
)

// instrumentUnitsFromSource extracts the string-literal first arg of every mk/mg/mh/mo(...) call in
// metrics.go, prefixed "genai_otel_bridge_" (the constructors add that prefix at runtime), mapped to
// the unit the instrument is created with: mk counters are created with no unit (""), mg/mh take it
// as their third argument. This is the wire unit the gateway sees.
func instrumentUnitsFromSource(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "metrics.go", nil, 0)
	if err != nil {
		t.Fatalf("parse metrics.go: %v", err)
	}
	units := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || (id.Name != "mk" && id.Name != "mg" && id.Name != "mh" && id.Name != "mo") || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		unit := ""
		if id.Name != "mk" {
			if len(call.Args) < 3 {
				t.Fatalf("%s call for %s has no unit argument", id.Name, lit.Value)
			}
			ulit, ok := call.Args[2].(*ast.BasicLit)
			if !ok || ulit.Kind != token.STRING {
				t.Fatalf("%s call for %s: unit argument is not a string literal", id.Name, lit.Value)
			}
			unit = ulit.Value[1 : len(ulit.Value)-1]
		}
		units["genai_otel_bridge_"+lit.Value[1:len(lit.Value)-1]] = unit
		return true
	})
	return units
}

func instrumentNamesFromSource(t *testing.T) []string {
	var names []string
	for n := range instrumentUnitsFromSource(t) {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestSelfObsSignalsParity holds the catalogue to the instruments by name AND unit: the unit decides
// the Prometheus-exposed name (a unit-"1" gauge gains "_ratio"), so a catalogue unit that differs from
// the one the instrument is created with prints a name nobody can query.
func TestSelfObsSignalsParity(t *testing.T) {
	units := instrumentUnitsFromSource(t)
	want := instrumentNamesFromSource(t)
	gotSet := map[string]bool{}
	for _, s := range Signals() {
		if s.Type == signal.KindMetric {
			if s.Plane != signal.PlaneSelf {
				t.Errorf("self-obs metric %q has plane %q", s.Name, s.Plane)
			}
			gotSet[s.Name] = true
			if u, ok := units[s.Name]; ok && u != s.Unit {
				t.Errorf("descriptor %q has unit %q but the instrument is created with unit %q", s.Name, s.Unit, u)
			}
		}
	}
	for _, n := range want {
		if !gotSet[n] {
			t.Errorf("instrument %q in metrics.go has no descriptor in Signals()", n)
		}
	}
	for n := range gotSet {
		found := false
		for _, w := range want {
			if w == n {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("descriptor %q in Signals() has no matching instrument in metrics.go", n)
		}
	}
}
