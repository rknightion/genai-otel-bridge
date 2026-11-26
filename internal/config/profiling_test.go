// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rknightion/genai-otel-bridge/internal/config/gen/helmgen"
)

// [#382] the generated chart default config, with only mode: push and a push block layered on top
// (what a Helm values override does after deep-merge), must validate and not crash-loop.
func TestPushProfilingWithChartDefaults(t *testing.T) {
	block, err := helmgen.RenderConfigBlock(reflect.TypeFor[Config](), "config.go")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config map[string]any `yaml:"config"`
	}
	if err := yaml.Unmarshal(block, &doc); err != nil {
		t.Fatalf("parse rendered block: %v", err)
	}
	for _, m := range envRefRe.FindAllStringSubmatch(string(block), -1) {
		v := "v"
		if strings.Contains(m[1], "ENDPOINT") || strings.Contains(m[1], "URL") {
			v = "https://x.example"
		}
		t.Setenv(m[1], v)
	}
	sub := func(m map[string]any, k string) map[string]any {
		v, _ := m[k].(map[string]any)
		if v == nil {
			v = map[string]any{}
			m[k] = v
		}
		return v
	}
	prof := sub(sub(doc.Config, "selfobs"), "profiling")
	prof["enabled"], prof["mode"] = true, "push"
	prof["push"] = map[string]any{"endpoint": "https://profiles.example", "instance_id": "1", "token": "t"}
	raw, err := yaml.Marshal(doc.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}, "langsmith": {}}); err != nil {
		t.Fatalf("chart defaults + mode push must validate: %v", err)
	}
	// pull mode with no address still defaults to :6060.
	cfg2, err := LoadBytes([]byte("selfobs:\n  profiling:\n    enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Selfobs.Profiling.Pull.Addr != ":6060" {
		t.Fatalf("pull mode default addr: %q", cfg2.Selfobs.Profiling.Pull.Addr)
	}
}

func TestValidateProfiling(t *testing.T) {
	setEnv(t)
	base := func(t *testing.T) *Config {
		c, err := Load("testdata/valid.yaml")
		if err != nil {
			t.Fatalf("load base config: %v", err)
		}
		return c
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring; "" means expect no error
	}{
		{"disabled is fine even with junk mode", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: false, Mode: "nonsense"}
		}, ""},
		{"pull mode ok", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "pull", Pull: ProfilingPull{Addr: ":6060"}}
		}, ""},
		{"push mode ok https", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "push", Push: ProfilingPush{Endpoint: "https://profiles-prod-001.grafana.net", InstanceID: "123", Token: "tok"}}
		}, ""},
		{"unknown mode rejected", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "sideways"}
		}, "selfobs.profiling.mode must be pull|push"},
		{"push without endpoint rejected", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "push", Push: ProfilingPush{InstanceID: "123", Token: "tok"}}
		}, "selfobs.profiling.push.endpoint required"},
		{"push http endpoint rejected", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "push", Push: ProfilingPush{Endpoint: "http://profiles.example", InstanceID: "123", Token: "tok"}}
		}, "must be https://"},
		{"push http loopback allowed", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "push", Push: ProfilingPush{Endpoint: "http://localhost:4040", InstanceID: "123", Token: "tok"}}
		}, ""},
		{"push without creds rejected", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "push", Push: ProfilingPush{Endpoint: "https://profiles.example"}}
		}, "instance_id and token required"},
		{"push with pull.addr rejected", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "push", Pull: ProfilingPull{Addr: ":6060"}, Push: ProfilingPush{Endpoint: "https://profiles.example", InstanceID: "1", Token: "t"}}
		}, "pull.addr set but mode is push"},
		{"pull with push block rejected", func(c *Config) {
			c.Selfobs.Profiling = ProfilingConfig{Enabled: true, Mode: "pull", Push: ProfilingPush{Endpoint: "https://profiles.example"}}
		}, "push.* set but mode is pull"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base(t)
			tt.mutate(c)
			err := c.Validate(map[string]struct{}{"portkey": {}})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}
