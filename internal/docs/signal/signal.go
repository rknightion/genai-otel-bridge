// SPDX-License-Identifier: AGPL-3.0-only

// Package signal is the shared, dependency-free descriptor for a telemetry signal the bridge can
// emit. Each emitting package (selfobs, source/<vendor>) returns a []Signal from a Signals() func;
// the docs generator (internal/docs/gen) renders them into docs/telemetry.md. It imports nothing
// from internal/* so any package can return it without an import cycle.
package signal

import "strings"

type Plane string
type Kind string

const (
	PlaneProduct Plane = "product" // republished upstream telemetry (Portkey, LangSmith)
	PlaneSelf    Plane = "self"    // the bridge's own self-observability

	KindMetric    Kind = "metric"
	KindLog       Kind = "log"
	KindTrace     Kind = "trace"
	KindAttribute Kind = "attribute" // a metadata/resource attribute, not a standalone signal
)

// Signal describes one emittable telemetry signal. Name may be a literal (self-obs, fixed names) or a
// template using {placeholders} for config-derived parts (product, e.g. "{metric_prefix}_requests").
type Signal struct {
	Plane       Plane
	Type        Kind
	Source      string   // "selfobs", "portkey", "langsmith"
	Name        string   // literal or {template}
	Instrument  string   // "counter" | "gauge" | "histogram" | "" for logs/traces
	Unit        string   // UCUM-ish, e.g. "s", "1", "By"; "" if none
	Description string   // one line; no prompt/response/body content
	Attributes  []string // label / attribute keys carried by this signal
	DependsOn   string   // config dependency, e.g. "loops.groups.cost=true"; "" = always emitted
}

// SortKey gives a deterministic catalogue ordering: plane, then type, then name.
func (s Signal) SortKey() string {
	return strings.Join([]string{string(s.Plane), string(s.Type), s.Name}, "\x00")
}
