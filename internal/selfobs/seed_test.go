// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"fmt"
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

	"github.com/rknightion/genai-otel-bridge/internal/loopid"
)

// reasonsReadByRules returns, per counter, the reason values read by the bundled rule files. A read of
// the counter with no literal reason matcher cannot be seeded, so it is reported as "*".
func reasonsReadByRules(t *testing.T, names ...string) map[string][]string {
	t.Helper()
	return labelReadByRules(t, "reason", names...)
}

// labelReadByRules is reasonsReadByRules for any label key (reason, kind, source).
func labelReadByRules(t *testing.T, label string, names ...string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("../../deploy/grafana/self-obs/*rule-*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no rule files found: %v", err)
	}
	out := map[string][]string{}
	for _, name := range names {
		re := regexp.MustCompile(regexp.QuoteMeta("genai_otel_bridge_"+name+"_total") + `(\{[^}]*\})?`)
		reason := regexp.MustCompile(`\b` + regexp.QuoteMeta(label) + `(=~|=)"([^"]*)"`)
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

// TestSeededEmitKindsAndAuthMatchBundledAlertRules [#517]: a rule reading an emit kind that is not
// seeded fails, as does a seeded kind no rule reads. The auth rule reads every series (no source
// matcher), so any source matcher appearing in a rule must be reflected in the seed first.
func TestSeededEmitKindsAndAuthMatchBundledAlertRules(t *testing.T) {
	kinds := labelReadByRules(t, "kind", "emit_errors")["emit_errors"]
	if want := slices.Sorted(slices.Values(alertedEmitKinds)); !slices.Equal(kinds, want) {
		t.Errorf("emit_errors: rule files read kinds %v but SeedLoop seeds %v (update the seed list)", kinds, want)
	}
	if src := labelReadByRules(t, "source", "auth_errors")["auth_errors"]; len(src) != 1 || src[0] != "*" {
		t.Errorf("auth_errors: rule files read sources %v; SeedLoop seeds every loop's own source only (want just *)", src)
	}
}

// TestSeedLoopSeedsEmitAndAuthSeries [#517]: each series exists at 0 under the exact attribute set its
// producer records, and the first failure steps that same series 0 -> 1 (no second series is born).
func TestSeedLoopSeedsEmitAndAuthSeries(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatal(err)
	}
	id := loopid.ID{Source: "portkey-a", Loop: "analytics"}
	m.SeedLoop(id, "analytics")
	collect := func(name string) map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, mt := range sm.Metrics {
				if mt.Name != name {
					continue
				}
				for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
					out[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
				}
			}
		}
		return out
	}
	emit := collect("genai_otel_bridge_emit_errors_total")
	if len(emit) != len(alertedEmitKinds) {
		t.Fatalf("emit_errors series seeded: %v, want exactly %d kinds", emit, len(alertedEmitKinds))
	}
	for _, k := range alertedEmitKinds {
		if v, ok := emit["kind="+k+",loop=analytics,source=portkey-a"]; !ok || v != 0 {
			t.Errorf("emit kind %s: present=%v value=%d, want present at 0", k, ok, v)
		}
	}
	if auth := collect("genai_otel_bridge_auth_errors_total"); len(auth) != 1 || auth["loop=analytics,source=portkey-a"] != 0 {
		t.Fatalf("auth_errors seeded: %v, want one series {loop=analytics,source=portkey-a} at 0", auth)
	}
	// AC3: the first failures move the seeded series, creating none.
	m.EmitError(id, "retryable_exhausted")
	m.AuthError("analytics", "portkey-a")
	emit = collect("genai_otel_bridge_emit_errors_total")
	if len(emit) != len(alertedEmitKinds) || emit["kind=retryable_exhausted,loop=analytics,source=portkey-a"] != 1 {
		t.Errorf("after first emit error: %v, want the seeded series at 1 and no new series", emit)
	}
	if auth := collect("genai_otel_bridge_auth_errors_total"); len(auth) != 1 || auth["loop=analytics,source=portkey-a"] != 1 {
		t.Errorf("after first auth error: %v, want the seeded series at 1", auth)
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
	m.SeedLoop(loopid.ID{Source: "s", Loop: "loop_a"}, "loop_a")
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

// TestSeedLoopSeedsTheSeriesTheRunnerRecords [#387]: the runner records samples_skipped under
// {source, loop, reason}, so the seed must create that same series. A seed under {loop, reason} alone
// is a different series: the first real event would then be born at 1 and increase() would miss it.
func TestSeedLoopSeedsTheSeriesTheRunnerRecords(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatal(err)
	}
	id := loopid.ID{Source: "portkey-a", Loop: "analytics"}
	m.SeedLoop(id, id.Loop)
	m.SamplesSkipped(id, "too_old", 1)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var points []string
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "genai_otel_bridge_samples_skipped_total" {
				continue
			}
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				if r, _ := dp.Attributes.Value("reason"); r.AsString() == "too_old" {
					src, _ := dp.Attributes.Value("source")
					points = append(points, fmt.Sprintf("%s=%d", src.AsString(), dp.Value))
				}
			}
		}
	}
	if len(points) != 1 || points[0] != "portkey-a=1" {
		t.Fatalf("too_old series after seed + one event: %v, want one series {source=portkey-a} stepping 0 -> 1", points)
	}
}
