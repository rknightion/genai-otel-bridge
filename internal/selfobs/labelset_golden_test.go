// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/genai-otel-bridge/internal/loopid"
)

// legacySelfMetricLabelKeys is the attribute-key set each of the 22 self-metric instruments records
// today, written out by name. [unit 1, G6] The list is deliberately literal: it does not iterate
// Signals() and does not check completeness. A unit that adds an instrument pins its label set in its
// own test and does not edit this list.
var legacySelfMetricLabelKeys = map[string]string{
	"genai_otel_bridge_emitted_total":                           "loop,source",
	"genai_otel_bridge_emitted_logs_total":                      "loop,source",
	"genai_otel_bridge_samples_skipped_total":                   "loop,reason,source",
	"genai_otel_bridge_emit_errors_total":                       "kind,loop,source",
	"genai_otel_bridge_guard_dropped_total":                     "loop,source",
	"genai_otel_bridge_bucket_revised_after_settle_total":       "loop,source",
	"genai_otel_bridge_new_label_values_total":                  "series",
	"genai_otel_bridge_samples_capped_total":                    "loop,reason,source",
	"genai_otel_bridge_source_capability_total":                 "graph,loop,source,state",
	"genai_otel_bridge_source_data_incomplete_total":            "loop,reason,source",
	"genai_otel_bridge_auth_errors_total":                       "loop,source",
	"genai_otel_bridge_emit_partial_success_rejected_total":     "plane",
	"genai_otel_bridge_last_success_timestamp_seconds":          "loop,source",
	"genai_otel_bridge_window_lag_seconds":                      "loop,source",
	"genai_otel_bridge_queue_depth":                             "loop,source",
	"genai_otel_bridge_loop_degraded":                           "loop,reason,source",
	"genai_otel_bridge_leader":                                  "",
	"genai_otel_bridge_go_heap_inuse_bytes":                     "",
	"genai_otel_bridge_go_memory_limit_bytes":                   "",
	"genai_otel_bridge_upstream_request_duration_seconds":       "method,status_class,target",
	"genai_otel_bridge_emit_request_duration_seconds":           "plane,status_class",
	"genai_otel_bridge_bucket_revised_after_settle_age_seconds": "loop",
}

func attrKeys(set attribute.Set) string {
	var keys []string
	for _, kv := range set.ToSlice() {
		keys = append(keys, string(kv.Key))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// TestSelfMetricLabelSetsGolden checks the attribute keys of legacy recordings and loop seeds,
// plus the scope seeds and write failure counter a tenant pipeline records, through a manual reader.
func TestSelfMetricLabelSetsGolden(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader), metric.WithView(selfHistogramView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	id := loopid.ID{Source: "src", Loop: "lp"}
	m.SeedLoop(id, id.Loop)
	m.EmittedSamples(id, 1)
	m.EmittedLogs(id, 1)
	m.SamplesSkipped(id, "r", 1)
	m.EmitError(id, "k")
	m.GuardDropped(id, 1)
	m.BucketRevisedAfterSettle(id.Loop, id.Source, time.Minute)
	m.NewLabelValue("s")
	m.SamplesCapped(id, 1)
	m.SourceCapability(id.Loop, id.Source, "g", "st")
	m.SourceDataIncomplete(id.Loop, id.Source, "r")
	m.AuthError("lp", "src")
	m.QueueDepth(id, 1)
	m.LastSuccess(id, time.Unix(1700, 0))
	m.WindowLag(id, time.Minute)
	m.LoopDegraded(id, "r", true)
	m.SetLeader(true)
	m.ObserveEmitRequest("metrics", 200, nil, time.Millisecond)
	m.ObserveEmitRequest("logs", 0, errors.New("x"), time.Millisecond)
	m.ObserveEmitPartialReject("metrics", 1)
	m.ObserveUpstreamRequest("host", "GET", 200, nil, time.Millisecond)

	got := collectLabelSets(t, reader)
	for name, want := range legacySelfMetricLabelKeys {
		assertLabelSet(t, got, name, want)
	}

	// Check the legacy sets before With also seeds a pipeline-labelled partial reject series.
	pipelineMetrics := m.With("tenant", "pipeline")
	pipelineMetrics.SeedScope()
	pipelineMetrics.ScopeMatchWriteFailed()
	got = collectLabelSets(t, reader)
	assertLabelSet(t, got, "genai_otel_bridge_scope_assertion_total", "pipeline,result,tenant")
	assertLabelSet(t, got, "genai_otel_bridge_scope_match_write_failures_total", "pipeline,tenant")
}

func collectLabelSets(t *testing.T, reader *metric.ManualReader) map[string]map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]bool{}
	add := func(name string, set attribute.Set) {
		if got[name] == nil {
			got[name] = map[string]bool{}
		}
		got[name][attrKeys(set)] = true
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			switch d := mm.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					add(mm.Name, p.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					add(mm.Name, p.Attributes)
				}
			case metricdata.ExponentialHistogram[float64]:
				for _, p := range d.DataPoints {
					add(mm.Name, p.Attributes)
				}
			default:
				t.Errorf("%s: unexpected data type %T", mm.Name, mm.Data)
			}
		}
	}
	return got
}

func assertLabelSet(t *testing.T, got map[string]map[string]bool, name, want string) {
	t.Helper()
	sets := got[name]
	if len(sets) != 1 || !sets[want] {
		t.Errorf("%s: recorded attribute key sets %v, want exactly {%q}", name, sets, want)
	}
}
