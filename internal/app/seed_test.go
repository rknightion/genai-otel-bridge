// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

type seedRecorder struct {
	schedule.NoopMetrics
	seeded []string
	ids    []schedule.LoopID
}

func (s *seedRecorder) SeedLoop(id schedule.LoopID, hookLoop string) {
	s.ids = append(s.ids, id)
	s.seeded = append(s.seeded, hookLoop)
}

// Build seeds the alerted counters once per configured loop, under the loop name the runner reports (#499).
func TestBuildSeedsEachConfiguredLoop(t *testing.T) {
	cfg := minimalConfig("http://127.0.0.1:1")
	cp, err := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	em := otlp.New(otlp.Config{Endpoint: "http://127.0.0.1:1", InstanceID: "1", Token: "t", MaxBytes: 1 << 20})
	rec := &seedRecorder{}
	ef, tf := fixedFactories(em, rec)
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, ef, tf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	specs := a.Specs()
	if len(specs) == 0 || len(rec.seeded) != len(specs) {
		t.Fatalf("seeded %v for %d loops", rec.seeded, len(specs))
	}
	for i, sp := range specs {
		if rec.seeded[i] != sp.Loop.Key().Loop {
			t.Errorf("seeded[%d]=%q want %q", i, rec.seeded[i], sp.Loop.Key().Loop)
		}
		// [#387] the skipped seed must use the identity the runner records under.
		if want := schedule.MetricsLoopID(sp.Loop); rec.ids[i] != want {
			t.Errorf("seeded id[%d]=%+v want %+v", i, rec.ids[i], want)
		}
	}
}

// TestSourceHooksSeparateSameNamedLoops exercises the real Collect hooks that Build wires to the
// self-metrics. Distinct configured sources of the same type must never increment each other's series.
func TestSourceHooksSeparateSameNamedLoops(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	bucket := base.Add(-5 * time.Minute).Format(time.RFC3339)
	var value atomic.Int64
	value.Store(1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/analytics/graphs/requests":
			fmt.Fprintf(w, `{"data_points":[{"timestamp":%q,"total":%d}]}`, bucket, value.Load())
		case "/analytics/groups/ai-models":
			fmt.Fprint(w, `{"data":[{"ai_model":"a","requests":2},{"ai_model":"b","requests":1}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := minimalConfig(upstream.URL)
	for i, src := range []string{"source-a", "source-b"} {
		sc := minimalConfig(upstream.URL).Sources[0]
		sc.SourceInstance = src
		analytics := sc.Loops["analytics"]
		analytics.MetricPrefix = fmt.Sprintf("fixture_%d", i)
		sc.Loops["analytics"] = analytics
		sc.Loops["groups"] = config.LoopConfig{
			Enabled: true, Cadence: config.Duration(5 * time.Minute), MetricPrefix: analytics.MetricPrefix,
			Settings: map[string]string{"max_groups": "1", "emit_prompts": "false", "metadata_keys": "fixture"},
		}
		if i == 0 {
			cfg.Sources[0] = sc
		} else {
			cfg.Sources = append(cfg.Sources, sc)
		}
	}
	cp, err := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })
	m, err := selfobs.NewMetrics(provider)
	if err != nil {
		t.Fatal(err)
	}
	ef, tf := fixedFactories(nil, m)
	a, err := Build(ctx, cfg, cp, coordinate.Noop{}, ef, tf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range a.Specs() {
		value.Store(1)
		portkey.SetLoopClockForTest(sp.Loop, func() time.Time { return base })
		b, err := sp.Loop.Collect(ctx, model.Watermark{})
		if err != nil {
			t.Fatal(err)
		}
		if sp.Loop.Key().Loop == "analytics" {
			value.Store(2)
			portkey.SetLoopClockForTest(sp.Loop, func() time.Time { return base.Add(time.Minute) })
			if _, err := sp.Loop.Collect(ctx, b.Watermark); err != nil {
				t.Fatal(err)
			}
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	for name, labels := range map[string]string{
		"bucket_revised_after_settle_total": "loop=analytics,source=",
		"source_capability_total":           "graph=metadata/fixture,loop=groups,source=",
		"source_data_incomplete_total":      "loop=groups,reason=window_truncated,source=",
	} {
		got := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, mm := range sm.Metrics {
				if mm.Name == "genai_otel_bridge_"+name {
					for _, dp := range mm.Data.(metricdata.Sum[int64]).DataPoints {
						if dp.Value != 0 {
							got[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
						}
					}
				}
			}
		}
		suffix := ""
		if name == "source_capability_total" {
			suffix = ",state=transient-404"
		}
		want := map[string]int64{labels + "source-a" + suffix: 1, labels + "source-b" + suffix: 1}
		if !maps.Equal(got, want) {
			t.Errorf("%s active series = %v, want %v", name, got, want)
		}
	}
}
