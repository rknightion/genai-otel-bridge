// SPDX-License-Identifier: AGPL-3.0-only

// Package render turns telemetry signal descriptors into the generated Markdown region of
// docs/telemetry.md and splices it between markers. It mirrors internal/config/gen/helmgen: pure
// functions, no I/O, importable from a gate test.
package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
)

const (
	Begin = "<!-- >>> BEGIN generated telemetry catalogue — do not edit by hand; run `make generate` <<< -->"
	End   = "<!-- >>> END generated telemetry catalogue <<< -->"
)

func cell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

// Catalogue renders the full marker-wrapped region: BEGIN, grouped tables, END (trailing newline).
func Catalogue(sigs []signal.Signal) []byte {
	ordered := append([]signal.Signal(nil), sigs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SortKey() < ordered[j].SortKey() })

	var b strings.Builder
	b.WriteString(Begin)
	b.WriteString("\n\n")

	planes := []struct {
		p     signal.Plane
		title string
	}{{signal.PlaneProduct, "Product telemetry"}, {signal.PlaneSelf, "Self-observability"}}
	types := []struct {
		k     signal.Kind
		title string
	}{{signal.KindMetric, "Metrics"}, {signal.KindLog, "Logs"}, {signal.KindTrace, "Traces"}, {signal.KindAttribute, "Attributes"}}

	for _, pl := range planes {
		// Only emit a plane heading if it has at least one signal.
		any := false
		for _, s := range ordered {
			if s.Plane == pl.p {
				any = true
				break
			}
		}
		if !any {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", pl.title)
		for _, ty := range types {
			var rows []signal.Signal
			for _, s := range ordered {
				if s.Plane == pl.p && s.Type == ty.k {
					rows = append(rows, s)
				}
			}
			if len(rows) == 0 {
				continue
			}
			fmt.Fprintf(&b, "#### %s\n\n", ty.title)
			b.WriteString("| Name | Kind | Unit | Labels / attributes | Depends on | Description |\n")
			b.WriteString("|------|------|------|---------------------|-----------|-------------|\n")
			for _, s := range rows {
				fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n",
					cell(s.Name), cell(s.Instrument), cell(s.Unit),
					cell(strings.Join(s.Attributes, ", ")), cell(s.DependsOn), cell(s.Description))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(End)
	b.WriteString("\n")
	return []byte(b.String())
}

// Splice replaces the inclusive Begin..End region in existing with region. Errors if markers absent.
func Splice(existing, region []byte) ([]byte, error) {
	lines := strings.Split(string(existing), "\n")
	start, stop := -1, -1
	for i, l := range lines {
		if strings.TrimSpace(l) == Begin {
			start = i
		}
		if strings.TrimSpace(l) == End {
			stop = i
			break
		}
	}
	if start == -1 || stop == -1 || stop < start {
		return nil, fmt.Errorf("render: could not locate BEGIN/END markers in telemetry.md")
	}
	var out []string
	out = append(out, lines[:start]...)
	out = append(out, strings.Split(strings.TrimRight(string(region), "\n"), "\n")...)
	out = append(out, lines[stop+1:]...)
	return []byte(strings.Join(out, "\n")), nil
}

// Extract returns the inclusive Begin..End region (trailing newline) from existing.
func Extract(existing []byte) ([]byte, error) {
	lines := strings.Split(string(existing), "\n")
	start, stop := -1, -1
	for i, l := range lines {
		if strings.TrimSpace(l) == Begin {
			start = i
		}
		if strings.TrimSpace(l) == End {
			stop = i
			break
		}
	}
	if start == -1 || stop == -1 || stop < start {
		return nil, fmt.Errorf("render: could not locate BEGIN/END markers in telemetry.md")
	}
	return []byte(strings.Join(lines[start:stop+1], "\n") + "\n"), nil
}
