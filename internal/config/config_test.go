// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"strings"
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

// TestValidateDynamoDBHA covers the DynamoDB HA validation rules added in Task C1.
func TestValidateDynamoDBHA(t *testing.T) {
	knownSources := map[string]struct{}{"portkey": {}}
	// base returns a freshly loaded valid *Config for mutation.
	base := func(t *testing.T) *Config {
		t.Helper()
		setEnv(t)
		cfg, err := Load("testdata/valid.yaml")
		if err != nil {
			t.Fatalf("Load valid.yaml: %v", err)
		}
		return cfg
	}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // empty ⇒ expect no error
	}{
		{
			name: "coordinator dynamodb requires checkpoint dynamodb",
			mutate: func(c *Config) {
				c.HA.Coordinator = "dynamodb"
				c.HA.Checkpoint = "configmap"
				c.HA.DynamoDB.Table = "t"
			},
			wantErr: "ha.coordinator=dynamodb requires ha.checkpoint=dynamodb",
		},
		{
			name: "dynamodb checkpoint requires table",
			mutate: func(c *Config) {
				c.HA.Coordinator = "none"
				c.HA.Checkpoint = "dynamodb"
				// DynamoDB.Table intentionally left empty
			},
			wantErr: "ha.dynamodb.table is required",
		},
		{
			name: "good dynamodb pair",
			mutate: func(c *Config) {
				c.HA.Coordinator = "dynamodb"
				c.HA.Checkpoint = "dynamodb"
				c.HA.DynamoDB.Table = "t"
				c.HA.DynamoDB.LeaseDuration = Duration(15 * time.Second)
				c.HA.DynamoDB.RenewDeadline = Duration(10 * time.Second)
				c.HA.DynamoDB.RetryPeriod = Duration(2 * time.Second)
			},
			wantErr: "",
		},
		{
			name: "lease must exceed renew deadline",
			mutate: func(c *Config) {
				c.HA.Coordinator = "dynamodb"
				c.HA.Checkpoint = "dynamodb"
				c.HA.DynamoDB.Table = "t"
				c.HA.DynamoDB.LeaseDuration = Duration(5 * time.Second)
				c.HA.DynamoDB.RenewDeadline = Duration(10 * time.Second)
				c.HA.DynamoDB.RetryPeriod = Duration(2 * time.Second)
			},
			wantErr: "ha.dynamodb.lease_duration must be > renew_deadline",
		},
		{
			name: "unknown coordinator rejected",
			mutate: func(c *Config) {
				c.HA.Coordinator = "etcd"
			},
			wantErr: "ha.coordinator must be lease|none|dynamodb",
		},
		{
			name: "unknown checkpoint rejected",
			mutate: func(c *Config) {
				c.HA.Checkpoint = "redis"
			},
			wantErr: "ha.checkpoint must be configmap|file|dynamodb",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base(t)
			tc.mutate(c)
			err := c.Validate(knownSources)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestDynamoDBLoadDefaults verifies that Load fills in the DynamoDB defaults when coordinator or
// checkpoint is set to dynamodb (via YAML), so callers never see zero-value durations.
func TestDynamoDBLoadDefaults(t *testing.T) {
	setEnv(t)
	// Write a minimal valid config with coordinator+checkpoint=dynamodb to a temp file.
	// Cannot use withExtraYAML (it appends, but valid.yaml already has ha:).
	const dynYAML = `emit:
  telemetry:
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  service_namespace: genai-otel-bridge
  deployment_environment: ${ENV}
ha:
  coordinator: dynamodb
  checkpoint: dynamodb
  dynamodb:
    table: mytable
queue:
  max_batches: 256
  max_batch_bytes: 1048576
  emit_workers: 1
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    rate_limit: { rps: 1, burst: 3 }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
        bucket_settle: 3m
        bootstrap_lookback: 50m
        max_backfill: 55m
        metric_prefix: portkey_api
        graphs: [requests, cost, tokens, latency, errors]
`
	p := filepath.Join(t.TempDir(), "dyn.yaml")
	if err := os.WriteFile(p, []byte(dynYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HA.DynamoDB.LockName != "genai-otel-bridge-leader" {
		t.Errorf("LockName default: got %q, want genai-otel-bridge-leader", cfg.HA.DynamoDB.LockName)
	}
	if time.Duration(cfg.HA.DynamoDB.LeaseDuration) != 15*time.Second {
		t.Errorf("LeaseDuration default: got %s, want 15s", time.Duration(cfg.HA.DynamoDB.LeaseDuration))
	}
	if time.Duration(cfg.HA.DynamoDB.RenewDeadline) != 10*time.Second {
		t.Errorf("RenewDeadline default: got %s, want 10s", time.Duration(cfg.HA.DynamoDB.RenewDeadline))
	}
	if time.Duration(cfg.HA.DynamoDB.RetryPeriod) != 2*time.Second {
		t.Errorf("RetryPeriod default: got %s, want 2s", time.Duration(cfg.HA.DynamoDB.RetryPeriod))
	}
}

// TestValidateRejectsNegativeLoopDurations (#38): config.Duration decodes "-10m" fine; a negative
// per-loop settle/backfill/lookback must be rejected by name (a negative settle even loosens the M3
// window check, so it can't be left to that check to catch).
func TestValidateRejectsNegativeLoopDurations(t *testing.T) {
	setEnv(t)
	cases := map[string]func(*LoopConfig){
		"bucket_settle":      func(l *LoopConfig) { l.BucketSettle = Duration(-10 * time.Minute) },
		"max_backfill":       func(l *LoopConfig) { l.MaxBackfill = Duration(-time.Hour) },
		"bootstrap_lookback": func(l *LoopConfig) { l.BootstrapLookback = Duration(-time.Minute) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, _ := Load("testdata/valid.yaml")
			lp := cfg.Sources[0].Loops["analytics"]
			mut(&lp)
			cfg.Sources[0].Loops["analytics"] = lp
			err := cfg.Validate(map[string]struct{}{"portkey": {}})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("expected negative %s to be rejected by name, got %v", name, err)
			}
		})
	}
}

// TestValidateRejectsNegativeDynamoDBDurations (#38): lease=-5s <= renew=-10s is false, so the ordering
// check wrongly "passes" — an explicit non-negativity check must catch it.
func TestValidateRejectsNegativeDynamoDBDurations(t *testing.T) {
	setEnv(t)
	cfg, _ := Load("testdata/valid.yaml")
	cfg.HA.Coordinator = "dynamodb"
	cfg.HA.Checkpoint = "dynamodb"
	cfg.HA.DynamoDB.Table = "t"
	cfg.HA.DynamoDB.LeaseDuration = Duration(-5 * time.Second)
	cfg.HA.DynamoDB.RenewDeadline = Duration(-10 * time.Second)
	cfg.HA.DynamoDB.RetryPeriod = Duration(-2 * time.Second)
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected negative ha.dynamodb durations to be rejected")
	}
}

// TestValidateRejectsRetryPeriodGERenewDeadline (#46): the DynamoDB renew ticker cadence IS
// retry_period; retry >= renew lets the lock expire between renews → recurring split-brain.
func TestValidateRejectsRetryPeriodGERenewDeadline(t *testing.T) {
	setEnv(t)
	base := func() *Config {
		cfg, _ := Load("testdata/valid.yaml")
		cfg.HA.Coordinator = "dynamodb"
		cfg.HA.Checkpoint = "dynamodb"
		cfg.HA.DynamoDB.Table = "t"
		cfg.HA.DynamoDB.LeaseDuration = Duration(15 * time.Second)
		cfg.HA.DynamoDB.RenewDeadline = Duration(10 * time.Second)
		return cfg
	}
	bad := base()
	bad.HA.DynamoDB.RetryPeriod = Duration(20 * time.Second)
	if err := bad.Validate(map[string]struct{}{"portkey": {}}); err == nil || !strings.Contains(err.Error(), "retry_period must be < renew_deadline") {
		t.Fatalf("expected retry>=renew to be rejected, got %v", err)
	}
	ok := base()
	ok.HA.DynamoDB.RetryPeriod = Duration(2 * time.Second) // the default 15s/10s/2s must still validate
	if err := ok.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("default retry_period 2s should pass, got %v", err)
	}
}

