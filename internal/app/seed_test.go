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
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

type seedRecorder struct {
	schedule.NoopMetrics
	seeded []string
}

func (s *seedRecorder) SeedLoop(loop string) { s.seeded = append(s.seeded, loop) }

// Build seeds the alerted counters once per configured loop, under the loop name the runner reports (#499).
func TestBuildSeedsEachConfiguredLoop(t *testing.T) {
	cfg := minimalConfig("http://127.0.0.1:1")
	cp, err := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	em := otlp.New(otlp.Config{Endpoint: "http://127.0.0.1:1", InstanceID: "1", Token: "t", MaxBytes: 1 << 20})
	rec := &seedRecorder{}
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, em, rec, source.Deps{})
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
	}
}
