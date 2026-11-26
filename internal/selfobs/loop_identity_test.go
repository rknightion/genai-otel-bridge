// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

type identityMemCP struct {
	mu sync.Mutex
	w  map[string]model.Watermark
}

func (c *identityMemCP) Load(_ context.Context, k model.CheckpointKey) (model.Watermark, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w[k.String()], nil
}

func (c *identityMemCP) Save(_ context.Context, k model.CheckpointKey, w model.Watermark) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkpoint.CheckMonotonic(c.w[k.String()], w); err != nil {
		return err
	}
	c.w[k.String()] = w
	return nil
}

type namedLoop struct{ key model.CheckpointKey }

func (l namedLoop) Key() model.CheckpointKey { return l.key }
func (l namedLoop) Cadence() time.Duration   { return time.Minute }
func (l namedLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, nil
}

type nullEmitter struct{}

func (nullEmitter) Emit(context.Context, model.Batch) error { return nil }

// lastSuccessSeries drives one committed quiet window per loop through a real LoopRunner and the real
// selfobs instruments, then returns the {source, loop} attribute pairs of the last_success data points.
func lastSuccessSeries(t *testing.T, loops []source.Loop) []string {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatal(err)
	}
	cp := &identityMemCP{w: map[string]model.Watermark{}}
	ctx := coordinate.WithEpoch(context.Background(), 1)
	for i, l := range loops {
		r := schedule.NewLoopRunner(l, nullEmitter{}, cp, source.NewGuard(source.GuardConfig{}), 1, 1, m)
		// Distinct times, so a shared series would keep only the last instance's value.
		r.ProcessBatch(ctx, model.Batch{Key: l.Key(), Watermark: model.Watermark{Time: time.Unix(1_700_000_000+int64(60*i), 0).UTC()}})
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "genai_otel_bridge_last_success_timestamp_seconds" {
				continue
			}
			g, ok := mt.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("last_success is %T, want a float64 gauge", mt.Data)
			}
			for _, dp := range g.DataPoints {
				src, _ := dp.Attributes.Value("source")
				loop, _ := dp.Attributes.Value("loop")
				out = append(out, src.AsString()+"|"+loop.AsString())
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestSameNamedLoopsHaveDistinctSeries [#387]: per-loop self-metrics carry the configured source
// instance as `source`, and a fan-out logs_export instance carries its own `loop` value, so two loop
// instances never share (and overwrite) one last-value gauge series. Both values come from
// configuration, so the label set stays bounded by configuration.
func TestSameNamedLoopsHaveDistinctSeries(t *testing.T) {
	t.Run("two_sources_same_loop_name", func(t *testing.T) {
		got := lastSuccessSeries(t, []source.Loop{
			namedLoop{key: model.CheckpointKey{SourceInstance: "portkey-a", Loop: "analytics", OutputFingerprint: "fp"}},
			namedLoop{key: model.CheckpointKey{SourceInstance: "portkey-b", Loop: "analytics", OutputFingerprint: "fp"}},
		})
		want := []string{"portkey-a|analytics", "portkey-b|analytics"}
		if !equalStrings(got, want) {
			t.Fatalf("last_success series=%v want %v (one per source instance)", got, want)
		}
	})
	t.Run("fan_out_logs_export", func(t *testing.T) {
		src, err := portkey.New(config.SourceConfig{
			Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1", SourceInstance: "portkey-test",
			Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "tok"},
			RateLimit: config.RateLimitConfig{RPS: 1, Burst: 3},
			Loops: map[string]config.LoopConfig{"logs_export": {Enabled: true, Settings: map[string]string{
				"workspace_id": "ws-x", "signed_url_allow_hosts": "host.example.com",
			}}},
			APIKeyUseCases: []config.APIKeyUseCase{
				{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
				{Label: "Content Gen", APIKeyIDs: []string{"uuid-b"}},
			},
		}, source.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if len(src.Loops()) != 2 || src.Loops()[0].Key().Loop != src.Loops()[1].Key().Loop {
			t.Fatalf("precondition: two fan-out instances sharing Key().Loop, got %d", len(src.Loops()))
		}
		got := lastSuccessSeries(t, src.Loops())
		want := []string{"portkey-test|logs_export,api_key_use_case=content_gen", "portkey-test|logs_export,api_key_use_case=data_gen"}
		if !equalStrings(got, want) {
			t.Fatalf("last_success series=%v want %v (one per fan-out instance)", got, want)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
