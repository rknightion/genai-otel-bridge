// SPDX-License-Identifier: AGPL-3.0-only

// Package model is the vendor-neutral seam between sources and the emitter.
// Sources produce these types; the emitter consumes them. Nothing else crosses this boundary.
// FROZEN — see ARCHITECTURE.md §4. Do not add/rename fields without a design change.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// MetricKind selects the OTLP metric representation. v1 emits Gauge ONLY.
type MetricKind int

const (
	Gauge MetricKind = iota
	Sum
)

// Temporality is carried by Sum only (ignored for Gauge). The GC OTLP gateway ingests
// CUMULATIVE only; the emitter rejects Delta for that target (ARCHITECTURE.md §10).
type Temporality int

const (
	TempUnset Temporality = iota
	Delta
	Cumulative
)

// Sample is one derived metric data point.
type Sample struct {
	Name        string
	Kind        MetricKind
	Temporality Temporality
	Monotonic   bool
	Unit        string
	Labels      map[string]string
	Value       float64
	Timestamp   time.Time
}

// LogRecord is one structured log line (no message bodies/content, ever). The two attribute maps are
// SEMANTIC INTENT fields — named for what they are FOR, not for any one backend's storage model.
// OTLP has resource attributes and log-record attributes; Loki "stream labels" / "structured metadata"
// (and Prometheus labels) are backend MAPPING OUTCOMES, not model primitives.
//   - IndexedAttributes: low-cardinality routing/query identity. Maps to OTLP resource attributes; a
//     backend may surface them as Loki stream labels (only if promoted) or Prometheus labels. This is
//     the cardinality-DANGEROUS tier — the governance guard (deny-by-default allow-list + per-series
//     budget) polices it exactly as it does Sample.Labels. High-cardinality per-record ids
//     (correlation_id, run_id, request ids) must NOT go here. Global producer identity
//     (service.namespace, environment, source instance) is set once on the emitter as resource
//     attributes — do not duplicate it per record.
//   - RecordAttributes: non-indexed per-record context. Maps to OTLP log-record attributes; a backend
//     may surface them as Loki structured metadata / searchable metadata / ordinary OTLP attributes.
type LogRecord struct {
	Timestamp         time.Time
	Body              string
	Severity          string
	IndexedAttributes map[string]string // low-cardinality routing/query identity (guard-policed)
	RecordAttributes  map[string]string // non-indexed per-record metadata/context
	// TraceID is the OTLP log-record trace id (16 bytes; empty = unset). It carries a source-provided
	// correlation id (e.g. a Portkey request-metadata correlation_id) through to OTLP `trace_id`, so a
	// backend can link these operational logs to the originating application's traces. This is
	// correlation passthrough, NOT span synthesis — we never invent spans from the gateway hop (ledger
	// #4/#15). The same value is also carried as a content-free RecordAttribute by the source.
	TraceID []byte
}

// CheckpointKey namespaces a watermark (Cdx-C4): a new instance/workspace/region, a changed
// prefix/label set, or a new series each get their own key, so a new series bootstraps its own history.
type CheckpointKey struct {
	SourceInstance    string
	Loop              string
	OutputFingerprint string
}

func (k CheckpointKey) String() string {
	return k.SourceInstance + "/" + k.Loop + "/" + k.OutputFingerprint
}

// Watermark is a loop's forward-only position. Opaque to the core; interpreted by the source.
type Watermark struct {
	Time   time.Time // last fully-emitted (or skipped-with-gap) observation time — monotonic
	Cursor string    // optional source-specific resume token
	Epoch  int64     // leader lease epoch that wrote it — for write fencing (Cdx-C14)
}

// Batch is the unit produced by one Collect and consumed by one Emit.
type Batch struct {
	Key       CheckpointKey
	Samples   []Sample
	Logs      []LogRecord
	Watermark Watermark // advances the key iff this batch emits (or is skipped-with-gap)
}

// Fingerprint hashes the emitted series set + naming config into the CheckpointKey's
// OutputFingerprint. Order-insensitive over series names so config reordering is a no-op,
// but adding/removing a series changes it (F37: new series bootstraps its own history).
func Fingerprint(seriesNames []string, namingConfig string) string {
	s := append([]string(nil), seriesNames...)
	sort.Strings(s)
	h := sha256.New()
	for _, n := range s {
		h.Write([]byte(n))
		h.Write([]byte{0})
	}
	h.Write([]byte(strings.TrimSpace(namingConfig)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}
