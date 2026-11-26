// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// reasonsReadByRules returns, per counter, the reason values read by the bundled rule files. A read of
// the counter with no literal reason matcher cannot be seeded, so it is reported as "*".
func reasonsReadByRules(t *testing.T, names ...string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("../../deploy/grafana/self-obs/*rule-*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no rule files found: %v", err)
	}
	out := map[string][]string{}
	for _, name := range names {
		re := regexp.MustCompile(regexp.QuoteMeta("genai_otel_bridge_"+name+"_total") + `(\{[^}]*\})?`)
		reason := regexp.MustCompile(`reason(=~|=)"([^"]*)"`)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			// Only PromQL expressions count: annotations quote the metric name in prose.
			for _, line := range strings.Split(string(b), "\n") {
				if !strings.Contains(line, "expr:") {
					continue
				}
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					rm := reason.FindStringSubmatch(m[1])
					if rm == nil {
						out[name] = append(out[name], "*")
						continue
					}
					if rm[1] == "=" {
						out[name] = append(out[name], rm[2])
					} else {
						out[name] = append(out[name], strings.Split(rm[2], "|")...)
					}
				}
			}
		}
		sort.Strings(out[name])
		out[name] = slices.Compact(out[name])
	}
	return out
}

func TestSeededReasonsMatchBundledAlertRules(t *testing.T) {
	read := reasonsReadByRules(t, "source_data_incomplete", "samples_skipped")
	for name, want := range map[string][]string{
		"source_data_incomplete": alertedIncompleteReasons,
		"samples_skipped":        alertedSkipReasons,
	} {
		got := slices.Sorted(slices.Values(want))
		if !slices.Equal(read[name], got) {
			t.Errorf("%s: rule files read reasons %v but SeedLoop seeds %v (update the seed list)", name, read[name], got)
		}
	}
}

func seededSeries(t *testing.T, reader metric.Reader) map[string]map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				loop, _ := dp.Attributes.Value(attribute.Key("loop"))
				reason, ok := dp.Attributes.Value(attribute.Key("reason"))
				if !ok {
					continue
				}
				if out[mt.Name] == nil {
					out[mt.Name] = map[string]int64{}
				}
				out[mt.Name][loop.AsString()+"/"+reason.AsString()] = dp.Value
			}
		}
	}
	return out
}

func TestSeedLoopCreatesAlertedSeriesAtZeroAndNothingElse(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatal(err)
	}
	m.SeedLoop("loop_a")
	read := reasonsReadByRules(t, "source_data_incomplete", "samples_skipped")
	got := seededSeries(t, reader)
	for name, reasons := range map[string][]string{
		"genai_otel_bridge_source_data_incomplete_total": read["source_data_incomplete"],
		"genai_otel_bridge_samples_skipped_total":        read["samples_skipped"],
	} {
		if len(got[name]) != len(reasons) {
			t.Errorf("%s: %d series seeded, want exactly the %d reasons a rule reads: %v", name, len(got[name]), len(reasons), got[name])
		}
		for _, r := range reasons {
			v, ok := got[name]["loop_a/"+r]
			if !ok || v != 0 {
				t.Errorf("%s{loop=loop_a,reason=%s}: present=%v value=%d, want present at 0", name, r, ok, v)
			}
		}
	}
	// AC3: the first window_oversize event moves the series from 0 to 1, a rise increase() can see.
	m.SourceDataIncomplete("loop_a", "window_oversize")
	after := seededSeries(t, reader)["genai_otel_bridge_source_data_incomplete_total"]["loop_a/window_oversize"]
	if after != 1 {
		t.Errorf("after first event value=%d, want 1 (0 -> 1 step)", after)
	}
}