// TestValidateRejectsEmptySelfEndpoint (#41): a present emit.self with an empty otlp.endpoint does NOT
// fall back to emit.telemetry — it exports to a malformed URL — so require the endpoint explicitly.
func TestValidateRejectsEmptySelfEndpoint(t *testing.T) {
	setEnv(t)
	cfg, _ := Load("testdata/valid.yaml")
	cfg.Emit.Self = &OTLPTarget{MetricInterval: Duration(2 * time.Minute)} // set a sub-key but no endpoint
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil || !strings.Contains(err.Error(), "emit.self.otlp.endpoint is required") {
		t.Fatalf("expected empty emit.self.otlp.endpoint to be rejected, got %v", err)
	}
	// No emit.self block ⇒ still valid (documented fallback to emit.telemetry).
	cfg2, _ := Load("testdata/valid.yaml")
	if err := cfg2.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("no emit.self block should validate, got %v", err)
	}
	// Fully specified emit.self ⇒ valid.
	cfg3, _ := Load("testdata/valid.yaml")
	cfg3.Emit.Self = &OTLPTarget{OTLP: OTLPConn{Endpoint: "https://otlp.example/otlp", InstanceID: "1", Token: "t"}}
	if err := cfg3.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("fully specified emit.self should validate, got %v", err)
	}
}

