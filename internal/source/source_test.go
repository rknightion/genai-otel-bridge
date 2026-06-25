// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/config"
)

type fakeSource struct {
	id    string
	loops []Loop
}

func (f fakeSource) ID() string    { return f.id }
func (f fakeSource) Loops() []Loop { return f.loops }

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
