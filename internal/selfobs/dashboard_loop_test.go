// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"gopkg.in/yaml.v3"

	"github.com/rknightion/genai-otel-bridge/internal/loopid"
)

// fanoutLoop is the self-metrics loop name of a fan-out logs_export instance; its hook-fed counters carry
// the bare Key().Loop (fanoutBase). plainLoop is an ordinary loop, the same on every instrument.
const (
	fanoutBase = "base"
	fanoutLoop = "base,api_key_use_case=uc"
	plainLoop  = "plain"
)

// loopValuesByInstrument records, through the real instruments, the loop label each per-loop
// instrument carries for the fan-out instance: the runner-fed ones carry its distinct name, the
// hook-fed ones (recorded via the composition root's source hooks) the bare loop name.
func loopValuesByInstrument(t *testing.T) map[string]string {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatal(err)
	}
	id := loopid.ID{Source: "s", Loop: fanoutLoop}
	// Runner-fed (schedule.LoopID).
	m.EmittedSamples(id, 1)
	m.EmittedLogs(id, 1)
	m.SamplesSkipped(id, "too_old", 1)
	m.EmitError(id, "collect")
	m.GuardDropped(id, 1)
	m.SamplesCapped(id, 1)
	m.QueueDepth(id, 1)
	m.LastSuccess(id, time.Unix(1, 0))
	m.WindowLag(id, time.Second)
	m.LoopDegraded(id, "r", true)
	// Hook-fed (bare loop name from the source).
	m.BucketRevisedAfterSettle(fanoutBase, time.Second)
	m.SourceCapability(fanoutBase, "g", "permission-denied")
	m.SourceDataIncomplete(fanoutBase, "window_truncated")
	m.AuthError(fanoutBase, "s")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	add := func(name string, loop string) {
		if prev, ok := out[name]; ok && prev != loop {
			t.Fatalf("%s carries two loop values %q and %q", name, prev, loop)
		}
		out[name] = loop
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			collect := func(loop string, ok bool) {
				if !ok {
					return
				}
				add(mt.Name, loop)
				if mt.Unit == "1" { // the OTLP->Prometheus translation adds _ratio to a unit-1 gauge
					add(mt.Name+"_ratio", loop)
				}
			}
			switch d := mt.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					v, ok := dp.Attributes.Value("loop")
					collect(v.AsString(), ok)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					v, ok := dp.Attributes.Value("loop")
					collect(v.AsString(), ok)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					v, ok := dp.Attributes.Value("loop")
					collect(v.AsString(), ok)
				}
			case metricdata.ExponentialHistogram[float64]:
				for _, dp := range d.DataPoints {
					v, ok := dp.Attributes.Value("loop")
					collect(v.AsString(), ok)
				}
			}
		}
	}
	// Completeness: every catalogued instrument with a loop attribute was exercised above.
	for _, s := range Signals() {
		for _, a := range s.Attributes {
			if _, ok := out[s.Name]; a == "loop" && !ok {
				t.Fatalf("instrument %s has a loop attribute but was not exercised; add its call above", s.Name)
			}
		}
	}
	return out
}

var (
	selectorRe  = regexp.MustCompile(`(genai_otel_bridge_[a-zA-Z0-9_]+|genai-otel-bridge:[a-zA-Z0-9_:]+)(\{[^}]*\})?`)
	loopMatchRe = regexp.MustCompile(`(?:^\{|,)\s*loop\s*(=~|=)\s*"([^"]*)"`)
)

// exprs returns every string under an "expr" key, and the recording rule's "metric" name if any.
func exprs(t *testing.T, path string) (out []string, record string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var walk func(n *yaml.Node, depth int)
	walk = func(n *yaml.Node, depth int) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Value == "expr" && v.Kind == yaml.ScalarNode {
					out = append(out, v.Value)
				}
				if k.Value == "metric" && v.Kind == yaml.ScalarNode && depth == 1 {
					record = v.Value
				}
				walk(v, depth+1)
			}
			return
		}
		for _, c := range n.Content {
			walk(c, depth)
		}
	}
	walk(&root, 0)
	return out, record
}

