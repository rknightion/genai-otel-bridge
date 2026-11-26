// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
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
