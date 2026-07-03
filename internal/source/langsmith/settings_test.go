// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// TestNewParsesSettings asserts New reads the LangSmith-specific knobs from the decoupled per-loop
// settings map (overlaying the defaults), and that a malformed value for a known key fails fast.
func TestNewParsesSettings(t *testing.T) {
	sc := config.SourceConfig{
		Type: "langsmith", Enabled: true, BaseURL: "https://ls.example",
		SourceInstance: "ls", Auth: config.AuthConfig{Header: "x-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 3},
		Loops: map[string]config.LoopConfig{
			"sessions": {Enabled: true, Cadence: config.Duration(time.Minute), MetricPrefix: "langsmith",
				Settings: map[string]string{
					"stats_window":        "30m",
					"max_sessions":        "5",
					"session_label_value": "name",
					"emit_feedback":       "false",
					"feedback_keys":       "a, b",
				}},
		},
	}
	src, err := New(sc, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	lp, ok := src.Loops()[0].(*sessionsLoop)
	if !ok {
		t.Fatalf("expected *sessionsLoop, got %T", src.Loops()[0])
	}
	if lp.statsWindow != 30*time.Minute {
		t.Errorf("statsWindow = %s, want 30m", lp.statsWindow)
	}
	if lp.maxSessions != 5 {
		t.Errorf("maxSessions = %d, want 5", lp.maxSessions)
	}
	if !lp.deriveCfg.useName {
		t.Errorf("session_label_value=name should set useName")
	}
	if lp.deriveCfg.emitFeedback {
		t.Errorf("emit_feedback=false should disable feedback")
	}
	if lp.deriveCfg.sessionLabelKey != "session" {
		t.Errorf("sessionLabelKey = %q, want fixed default \"session\" (not operator-settable)", lp.deriveCfg.sessionLabelKey)
	}
	if !lp.deriveCfg.feedbackAllow["a"] || !lp.deriveCfg.feedbackAllow["b"] {
		t.Errorf("feedback_keys not parsed: %+v", lp.deriveCfg.feedbackAllow)
	}

	// A malformed value for a known key must fail fast, not silently default.
	bad := sc
	bad.Loops = map[string]config.LoopConfig{
		"sessions": {Enabled: true, Cadence: config.Duration(time.Minute),
			Settings: map[string]string{"max_sessions": "not-a-number"}},
	}
	if _, err := New(bad, source.Deps{}); err == nil {
		t.Fatal("expected error on malformed max_sessions")
	}
}

// TestSessionLabelValueEnum (#102): session_label_value must be exactly "id" or "name"; any other value
// (incl. a case typo) is a hard error, not a silent fall-back to id — for BOTH the sessions and usage
// apply functions (shared validator). This enforces the same malformed=hard-error contract as the rest
// of the file, so operator intent (human-readable names) is never silently discarded.
func TestSessionLabelValueEnum(t *testing.T) {
	cases := []struct {
		v       string
		wantErr bool
	}{
		{"id", false},
		{"name", false},
		{"Name", true},  // case typo — previously silently meant "id"
		{"names", true}, // plural typo
		{"NAME", true},
		{"", true},
	}
	for _, tc := range cases {
		t.Run("sessions/"+tc.v, func(t *testing.T) {
			cfg := Config{}
			err := applySettings(&cfg, map[string]string{"session_label_value": tc.v})
			if (err != nil) != tc.wantErr {
				t.Fatalf("applySettings(%q): err=%v wantErr=%v", tc.v, err, tc.wantErr)
			}
			if !tc.wantErr && cfg.SessionLabelValue != tc.v {
				t.Fatalf("valid value %q must be assigned, got %q", tc.v, cfg.SessionLabelValue)
			}
		})
		t.Run("usage/"+tc.v, func(t *testing.T) {
			cfg := usageConfig{}
			err := applyUsageSettings(&cfg, map[string]string{"session_label_value": tc.v})
			if (err != nil) != tc.wantErr {
				t.Fatalf("applyUsageSettings(%q): err=%v wantErr=%v", tc.v, err, tc.wantErr)
			}
			if !tc.wantErr && cfg.SessionLabelValue != tc.v {
				t.Fatalf("valid value %q must be assigned, got %q", tc.v, cfg.SessionLabelValue)
			}
		})
	}
}

// TestExampleSourceBuilds asserts the chart example the generator renders is itself a valid, buildable
// LangSmith source (so the documented example can't drift into an unbuildable shape).
func TestExampleSourceBuilds(t *testing.T) {
	ex := ExampleSource()
	if ex.Type != "langsmith" {
		t.Fatalf("example type = %q, want langsmith", ex.Type)
	}
	if _, err := New(ex, source.Deps{}); err != nil {
		t.Fatalf("ExampleSource() must be buildable: %v", err)
	}
}
