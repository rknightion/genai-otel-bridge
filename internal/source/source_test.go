// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"context"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

type fakeSource struct {
	id    string
	loops []Loop
}

func (f fakeSource) ID() string    { return f.id }
func (f fakeSource) Loops() []Loop { return f.loops }

// declLoop is a Loop that declares its series (implements SeriesDeclarer). An empty series slice models a
// logs-only loop.
type declLoop struct {
	loop   string
	series []string
}

func (d declLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{SourceInstance: "s", Loop: d.loop}
}
func (d declLoop) Cadence() time.Duration { return time.Minute }
func (d declLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, nil
}
func (d declLoop) SeriesNames() []string { return d.series }

// nonDeclLoop is a Loop that does NOT implement SeriesDeclarer (the wiring bug ValidateOwnership catches).
type nonDeclLoop struct{ loop string }

func (n nonDeclLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{SourceInstance: "s", Loop: n.loop}
}
func (n nonDeclLoop) Cadence() time.Duration { return time.Minute }
func (n nonDeclLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, nil
}

// TestRegisterDuplicatePanics (#133): a second registration under an existing type must fail loud, not
// silently shadow the first vendor's constructor.
func TestRegisterDuplicatePanics(t *testing.T) {
	reg := NewRegistry()
	ctor := func(config.SourceConfig, Deps) (Source, error) { return fakeSource{id: "x"}, nil }
	reg.Register("portkey", ctor)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register of the same type must panic")
		}
	}()
	reg.Register("portkey", ctor) // duplicate → panic
}

// TestValidateOwnershipRequiresDeclarer (#63): a loop that does not implement SeriesDeclarer is a wiring
// bug — rejected loud at startup — while logs-only loops declaring an empty slice build fine.
func TestValidateOwnershipRequiresDeclarer(t *testing.T) {
	// A non-declaring loop is rejected.
	if err := ValidateOwnership([]Source{fakeSource{id: "s", loops: []Loop{nonDeclLoop{loop: "mystery"}}}}); err == nil {
		t.Fatal("a loop that does not implement SeriesDeclarer must be rejected")
	}
	// A logs-only loop (empty declaration) + a metric loop with distinct series build fine.
	ok := []Source{fakeSource{id: "s", loops: []Loop{
		declLoop{loop: "logs", series: nil},
		declLoop{loop: "metrics", series: []string{"foo_requests"}},
	}}}
	if err := ValidateOwnership(ok); err != nil {
		t.Fatalf("valid declaring loops rejected: %v", err)
	}
	// Two loops declaring the same normalized series still collide.
	dup := []Source{fakeSource{id: "s", loops: []Loop{
		declLoop{loop: "a", series: []string{"foo.requests"}},
		declLoop{loop: "b", series: []string{"foo_requests"}}, // collides after NormalizeSeriesName
	}}}
	if err := ValidateOwnership(dup); err == nil {
		t.Fatal("post-normalisation duplicate series must be rejected")
	}
}

func TestRegistryBuildAndKnown(t *testing.T) {
	reg := NewRegistry()
	reg.Register("portkey", func(config.SourceConfig, Deps) (Source, error) { return fakeSource{id: "portkey"}, nil })
	if _, ok := reg.Known()["portkey"]; !ok {
		t.Fatal("portkey not registered")
	}
	s, err := reg.Build(config.SourceConfig{Type: "portkey"}, Deps{})
	if err != nil || s.ID() != "portkey" {
		t.Fatalf("build: %v %v", s, err)
	}
	if _, err := reg.Build(config.SourceConfig{Type: "nope"}, Deps{}); err == nil {
		t.Fatal("unknown type must error")
	}
}
