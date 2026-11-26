// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/app"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// TestGracefulShutdownOrderAndBounding pins the [#129] fixes: signal reset runs FIRST, the self-metrics
// flush runs BEFORE the pprof drain, and a hung step does not block overall shutdown past its deadline.
func TestGracefulShutdownOrderAndBounding(t *testing.T) {
	orig := shutdownStepTimeout
	shutdownStepTimeout = 50 * time.Millisecond
	defer func() { shutdownStepTimeout = orig }()

	var mu sync.Mutex
	var order []string
	rec := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	stop := func() { rec("stop") }
	mp := func(context.Context) error { rec("metrics"); return nil }
	tp := func(context.Context) error { rec("tracing"); return nil }
	// profStop hangs until its ctx deadline — proves the step is bounded AND that it runs after metrics.
	prof := func(ctx context.Context) error { rec("profiling"); <-ctx.Done(); return ctx.Err() }

	start := time.Now()
	gracefulShutdown(stop, mp, tp, prof)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("gracefulShutdown blocked on a hung step for %v — not bounded", elapsed)
	}

	if len(order) == 0 || order[0] != "stop" {
		t.Fatalf("signal reset must run first, got %v", order)
	}
	if slices.Index(order, "metrics") > slices.Index(order, "profiling") {
		t.Fatalf("self-metrics flush must precede the pprof drain, got %v", order)
	}
}

// TestLivenessThreshold pins the /healthz staleness derivation [CP-C5] and, critically, that only
// ENABLED sources/loops contribute [#88]: a disabled/parked slow loop must NOT inflate the threshold
// (else a wedged leader would pass liveness for hours). threshold == max(DegradedBackoff, slowest
// ENABLED cadence) + emitRetryBudget(2m) + livenessMargin(4m).
// TestSelfProviderRejectsCleartextCredentialsBeforeAnyExport pins that the cleartext-credential gate runs
// before the self-metrics PeriodicReader exists: NewMetrics records values at once, so a provider built
// from an http:// endpoint with credentials would send the Basic Authorization header on its first
// export (or at shutdown flush) even though startup is about to fail.
func TestSelfProviderRejectsCleartextCredentialsBeforeAnyExport(t *testing.T) {
	var hits atomic.Int32
	// Loopback endpoints are exempt from the gate by design, so the fake must be reached on a
	// non-loopback address of this host.
	var host string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			host = ipn.IP.String()
			break
		}
	}
	if host == "" {
		t.Skip("no non-loopback IPv4 address on this host")
	}
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	srvURL := "http://" + net.JoinHostPort(host, port)
	cfg, err := config.LoadBytes([]byte(fmt.Sprintf(`
emit:
  telemetry:
    otlp: {endpoint: "%[1]s", instance_id: "123", token: "secret"}
  self:
    otlp: {endpoint: "%[1]s", instance_id: "123", token: "secret"}
    metric_interval: 60s
identity: {service_namespace: genai-otel-bridge, deployment_environment: dev}
ha: {coordinator: none, checkpoint: file}
queue: {max_batches: 256, max_batch_bytes: 1048576, emit_workers: 1}
sources: []
`, srvURL)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mp, shutdown, err := newSelfProvider(ctx, cfg, "pod-a")
	if shutdown != nil {
		if mp != nil {
			if _, merr := selfobs.NewMetrics(mp); merr != nil {
				t.Fatal(merr)
			}
		}
		_ = shutdown(ctx) // flushes whatever the provider holds
	}
	if err != nil && !strings.Contains(err.Error(), "https://") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
	if err == nil {
		t.Fatal("a cleartext endpoint with credentials must be rejected before a provider is built")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("endpoint received %d requests before the config was rejected", n)
	}
}

func TestLivenessThreshold(t *testing.T) {
	const budget = 6 * time.Minute // emitRetryBudget(2m) + livenessMargin(4m)
	dur := func(d time.Duration) config.Duration { return config.Duration(d) }
	loop := func(enabled bool, cadence time.Duration) config.LoopConfig {
		return config.LoopConfig{Enabled: enabled, Cadence: dur(cadence)}
	}

	cases := []struct {
		name string
		cfg  *config.Config
		want time.Duration
	}{
		{
			// Only fast ENABLED loops (< DegradedBackoff) ⇒ base floors at DegradedBackoff.
			name: "fast enabled loops floor at DegradedBackoff",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: true, Loops: map[string]config.LoopConfig{"analytics": loop(true, time.Minute)}},
			}},
			want: schedule.DegradedBackoff + budget,
		},
		{
			// [#88] A DISABLED slow loop (6h) alongside an enabled fast loop must be ignored — the
			// threshold stays at the DegradedBackoff floor, NOT 6h+6m.
			name: "disabled slow loop does not inflate threshold",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: true, Loops: map[string]config.LoopConfig{
					"analytics": loop(true, time.Minute),
					"eval":      loop(false, 6*time.Hour),
				}},
			}},
			want: schedule.DegradedBackoff + budget,
		},
		{
			// An ENABLED slow loop (> DegradedBackoff) DOES raise the base.
			name: "enabled slow loop raises base",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: true, Loops: map[string]config.LoopConfig{"eval": loop(true, 30*time.Minute)}},
			}},
			want: 30*time.Minute + budget,
		},
		{
			// A DISABLED SOURCE excludes its (even enabled) loops entirely.
			name: "disabled source excludes its loops",
			cfg: &config.Config{Sources: []config.SourceConfig{
				{Enabled: false, Loops: map[string]config.LoopConfig{"eval": loop(true, 6*time.Hour)}},
				{Enabled: true, Loops: map[string]config.LoopConfig{"analytics": loop(true, time.Minute)}},
			}},
			want: schedule.DegradedBackoff + budget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// [#522] The threshold reads the resolved plan, so each config case goes through Resolve.
			plan, err := tenancy.Resolve(tc.cfg)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := livenessThreshold(plan); got != tc.want {
				t.Errorf("livenessThreshold = %v, want %v", got, tc.want)
			}
		})
	}
}