// TestValidateRejectsBadRateLimit (#39): a struct-built config (bypassing Load's defaulting) with
// burst 0 or non-positive rps must be rejected before it reaches rate.NewLimiter.
func TestValidateRejectsBadRateLimit(t *testing.T) {
	setEnv(t)
	cfg, _ := Load("testdata/valid.yaml")
	s := cfg.Sources[0]
	s.RateLimit = RateLimitConfig{RPS: 1, Burst: 0}
	cfg.Sources[0] = s
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil || !strings.Contains(err.Error(), "burst") {
		t.Fatalf("expected burst 0 to be rejected, got %v", err)
	}
	cfg2, _ := Load("testdata/valid.yaml")
	s2 := cfg2.Sources[0]
	s2.RateLimit = RateLimitConfig{RPS: -1, Burst: 3}
	cfg2.Sources[0] = s2
	if err := cfg2.Validate(map[string]struct{}{"portkey": {}}); err == nil || !strings.Contains(err.Error(), "rps") {
		t.Fatalf("expected negative rps to be rejected, got %v", err)
	}
}

// TestLoadDefaultsRateLimitAndBucketSettle (#39, #52): an omitted rate_limit block must default to
// rps=1/burst=3 (never burst 0), and an omitted bucket_settle must default to 10m (never settle=0),
// and the resulting config must validate.
func TestLoadDefaultsRateLimitAndBucketSettle(t *testing.T) {
	setEnv(t)
	const y = `emit:
  telemetry:
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  deployment_environment: ${ENV}
ha:
  coordinator: none
  checkpoint: file
queue:
  emit_workers: 1
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
        bootstrap_lookback: 50m
        max_backfill: 90m
        metric_prefix: portkey_api
        graphs: [requests]
`
	cfg, err := LoadBytes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Sources[0]
	if s.RateLimit.RPS != 1 || s.RateLimit.Burst != 3 {
		t.Fatalf("omitted rate_limit should default to rps=1/burst=3, got %+v", s.RateLimit)
	}
	if got := time.Duration(s.Loops["analytics"].BucketSettle); got != 10*time.Minute {
		t.Fatalf("omitted bucket_settle should default to 10m, got %s", got)
	}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("defaulted config should validate, got %v", err)
	}
}

