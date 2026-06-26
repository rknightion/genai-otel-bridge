// SPDX-License-Identifier: AGPL-3.0-only

// Package otlp hand-encodes OTLP/HTTP. Modelled on a sibling tool's OTLP sink, but with DETERMINISTIC
// ordering (that tool does not sort): attribute KVs and series are sorted so re-emitting a
// settled batch is byte-identical — the precondition for conditional sink idempotency (§3.3/§4.4).
package otlp

import (
	"fmt"
	"sort"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

const scopeName = "genai-otel-bridge"

// sortedKVs converts a map to []*KeyValue in sorted-key order (deterministic).
func sortedKVs(m map[string]string) []*commonpb.KeyValue {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*commonpb.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, &commonpb.KeyValue{
			Key:   k,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: m[k]}},
		})
	}
	return out
}

// Encode builds an ExportMetricsServiceRequest body (gauge-only for v1). identity ⇒ resource
// attributes; each Sample ⇒ one Gauge NumberDataPoint at TimeUnixNano=bucket time. Rejects
// Delta temporality (GC OTLP ingests cumulative only). Output is deterministic.
func Encode(identity map[string]string, samples []model.Sample) ([]byte, error) {
	// Stable series order: sort by (name, unit, sorted-label-string, timestamp). [ext-review-10] Unit
	// is part of the grouping key below, so it MUST also be part of the sort key — otherwise two
	// same-name samples differing only by unit sort by input order, producing non-deterministic bytes
	// and splitting the (name,unit) group across multiple Metric messages.
	sorted := append([]model.Sample(nil), samples...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		if sorted[i].Unit != sorted[j].Unit {
			return sorted[i].Unit < sorted[j].Unit
		}
		lki := labelKey(sorted[i].Labels)
		lkj := labelKey(sorted[j].Labels)
		if lki != lkj {
			return lki < lkj
		}
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	// [CP-M2] Group samples by (name, unit) into ONE Metric with multiple data points — NOT multiple
	// Metric messages sharing a name (some OTLP consumers reject/merge those unpredictably). `sorted`
	// is ordered by (name, label-key), so same-name samples are contiguous (e.g. latency quantiles).
	var metrics []*metricspb.Metric
	var cur *metricspb.Metric
	var curGauge *metricspb.Gauge
	for _, s := range sorted {
		if s.Temporality == model.Delta { // [AR-M-delta] reject ANY Delta (GC gateway is cumulative-only)
			return nil, fmt.Errorf("otlp: Delta temporality not accepted by the GC gateway (use Cumulative): %s", s.Name)
		}
		if s.Kind != model.Gauge {
			return nil, fmt.Errorf("otlp: v1 emits Gauge only, got kind=%d for %s", s.Kind, s.Name)
		}
		dp := &metricspb.NumberDataPoint{
			Attributes:   sortedKVs(s.Labels),
			TimeUnixNano: uint64(s.Timestamp.UTC().UnixNano()),
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: s.Value},
		}
		if cur == nil || cur.Name != s.Name || cur.Unit != s.Unit {
			curGauge = &metricspb.Gauge{}
			cur = &metricspb.Metric{Name: s.Name, Unit: s.Unit, Data: &metricspb.Metric_Gauge{Gauge: curGauge}}
			metrics = append(metrics, cur)
		}
		curGauge.DataPoints = append(curGauge.DataPoints, dp)
	}

	rm := &metricspb.ResourceMetrics{
		Resource: &resourcepb.Resource{Attributes: sortedKVs(identity)},
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: scopeName},
			Metrics: metrics,
		}},
	}
	body, err := proto.Marshal(rm)
	if err != nil {
		return nil, fmt.Errorf("otlp: marshal ResourceMetrics: %w", err)
	}
	// Hand-wrap as ExportMetricsServiceRequest field 1 (avoids importing collector/*).
	var buf []byte
	buf = protowire.AppendTag(buf, 1, protowire.BytesType)
	buf = protowire.AppendBytes(buf, body)
	return buf, nil
}

// labelKey is a deterministic sort tiebreaker for a label set. [ext-review-11] It LENGTH-PREFIXES
// each key/value component so distinct label sets can never collide into the same string — the old
// "k=v;" join made {"a":"b;c=d"} and {"a":"b","c":"d"} identical, which would let two genuinely
// different same-(name,unit,ts) samples sort by input order and break byte-determinism (the
// conditional-idempotency precondition). The emitted attributes themselves come from sortedKVs; this
// is purely the ordering key.
func labelKey(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%d:%s%d:%s", len(k), k, len(m[k]), m[k])
	}
	return b.String()
}