// TestDashboardLoopFilterMatchesEveryInstrument [#387]: the dashboard's $loop variable offers loop
// values taken from a runner-fed metric, while hook-fed counters carry the bare loop name. For a
// fan-out logs_export instance (and an ordinary loop), every panel selector that filters on $loop must
// match the loop value its own metric carries when the variable is set to the value it offers for that
// instance; otherwise selecting the loop silently hides the panel. No rule may join hook-fed and
// runner-fed metrics, whose loop values differ for a fan-out instance.
func TestDashboardLoopFilterMatchesEveryInstrument(t *testing.T) {
	fanout := loopValuesByInstrument(t) // metric -> loop value for the fan-out instance
	dir := "../../deploy/grafana/self-obs"

	// Recording rules inherit the loop label of the metric they read.
	rules, err := filepath.Glob(filepath.Join(dir, "*rule-*.yaml"))
	if err != nil || len(rules) == 0 {
		t.Fatalf("no rule files: %v", err)
	}
	for _, f := range rules {
		es, record := exprs(t, f)
		vals := map[string]bool{}
		for _, e := range es {
			for _, m := range selectorRe.FindAllStringSubmatch(e, -1) {
				if v, ok := fanout[m[1]]; ok {
					vals[v] = true
				}
			}
		}
		if len(vals) > 1 {
			t.Errorf("%s joins hook-fed and runner-fed metrics whose loop values differ for a fan-out instance: %v", filepath.Base(f), vals)
		}
		if record != "" && len(vals) == 1 {
			for v := range vals {
				fanout[record] = v
			}
		}
	}

	dash, _ := exprs(t, filepath.Join(dir, "dashboard-self-obs.yaml"))
	b, err := os.ReadFile(filepath.Join(dir, "dashboard-self-obs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Variables []struct {
				Spec struct {
					Name       string `yaml:"name"`
					Definition string `yaml:"definition"`
					Regex      string `yaml:"regex"`
				} `yaml:"spec"`
			} `yaml:"variables"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var offered func(metricLoop string) string
	for _, v := range doc.Spec.Variables {
		if v.Spec.Name != "loop" {
			continue
		}
		src := regexp.MustCompile(`^label_values\(([a-zA-Z0-9_:]+),\s*loop\)$`).FindStringSubmatch(v.Spec.Definition)
		if src == nil {
			t.Fatalf("$loop definition %q is not label_values(<metric>, loop)", v.Spec.Definition)
		}
		srcVal, ok := fanout[src[1]]
		if !ok {
			t.Fatalf("$loop reads %s, which carries no known loop label", src[1])
		}
		var re *regexp.Regexp
		if r := strings.Trim(v.Spec.Regex, "/"); r != "" {
			re = regexp.MustCompile(r)
		}
		// Grafana keeps the first capture group of the variable regex (or the whole value without one).
		offered = func(metricLoop string) string {
			if metricLoop == fanoutBase || metricLoop == fanoutLoop {
				metricLoop = srcVal
			}
			if re == nil {
				return metricLoop
			}
			m := re.FindStringSubmatch(metricLoop)
			if m == nil {
				return ""
			}
			if len(m) > 1 {
				return m[1]
			}
			return m[0]
		}
	}
	if offered == nil {
		t.Fatal("dashboard has no $loop variable")
	}

	checked := 0
	for _, e := range dash {
		for _, m := range selectorRe.FindAllStringSubmatch(e, -1) {
			lm := loopMatchRe.FindStringSubmatch(m[2])
			if lm == nil || !strings.Contains(lm[2], "$loop") {
				continue
			}
			want, ok := fanout[m[1]]
			if !ok {
				t.Errorf("panel filters %s on $loop but its loop label is unknown: %s", m[1], e)
				continue
			}
			for _, carried := range []string{want, plainLoop} {
				v := offered(carried)
				pat := strings.ReplaceAll(lm[2], "$loop", regexp.QuoteMeta(v))
				var match bool
				if lm[1] == "=" {
					match = pat == carried
				} else {
					match = regexp.MustCompile("^(?:" + pat + ")$").MatchString(carried)
				}
				if !match {
					t.Errorf("selecting $loop=%q hides %s{loop=%q}: matcher loop%s%q in %s", v, m[1], carried, lm[1], lm[2], e)
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no dashboard selector filters on $loop; the check proved nothing")
	}
}
