// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withExtraYAML writes valid.yaml plus extra top-level YAML to a temp file and returns its path,
// so a test can exercise real parsing of an added key without a permanent fixture.
func withExtraYAML(t *testing.T, extra string) string {
	t.Helper()
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(string(base)+"\n"+extra+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGovernanceBudgetDefaults(t *testing.T) {
	setEnv(t)
	cfg, err := Load("testdata/valid.yaml") // no governance block ⇒ default applies
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.PerMetricCardinalityBudget != 10000 {
		t.Fatalf("unset per_metric_cardinality_budget should default to 10000 (0 would mean unlimited), got %d", cfg.Governance.PerMetricCardinalityBudget)
	}
}

func TestGovernanceBudgetExplicit(t *testing.T) {
	setEnv(t)
	cfg, err := Load(withExtraYAML(t, "governance:\n  per_metric_cardinality_budget: 500"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.PerMetricCardinalityBudget != 500 {
		t.Fatalf("explicit per_metric_cardinality_budget should be preserved, got %d", cfg.Governance.PerMetricCardinalityBudget)
	}
}

func TestValidateRejectsNegativeBudget(t *testing.T) {
	setEnv(t)
	cfg, _ := Load("testdata/valid.yaml")
	cfg.Governance.PerMetricCardinalityBudget = -1
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected negative per_metric_cardinality_budget to be rejected")
	}
}

func TestLogFormatUnsetIsEmpty(t *testing.T) {
	setEnv(t)
	cfg, err := Load("testdata/valid.yaml") // no log block ⇒ empty; the handler layer defaults empty→logfmt
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Format != "" {
		t.Fatalf("unset log.format should be empty, got %q", cfg.Log.Format)
	}
}

func TestLogFormatParsedAndValidated(t *testing.T) {
	setEnv(t)
	cfg, err := Load(withExtraYAML(t, "log:\n  format: json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Format != "json" {
		t.Fatalf("log.format should parse to json, got %q", cfg.Log.Format)
	}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("json is a valid log.format: %v", err)
	}
	bad, _ := Load("testdata/valid.yaml")
	bad.Log.Format = "xml"
	if err := bad.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected invalid log.format to be rejected")
	}
}

func TestLogLevelParsedAndValidated(t *testing.T) {
	setEnv(t)
	cfg, err := Load(withExtraYAML(t, "log:\n  level: debug"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("log.level should parse to debug, got %q", cfg.Log.Level)
	}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("debug is a valid log.level: %v", err)
	}
	for _, lvl := range []string{"", "info", "warn", "error"} {
		ok, _ := Load("testdata/valid.yaml")
		ok.Log.Level = lvl
		if err := ok.Validate(map[string]struct{}{"portkey": {}}); err != nil {
			t.Fatalf("log.level %q should be valid: %v", lvl, err)
		}
	}
	bad, _ := Load("testdata/valid.yaml")
	bad.Log.Level = "trace"
	if err := bad.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected invalid log.level to be rejected")
	}
}

func setEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GC_OTLP_ENDPOINT": "https://otlp.example", "GC_INSTANCE_ID": "123",
		"GC_OTLP_TOKEN": "tok", "ENV": "dev", "PORTKEY_API_KEY": "pk",
	} {
		t.Setenv(k, v)
	}
}

func TestLoadValid(t *testing.T) {
	setEnv(t)
	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatal(err)
	}
	lp := cfg.Sources[0].Loops["analytics"]
	if time.Duration(lp.Window) != 50*time.Minute || time.Duration(lp.BucketSettle) != 3*time.Minute {
		t.Fatalf("durations wrong: %+v", lp)
	}
	if cfg.Sources[0].Auth.Value != "pk" {
		t.Fatalf("secret not resolved: %q", cfg.Sources[0].Auth.Value)
	}
}

func TestUnsetSecretFatal(t *testing.T) {
	setEnv(t)
	os.Unsetenv("PORTKEY_API_KEY")
	if _, err := Load("testdata/valid.yaml"); err == nil {
		t.Fatal("expected fatal on unset ${PORTKEY_API_KEY}")
	}
}

