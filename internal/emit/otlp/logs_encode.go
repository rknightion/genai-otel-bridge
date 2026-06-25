// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"bytes"
	"fmt"
	"maps"
	"sort"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// EncodeLogs builds an ExportLogsServiceRequest body from log records. Records are GROUPED by their
// IndexedAttributes into one ResourceLogs per distinct set, with that group's Resource =
// identity ∪ IndexedAttributes — because OTLP resource attributes are what a backend promotes to Loki
// stream labels (GS1). RecordAttributes become per-record log-record attributes (Loki structured
// metadata). Body is empty in v1 (FR10 — no content) ⇒ omitted. Output is DETERMINISTIC (sorted group
// order, sorted records within a group, sorted KVs) so a re-emitted chunk is byte-identical.
func EncodeLogs(identity map[string]string, logs []model.LogRecord) ([]byte, error) {
	type group struct {
		indexed map[string]string
		recs    []model.LogRecord
	}
	groups := map[string]*group{}
	for _, lr := range logs {
		k := labelKey(lr.IndexedAttributes)
		g := groups[k]
		if g == nil {
			g = &group{indexed: lr.IndexedAttributes}
			groups[k] = g
		}
		g.recs = append(g.recs, lr)
	}
	order := make([]string, 0, len(groups))
	for k := range groups {
		order = append(order, k)
	}
	sort.Strings(order) // deterministic ResourceLogs ordering

	var wrapped []byte
	for _, k := range order {
		g := groups[k]
		recs := append([]model.LogRecord(nil), g.recs...)
		sort.SliceStable(recs, func(i, j int) bool {
			if !recs[i].Timestamp.Equal(recs[j].Timestamp) {
				return recs[i].Timestamp.Before(recs[j].Timestamp)
			}
			if recs[i].Severity != recs[j].Severity {
				return recs[i].Severity < recs[j].Severity
			}
			if c := strings.Compare(labelKey(recs[i].RecordAttributes), labelKey(recs[j].RecordAttributes)); c != 0 {
				return c < 0
			}
			// Final tiebreaker on TraceID: today it derives from a record attr (so equal attrs ⇒ equal
			// TraceID), but pin byte-identical re-emit ordering even if a future source sets it independently.
			return bytes.Compare(recs[i].TraceID, recs[j].TraceID) < 0
		})
		lrs := make([]*logspb.LogRecord, 0, len(recs))
		for _, r := range recs {
			lr := &logspb.LogRecord{
				TimeUnixNano: uint64(r.Timestamp.UTC().UnixNano()),
				SeverityText: r.Severity,
				Attributes:   sortedKVs(r.RecordAttributes),
			}
			if len(r.TraceID) == 16 { // source-provided correlation id → OTLP trace_id (logs↔traces)
				lr.TraceId = r.TraceID
			}
			if r.Body != "" { // v1 keeps Body empty (FR10); honour a non-empty value defensively if ever set
				lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: r.Body}}
			}
			lrs = append(lrs, lr)
		}
		rl := &logspb.ResourceLogs{
			Resource: &resourcepb.Resource{Attributes: sortedKVs(mergeAttrs(identity, g.indexed))},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: scopeName},
				LogRecords: lrs,
			}},
		}
		body, err := proto.Marshal(rl)
		if err != nil {
			return nil, fmt.Errorf("otlp: marshal ResourceLogs: %w", err)
		}
		// Hand-wrap each ResourceLogs as a (repeated) field 1 of ExportLogsServiceRequest — same
		// collector-import-free pattern as Encode; logs just have >1 ResourceLogs (one per indexed group).
		wrapped = protowire.AppendTag(wrapped, 1, protowire.BytesType)
		wrapped = protowire.AppendBytes(wrapped, body)
	}
	return wrapped, nil
}

// mergeAttrs returns a ∪ b (b wins on key conflict). Global producer identity (a) and per-group routing
// identity (b) are distinct axes and shouldn't collide; the merge is order-independent for disjoint keys.
func mergeAttrs(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}
