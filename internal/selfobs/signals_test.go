// SPDX-License-Identifier: AGPL-3.0-only
package selfobs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
)

// instrumentNamesFromSource extracts the string-literal first arg of every mk/mg/mh(...) call in
// metrics.go and prefixes "genai_otel_bridge_" (the constructors add that prefix at runtime).
func instrumentNamesFromSource(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "metrics.go", nil, 0)
	if err != nil {
		t.Fatalf("parse metrics.go: %v", err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || (id.Name != "mk" && id.Name != "mg" && id.Name != "mh") || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		names = append(names, "genai_otel_bridge_"+lit.Value[1:len(lit.Value)-1])
		return true
	})
	sort.Strings(names)
	return names
}

func TestSelfObsSignalsParity(t *testing.T) {
	want := instrumentNamesFromSource(t)
	gotSet := map[string]bool{}
	for _, s := range Signals() {
		if s.Type == signal.KindMetric {
			if s.Plane != signal.PlaneSelf {
				t.Errorf("self-obs metric %q has plane %q", s.Name, s.Plane)
			}
			gotSet[s.Name] = true
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