func TestValidateRejects(t *testing.T) {
	setEnv(t)
	cfg, _ := Load("testdata/valid.yaml")
	// Unknown source type.
	if err := cfg.Validate(map[string]struct{}{"langsmith": {}}); err == nil {
		t.Fatal("expected unknown-type error")
	}
	// Window > 55m.
	cfg2, _ := Load("testdata/valid.yaml")
	lp := cfg2.Sources[0].Loops["analytics"]
	lp.Window = Duration(56 * time.Minute)
	cfg2.Sources[0].Loops["analytics"] = lp
	if err := cfg2.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected window>55m error")
	}
	// cadence*(1+2·jitter) + settle > window  ⇒ rejected (AR-M-win).
	cfg3, _ := Load("testdata/valid.yaml")
	lp3 := cfg3.Sources[0].Loops["analytics"]
	lp3.Window = Duration(3 * time.Minute) // 3m < 60s*1.2 + 3m = 4.2m
	cfg3.Sources[0].Loops["analytics"] = lp3
	if err := cfg3.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected lower-bound window error")
	}
	// [round3 MEDIUM-1] a cleartext (non-loopback) self endpoint must be rejected (token leak).
	cfg4, _ := Load("testdata/valid.yaml")
	cfg4.Emit.Self = &OTLPTarget{OTLP: OTLPConn{Endpoint: "http://collector.internal:4318", InstanceID: "1", Token: "t"}}
	if err := cfg4.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected cleartext emit.self.otlp.endpoint to be rejected")
	}
	// ...but loopback self endpoint is fine (dev).
	cfg5, _ := Load("testdata/valid.yaml")
	cfg5.Emit.Self = &OTLPTarget{OTLP: OTLPConn{Endpoint: "http://localhost:4318"}}
	if err := cfg5.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("loopback self endpoint should be allowed, got %v", err)
	}
}

// TestValidateSnapshotLoop covers an aggregate-now (snapshot) loop: it declares no window, so the
// time-bucket window/settle/backfill checks (which encode portkey semantics) MUST be skipped — only
// the cadence floor still applies. It also asserts the decoupled `settings` map round-trips from YAML.
func TestValidateSnapshotLoop(t *testing.T) {
	setEnv(t)
	t.Setenv("LANGSMITH_API_KEY", "ls")
	cfg, err := Load("testdata/snapshot.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(map[string]struct{}{"langsmith": {}}); err != nil {
		t.Fatalf("snapshot loop (no window) should validate, got: %v", err)
	}
	lp := cfg.Sources[0].Loops["sessions"]
	if time.Duration(lp.Window) != 0 {
		t.Fatalf("expected zero window (snapshot), got %s", time.Duration(lp.Window))
	}
	if lp.Settings["stats_window"] != "1h" || lp.Settings["max_sessions"] != "1000" {
		t.Fatalf("settings map did not round-trip: %+v", lp.Settings)
	}
}

func TestMaxDPMDefaultsToOne(t *testing.T) {
	setEnv(t)
	cfg, err := Load("testdata/valid.yaml") // no governance block ⇒ default applies
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.MaxDPM != 1 {
		t.Fatalf("expected max_dpm default 1; got %d", cfg.Governance.MaxDPM)
	}
}

func TestValidateRejectsNegativeMaxDPM(t *testing.T) {
	setEnv(t)
	cfg, _ := Load("testdata/valid.yaml")
	cfg.Governance.MaxDPM = -1
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected negative max_dpm to be rejected")
	}
}

func TestSelfIntervalFloorScalesWithMaxDPM(t *testing.T) {
	setEnv(t)
	self := func(mi Duration) *OTLPTarget {
		return &OTLPTarget{OTLP: OTLPConn{Endpoint: "https://otlp.example/otlp", InstanceID: "1", Token: "t"}, MetricInterval: mi}
	}
	// max_dpm=2 → floor 30s: 45s OK, 20s rejected.
	cfg, _ := Load("testdata/valid.yaml")
	cfg.Governance.MaxDPM = 2
	cfg.Emit.Self = self(Duration(45 * time.Second))
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("45s should pass at max_dpm=2 (floor 30s): %v", err)
	}
	cfg2, _ := Load("testdata/valid.yaml")
	cfg2.Governance.MaxDPM = 2
	cfg2.Emit.Self = self(Duration(20 * time.Second))
	if err := cfg2.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("20s should be rejected at max_dpm=2 (floor 30s)")
	}
}

// Self-obs export interval below 60s violates the 1DPM emission constraint; unset/≥60s is fine.
func TestValidateSelfMetricInterval(t *testing.T) {
	setEnv(t)
	self := func(mi Duration) *OTLPTarget {
		return &OTLPTarget{OTLP: OTLPConn{Endpoint: "https://otlp.example/otlp", InstanceID: "1", Token: "t"}, MetricInterval: mi}
	}
	// Sub-minute ⇒ rejected.
	cfg, _ := Load("testdata/valid.yaml")
	cfg.Emit.Self = self(Duration(30 * time.Second))
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected sub-60s emit.self.metric_interval to be rejected (1DPM)")
	}
	// Exactly 60s ⇒ ok.
	cfg2, _ := Load("testdata/valid.yaml")
	cfg2.Emit.Self = self(Duration(time.Minute))
	if err := cfg2.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("60s self interval should pass, got %v", err)
	}
	// Unset (0 ⇒ provider default 60s) ⇒ ok.
	cfg3, _ := Load("testdata/valid.yaml")
	cfg3.Emit.Self = self(0)
	if err := cfg3.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("unset self interval should pass, got %v", err)
	}
}