// validateCfgLiteral is a complete legacy config with literal values only, so the config check sets
// no placeholder environment variable in this test binary.
const validateCfgLiteral = `
emit:
  telemetry:
    otlp:
      endpoint: https://otlp.example/otlp
      instance_id: "100"
      token: sentinel-token-not-printed
identity:
  service_namespace: genai-otel-bridge
  deployment_environment: test
ha:
  coordinator: lease
  checkpoint: configmap
queue:
  max_batches: 256
  max_batch_bytes: 1048576
  emit_workers: 1
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-validate-test
    auth: { header: x-portkey-api-key, value: sentinel-key-not-printed }
    rate_limit: { rps: 1, burst: 3 }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
        bucket_settle: 10m
        bootstrap_lookback: 50m
        max_backfill: 90m
        metric_prefix: portkey_api
        graphs: [requests, cost, tokens, latency, errors]
`

// [#522] -validate-config prints the plan table, then "validate-config: OK (<path>) plan <hash>",
// and exits 0; a bad config prints FAIL to stderr, nothing to stdout, and exits 1.
func TestRunValidateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte(validateCfgLiteral), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := runValidateConfig(path, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout has %d lines, want table header, one row and the OK line:\n%s", len(lines), stdout.String())
	}
	if !strings.HasPrefix(lines[0], "tenant") || !strings.Contains(lines[1], "portkey-validate-test") {
		t.Errorf("stdout does not start with the plan table:\n%s", stdout.String())
	}
	ok := regexp.MustCompile("^validate-config: OK \\(" + regexp.QuoteMeta(path) + "\\) plan [0-9a-f]{64}$")
	if !ok.MatchString(lines[2]) {
		t.Errorf("last line = %q, want validate-config: OK (<path>) plan <hash>", lines[2])
	}
	for _, s := range []string{"sentinel-token-not-printed", "sentinel-key-not-printed"} {
		if strings.Contains(stdout.String(), s) {
			t.Errorf("stdout prints the secret %q", s)
		}
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte(strings.Replace(validateCfgLiteral, "emit_workers: 1", "emit_workers: 2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runValidateConfig(bad, &stdout, &stderr); code != 1 {
		t.Fatalf("bad config: exit %d, want 1", code)
	}
	if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "validate-config: FAIL: ") {
		t.Errorf("bad config: stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

// AC5 (#530): main refuses a tenant-shape plan right after app.Prepare. The fixture validates, so the
// refusal, not a load rule, is what stops it; a legacy plan passes.
func TestTenantShapeRefused(t *testing.T) {
	t.Setenv("TENANT_FIXTURE_SELF_TOKEN", "self-token-example")
	cfg, err := config.Load("../../internal/config/testdata/tenants.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan, err := app.Prepare(cfg)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := refuseTenantShape(plan); !errors.Is(err, tenancy.ErrTenantShapeNotEnabled) {
		t.Fatalf("refuseTenantShape = %v, want ErrTenantShapeNotEnabled", err)
	}
	if err := refuseTenantShape(tenancy.Plan{Rows: []tenancy.Row{{}}}); err != nil {
		t.Fatalf("a legacy plan is refused: %v", err)
	}
}

// TestSelfOTLPTenantShapeNeverFallsBack (#532): a legacy config with no emit.self sends self-telemetry to
// emit.telemetry, as before; in the tenant shape self-telemetry goes to emit.self only, and with no
// emit.self it is an error, never a fallback to emit.telemetry (design §2).
func TestSelfOTLPTenantShapeNeverFallsBack(t *testing.T) {
	telemetry := config.OTLPConn{Endpoint: "https://telemetry.example", InstanceID: "1", Token: "t"}
	self := config.OTLPConn{Endpoint: "https://self.example", InstanceID: "2", Token: "s"}
	tenants := map[string]config.TenantConfig{"team-a": {}}

	legacy := &config.Config{Emit: config.EmitConfig{Telemetry: config.OTLPTarget{OTLP: telemetry}}}
	if got, _, err := selfOTLP(legacy); err != nil || got != telemetry {
		t.Errorf("legacy, no emit.self: got %+v, %v; want emit.telemetry", got, err)
	}
	withSelf := &config.Config{Tenants: tenants, Emit: config.EmitConfig{Self: &config.OTLPTarget{OTLP: self, MetricInterval: config.Duration(2 * time.Minute)}}}
	if got, iv, err := selfOTLP(withSelf); err != nil || got != self || iv != 2*time.Minute {
		t.Errorf("tenant shape with emit.self: got %+v, %s, %v; want emit.self and its interval", got, iv, err)
	}
	noSelf := &config.Config{Tenants: tenants, Emit: config.EmitConfig{Telemetry: config.OTLPTarget{OTLP: telemetry}}}
	if got, _, err := selfOTLP(noSelf); err == nil || got == telemetry {
		t.Errorf("tenant shape with no emit.self: got %+v, %v; want an error and no fallback", got, err)
	}
}

// TestTenantSecretsRoot (#532): the tenant secrets root is the path the chart mounts each tenant's Secret
// under; it is a contract with deploy/helm, so it is pinned as written.
func TestTenantSecretsRoot(t *testing.T) {
	if tenantSecretsRoot != "/var/run/secrets/genai-otel-bridge/tenants" {
		t.Fatalf("tenantSecretsRoot = %q", tenantSecretsRoot)
	}
}
