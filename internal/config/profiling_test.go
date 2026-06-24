// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

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