// TestLoadBytesResolvesEnv covers the file-less config path (the ECS GENAI_OTEL_BRIDGE_CONFIG delivery):
// LoadBytes parses raw YAML in-memory and resolves ${ENV} secret refs, identically to Load — so no temp
// file is needed and a read-only root filesystem is fine.
func TestLoadBytesResolvesEnv(t *testing.T) {
	t.Setenv("TEST_OTLP_EP", "https://otlp.example.com")
	cfg, err := LoadBytes([]byte("emit:\n  telemetry:\n    otlp:\n      endpoint: ${TEST_OTLP_EP}\n"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if got := cfg.Emit.Telemetry.OTLP.Endpoint; got != "https://otlp.example.com" {
		t.Fatalf("endpoint=%q, want the resolved env value", got)
	}
}

// TestParseErrorRedactsResolvedSecret (#111): when a resolved secret value lands in a field that fails
// to decode (here a duration field parameterised by a non-duration secret), the resulting error must
// NOT contain the secret — otherwise it reaches stderr → the container log → Loki. It must be redacted.
func TestParseErrorRedactsResolvedSecret(t *testing.T) {
	const secret = "sk-supersecret-abc123-value"
	t.Setenv("TEST_SETTLE", secret)
	_, err := LoadBytes([]byte("sources:\n  - type: portkey\n    loops:\n      analytics:\n        bucket_settle: ${TEST_SETTLE}\n"))
	if err == nil {
		t.Fatal("expected a decode error for a non-duration secret value")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("config parse error leaked the resolved secret: %s", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("expected the secret to be [redacted] in the error, got: %s", err)
	}
}

// TestQueueDepthDefaults (#114): a config that sets the mandatory queue.emit_workers but omits
// max_batches/max_batch_bytes must load with the documented 256 / 1 MiB defaults (mirroring the helm
// render tags and the governance defaulting pattern) — NOT the runner's depth-1 clamp or a disabled
// proactive over-cap split (MaxBytes 0). Struct/env-injected configs bypass helm's tag defaults.
func TestQueueDepthDefaults(t *testing.T) {
	setEnv(t)
	const y = `
emit:
  telemetry:
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  deployment_environment: ${ENV}
ha:
  coordinator: lease
  checkpoint: configmap
queue:
  emit_workers: 1
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
`
	cfg, err := LoadBytes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Queue.MaxBatches != 256 {
		t.Fatalf("omitted queue.max_batches should default to 256 (not the runner depth-1 clamp), got %d", cfg.Queue.MaxBatches)
	}
	if cfg.Queue.MaxBatchBytes != 1048576 {
		t.Fatalf("omitted queue.max_batch_bytes should default to 1048576 (not 0 = disabled proactive split), got %d", cfg.Queue.MaxBatchBytes)
	}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("defaulted config should validate, got %v", err)
	}
}

// TestQueueDepthExplicitPreserved (#114): explicitly-set queue depths are preserved (defaulting only
// fires on the zero value).
func TestQueueDepthExplicitPreserved(t *testing.T) {
	setEnv(t)
	const y = `
emit:
  telemetry:
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  deployment_environment: ${ENV}
ha:
  coordinator: lease
  checkpoint: configmap
queue:
  max_batches: 10
  max_batch_bytes: 2048
  emit_workers: 1
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
`
	cfg, err := LoadBytes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Queue.MaxBatches != 10 || cfg.Queue.MaxBatchBytes != 2048 {
		t.Fatalf("explicit queue depths should be preserved, got max_batches=%d max_batch_bytes=%d", cfg.Queue.MaxBatches, cfg.Queue.MaxBatchBytes)
	}
}

// TestValidateRejectsTelemetryMetricInterval (#113): emit.telemetry.metric_interval parses under
// KnownFields (MetricInterval lives on the shared OTLPTarget) but is never read or validated — only
// emit.self.metric_interval is honoured. A non-zero value is a dead knob (the operator misdiagnoses
// their DPM bill), so reject it, matching the "config lies about intent" cross-checks. Unset ⇒ fine.
func TestValidateRejectsTelemetryMetricInterval(t *testing.T) {
	setEnv(t)
	// Accepted by the schema (proves the silent-parse path), then rejected by Validate.
	const y = `
emit:
  telemetry:
    metric_interval: 300s
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  deployment_environment: ${ENV}
ha:
  coordinator: lease
  checkpoint: configmap
queue:
  emit_workers: 1
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
`
	cfg, err := LoadBytes([]byte(y))
	if err != nil {
		t.Fatalf("emit.telemetry.metric_interval should parse under KnownFields: %v", err)
	}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil || !strings.Contains(err.Error(), "emit.telemetry.metric_interval") {
		t.Fatalf("expected non-zero emit.telemetry.metric_interval to be rejected (silently ignored dead knob), got %v", err)
	}
	// Unset (0) ⇒ valid.
	ok, _ := Load("testdata/valid.yaml")
	if err := ok.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("unset emit.telemetry.metric_interval should pass: %v", err)
	}
}
