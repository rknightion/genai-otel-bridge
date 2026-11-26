// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// Tests for unit 6b-2 (#532): tenant assembly reads each tenant's secrets, blocks a tenant with an
// unreadable secret or a duplicate API key, and refuses a tenant row with a loop that emits log records.

// secretTenant is one target of a test tenant: its key and token, as its secret files hold them.
type secretTenant struct {
	tenant, target, key, token string
}

// secretWorld builds tenant rows whose secrets are files read through a mapSecrets, and records what
// assembly does with them: the pipelines it asked telemetry for, the emitters it built, the
// OnTenantBlocked calls and every log record.
type secretWorld struct {
	t       *testing.T
	pk      *assemblyPortkey
	secrets *mapSecrets
	rows    []tenancy.Row
	rec     *specRecorder

	mu        sync.Mutex
	telemetry []string
	blocked   []string
	logs      bytes.Buffer
}

func newSecretWorld(t *testing.T, targets ...secretTenant) *secretWorld {
	t.Helper()
	w := &secretWorld{t: t, pk: newAssemblyPortkey(t), secrets: &mapSecrets{values: map[[2]string]string{}, fail: map[[2]string]bool{}}, rec: &specRecorder{}}
	for i, g := range targets {
		r := tenantRow(w.pk.URL, g.tenant, g.target, "https://stack-"+g.tenant+".example", "10"+string(rune('0'+i)))
		r.Identity = config.IdentityConfig{ServiceNamespace: g.tenant + "-" + g.target, DeploymentEnvironment: "dev"}.ProductIdentity()
		r.AuthValueFile, r.TokenFile = "api-key-"+g.target, "otlp-token"
		r.Source.Auth.Value, r.Destination.Token = "", ""
		w.rows = append(w.rows, r)
		w.secrets.values[[2]string{g.tenant, r.AuthValueFile}] = g.key
		w.secrets.values[[2]string{g.tenant, r.TokenFile}] = g.token
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{mu: &w.mu, w: &w.logs}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return w
}

// unreadable makes one tenant's file fail to read.
func (w *secretWorld) unreadable(tenant, file string) { w.secrets.fail[[2]string{tenant, file}] = true }

func (w *secretWorld) build(opts Options) (*App, error) {
	w.t.Helper()
	tf := func(tenant, pipeline string) PipelineTelemetry {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.telemetry = append(w.telemetry, pipeline)
		return noopTelemetry{}
	}
	opts.Secrets = w.secrets
	if opts.OnTenantBlocked == nil {
		opts.OnTenantBlocked = func(tenant, reason string, blocked bool) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.blocked = append(w.blocked, blockCall(tenant, reason, blocked))
		}
	}
	return BuildFromPlan(context.Background(), minimalConfig(w.pk.URL), tenancy.Plan{Rows: w.rows, TenantShape: true}, assemblyCheckpointer(w.t), coordinate.Noop{}, w.rec.factory, tf, opts)
}

func blockCall(tenant, reason string, blocked bool) string {
	if blocked {
		return tenant + " " + reason + " blocked"
	}
	return tenant + " " + reason + " not blocked"
}

// wantBlocked is the OnTenantBlocked calls for tenants in order, each blocked for the reasons given.
func wantBlocked(tenants []string, reasons map[string][]string) []string {
	var out []string
	for _, tn := range tenants {
		for _, r := range []string{"secret_unreadable", "duplicate_key"} {
			out = append(out, blockCall(tn, r, slices.Contains(reasons[tn], r)))
		}
	}
	return out
}

// builtPipelines is the sorted set of pipelines that got a loop spec.
func builtPipelines(a *App) []string {
	var out []string
	for _, sp := range a.Specs() {
		if !slices.Contains(out, sp.Pipeline.ID) {
			out = append(out, sp.Pipeline.ID)
		}
	}
	slices.Sort(out)
	return out
}

// logRecords is every captured log record.
func (w *secretWorld) logRecords() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(w.logs.Bytes()), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// blockLines is the captured log records with the given reason.
func (w *secretWorld) blockLines(reason string) []map[string]any {
	var out []map[string]any
	for _, rec := range w.logRecords() {
		if rec["reason"] == reason {
			out = append(out, rec)
		}
	}
	return out
}

// requireNoSecretIn fails if text carries any secret value of the world, or the fingerprint (or a
// 12-character prefix of it) or the scope-match digest of any key.
func (w *secretWorld) requireNoSecretIn(what, text string) {
	w.t.Helper()
	for _, v := range w.secrets.values {
		if v == "" {
			continue
		}
		fp := source.KeyFingerprint(v)
		for _, s := range []string{v, fp, fp[:12], source.ScopeMatchDigest(fp, "", "")} {
			if strings.Contains(text, s) {
				w.t.Errorf("%s carries a secret value, a key fingerprint or a digest (%.6s...): %s", what, s, text)
			}
		}
	}
}

func (w *secretWorld) allLogs() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.logs.String()
}

// requireUnbuilt fails if assembly asked telemetry for, or built an emitter for, a pipeline of tenant.
func (w *secretWorld) requireUnbuilt(tenant string) {
	w.t.Helper()
	w.mu.Lock()
	asked := slices.Clone(w.telemetry)
	w.mu.Unlock()
	for _, p := range asked {
		if strings.HasPrefix(p, tenant+".") {
			w.t.Errorf("assembly asked telemetry for %s of blocked tenant %s", p, tenant)
		}
	}
	for _, e := range w.rec.built {
		if e.spec.Tenant == tenant {
			w.t.Errorf("assembly built an emitter for %s of blocked tenant %s", e.spec.Pipeline, tenant)
		}
	}
}

// TestTenantUnreadableSecretBlocksOnlyThatTenant (AC1): a tenant whose API key or OTLP token cannot be
// read, or reads empty, is not built (no telemetry, no emitter, no loop), OnTenantBlocked reports it under
// secret_unreadable and every other tenant 0 under both reasons, one log line names the tenant and the
// file, and every other tenant is built. Nothing logged carries a secret value. Fails closed: on the base
// any read error failed the whole build, and a check that skipped the read would build the tenant.
func TestTenantUnreadableSecretBlocksOnlyThatTenant(t *testing.T) {
	for _, c := range []struct{ name, file, value string }{
		{"OTLP token unreadable", "otlp-token", ""},
		{"API key unreadable", "api-key-primary", ""},
		{"API key empty", "api-key-primary", "empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newSecretWorld(t,
				secretTenant{"tenant-a", "primary", "key-a-value", "token-a-value"},
				secretTenant{"tenant-b", "primary", "key-b-value", "token-b-value"},
				secretTenant{"tenant-c", "primary", "key-c-value", "token-c-value"},
			)
			if c.value == "empty" {
				w.secrets.values[[2]string{"tenant-b", c.file}] = ""
			} else {
				w.unreadable("tenant-b", c.file)
			}
			a, err := w.build(Options{})
			if err != nil {
				t.Fatalf("one tenant's unreadable secret must not fail the build: %v", err)
			}
			if got, want := builtPipelines(a), []string{"tenant-a.primary", "tenant-c.primary"}; !slices.Equal(got, want) {
				t.Errorf("built pipelines %v, want %v", got, want)
			}
			w.requireUnbuilt("tenant-b")
			if got, want := w.blocked, wantBlocked([]string{"tenant-a", "tenant-b", "tenant-c"}, map[string][]string{"tenant-b": {"secret_unreadable"}}); !slices.Equal(got, want) {
				t.Errorf("OnTenantBlocked calls %q, want %q", got, want)
			}
			lines := w.blockLines("secret_unreadable")
			if len(lines) != 1 || lines[0]["tenant"] != "tenant-b" || !strings.Contains(lines[0]["err"].(string), c.file) {
				t.Errorf("want one secret_unreadable line naming tenant-b and %s, got %v", c.file, lines)
			}
			w.requireNoSecretIn("the log", w.allLogs())
		})
	}
}

// TestAllTenantsBlockedFails (AC1): when every tenant is blocked, for either reason or both, the build
// fails, naming every tenant (with its file or its pipelines) and no secret value or fingerprint, builds
// nothing, and still reports every tenant through OnTenantBlocked. On the base the duplicate cases built.
func TestAllTenantsBlockedFails(t *testing.T) {
	cases := []struct {
		name    string
		targets []secretTenant
		seed    func(w *secretWorld)
		reasons map[string][]string
		names   []string
	}{
		{
			name:    "every secret unreadable",
			targets: []secretTenant{{"tenant-a", "primary", "key-a-value", "token-a-value"}, {"tenant-b", "primary", "key-b-value", "token-b-value"}},
			seed: func(w *secretWorld) {
				w.unreadable("tenant-a", "otlp-token")
				w.unreadable("tenant-b", "api-key-primary")
			},
			reasons: map[string][]string{"tenant-a": {"secret_unreadable"}, "tenant-b": {"secret_unreadable"}},
			names:   []string{"tenant-a", "otlp-token", "tenant-b", "api-key-primary"},
		},
		{
			name:    "every tenant shares one key",
			targets: []secretTenant{{"tenant-a", "primary", "shared-key-value", "token-a-value"}, {"tenant-b", "primary", "shared-key-value", "token-b-value"}},
			reasons: map[string][]string{"tenant-a": {"duplicate_key"}, "tenant-b": {"duplicate_key"}},
			names:   []string{"tenant-a", "tenant-b", "tenant-a.primary,tenant-b.primary"},
		},
		{
			name: "one unreadable, the others sharing a key",
			targets: []secretTenant{{"tenant-a", "primary", "key-a-value", "token-a-value"},
				{"tenant-b", "primary", "shared-key-value", "token-b-value"}, {"tenant-c", "primary", "shared-key-value", "token-c-value"}},
			seed:    func(w *secretWorld) { w.unreadable("tenant-a", "otlp-token") },
			reasons: map[string][]string{"tenant-a": {"secret_unreadable"}, "tenant-b": {"duplicate_key"}, "tenant-c": {"duplicate_key"}},
			names:   []string{"tenant-a", "otlp-token", "tenant-b", "tenant-c", "tenant-b.primary,tenant-c.primary"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newSecretWorld(t, c.targets...)
			if c.seed != nil {
				c.seed(w)
			}
			a, err := w.build(Options{})
			if err == nil {
				t.Fatalf("every tenant is blocked, so the build must fail; it built %v", builtPipelines(a))
			}
			msg := err.Error()
			if !strings.Contains(msg, "every tenant is blocked") {
				t.Errorf("error %q must say every tenant is blocked", msg)
			}
			for _, n := range c.names {
				if !strings.Contains(msg, n) {
					t.Errorf("error %q must name %s", msg, n)
				}
			}
			w.requireNoSecretIn("the error", msg)
			w.requireNoSecretIn("the log", w.allLogs())
			if len(w.rec.built) != 0 {
				t.Errorf("%d emitters built, want none", len(w.rec.built))
			}
			var tenants []string
			for _, g := range c.targets {
				tenants = append(tenants, g.tenant)
			}
			if got, want := w.blocked, wantBlocked(tenants, c.reasons); !slices.Equal(got, want) {
				t.Errorf("OnTenantBlocked calls %q, want %q", got, want)
			}
		})
	}
}

// TestDuplicateKeyInOneTenant (AC2): two targets of one tenant whose key files hold one key block that
// tenant (duplicate_key) and build the other; one log line names both pipeline ids and never the key or
// its fingerprint. Fails closed: on the base the tenant was built with one key on two targets.
func TestDuplicateKeyInOneTenant(t *testing.T) {
	w := newSecretWorld(t,
		secretTenant{"tenant-a", "primary", "shared-key-value", "token-a-value"},
		secretTenant{"tenant-a", "secondary", "shared-key-value", "token-a-value"},
		secretTenant{"tenant-b", "primary", "key-b-value", "token-b-value"},
	)
	a, err := w.build(Options{})
	if err != nil {
		t.Fatalf("a duplicate in one tenant must not fail the build: %v", err)
	}
	if got, want := builtPipelines(a), []string{"tenant-b.primary"}; !slices.Equal(got, want) {
		t.Errorf("built pipelines %v, want %v", got, want)
	}
	w.requireUnbuilt("tenant-a")
	if got, want := w.blocked, wantBlocked([]string{"tenant-a", "tenant-b"}, map[string][]string{"tenant-a": {"duplicate_key"}}); !slices.Equal(got, want) {
		t.Errorf("OnTenantBlocked calls %q, want %q", got, want)
	}
	lines := w.blockLines("duplicate_key")
	if len(lines) != 1 || lines[0]["pipelines"] != "tenant-a.primary,tenant-a.secondary" {
		t.Errorf("want one duplicate_key line naming tenant-a.primary,tenant-a.secondary, got %v", lines)
	}
	w.requireNoSecretIn("the log", w.allLogs())
}

// TestDuplicateKeyAcrossTenants (AC2): a key in two tenants blocks both, with every target of each (a
// tenant's other, unique target too); a second shared key is a second set and a second line; a tenant
// blocked for an unreadable token still has its readable key compared, so the tenant sharing that key is
// blocked too; every other tenant is built. Each line names its set's pipeline ids only.
func TestDuplicateKeyAcrossTenants(t *testing.T) {
	w := newSecretWorld(t,
		secretTenant{"tenant-a", "primary", "shared-key-one", "token-a-value"},
		secretTenant{"tenant-a", "secondary", "key-a2-value", "token-a-value"},
		secretTenant{"tenant-b", "primary", "shared-key-one", "token-b-value"},
		secretTenant{"tenant-c", "primary", "shared-key-two", "token-c-value"},
		secretTenant{"tenant-d", "primary", "shared-key-two", "token-d-value"},
		secretTenant{"tenant-e", "primary", "key-e-value", "token-e-value"},
		secretTenant{"tenant-f", "primary", "shared-key-three", "token-f-value"},
		secretTenant{"tenant-g", "primary", "shared-key-three", "token-g-value"},
		secretTenant{"tenant-h", "primary", "key-h-value", "token-h-value"},
	)
	w.unreadable("tenant-f", "otlp-token")
	a, err := w.build(Options{})
	if err != nil {
		t.Fatalf("duplicates in some tenants must not fail the build: %v", err)
	}
	if got, want := builtPipelines(a), []string{"tenant-e.primary", "tenant-h.primary"}; !slices.Equal(got, want) {
		t.Errorf("built pipelines %v, want %v", got, want)
	}
	for _, tn := range []string{"tenant-a", "tenant-b", "tenant-c", "tenant-d", "tenant-f", "tenant-g"} {
		w.requireUnbuilt(tn)
	}
	want := wantBlocked([]string{"tenant-a", "tenant-b", "tenant-c", "tenant-d", "tenant-e", "tenant-f", "tenant-g", "tenant-h"}, map[string][]string{
		"tenant-a": {"duplicate_key"}, "tenant-b": {"duplicate_key"}, "tenant-c": {"duplicate_key"}, "tenant-d": {"duplicate_key"},
		"tenant-f": {"secret_unreadable", "duplicate_key"}, "tenant-g": {"duplicate_key"},
	})
	if !slices.Equal(w.blocked, want) {
		t.Errorf("OnTenantBlocked calls %q, want %q", w.blocked, want)
	}
	var sets []string
	for _, rec := range w.blockLines("duplicate_key") {
		sets = append(sets, rec["pipelines"].(string))
		for k := range rec {
			if !slices.Contains([]string{"time", "level", "msg", "reason", "pipelines"}, k) {
				t.Errorf("a duplicate_key line carries %q; it names the pipeline ids only", k)
			}
		}
	}
	if want := []string{"tenant-a.primary,tenant-b.primary", "tenant-c.primary,tenant-d.primary", "tenant-f.primary,tenant-g.primary"}; !slices.Equal(sets, want) {
		t.Errorf("duplicate_key lines name %q, want %q", sets, want)
	}
	w.requireNoSecretIn("the log", w.allLogs())
}

// TestLegacyRowsAreNotCheckedForSecretsOrDuplicates: legacy rows (no tenant) sharing one inline key build
// as before, and OnTenantBlocked is never called (design §5.5: a legacy install is not checked; G6).
func TestLegacyRowsAreNotCheckedForSecretsOrDuplicates(t *testing.T) {
	pk := newAssemblyPortkey(t)
	rows := []tenancy.Row{
		assemblyRow(pk.URL, "pk-one", "https://otlp.example", "1", "genai-otel-bridge"),
		assemblyRow(pk.URL, "pk-two", "https://otlp.example", "1", "genai-otel-bridge"),
	}
	rows[1].Source.Loops["analytics"] = config.LoopConfig{Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
		BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute), MaxBackfill: config.Duration(55 * time.Minute),
		MetricPrefix: "portkey_other", Graphs: []string{"requests"}}
	if rows[0].Source.Auth.Value != rows[1].Source.Auth.Value {
		t.Fatal("test setup: the two legacy rows must share one key")
	}
	called := false
	a, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: rows}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory,
		Options{OnTenantBlocked: func(string, string, bool) { called = true }})
	if err != nil {
		t.Fatalf("legacy rows sharing a key must build: %v", err)
	}
	if len(a.Specs()) != 2 || called {
		t.Errorf("specs %d (want 2), OnTenantBlocked called %v (want never)", len(a.Specs()), called)
	}
}

// TestTenantLogsLoopRefused (AC3): a tenant-shape row (one with a pipeline id) whose source builds a loop
// that emits log records (source.IndexedKeyDeclarer, here Portkey's logs_export) fails assembly, naming the
// pipeline and the loop; the same source on a legacy row builds. On the base the tenant row built.
func TestTenantLogsLoopRefused(t *testing.T) {
	pk := newAssemblyPortkey(t)
	logsSource := logsExportConfig(pk.URL, "127.0.0.1").Sources[0]
	build := func(row tenancy.Row) error {
		_, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: []tenancy.Row{row}}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory, Options{})
		return err
	}
	legacy := assemblyRow(pk.URL, "pk-logs", "https://otlp.example", "1", "genai-otel-bridge")
	legacy.Source.Loops = logsSource.Loops
	if err := build(legacy); err != nil {
		t.Fatalf("a legacy row with a logs loop must build: %v", err)
	}
	row := tenantRow(pk.URL, "tenant-a", "primary", "https://stack-a.example", "100")
	row.Source.Loops = logsSource.Loops
	err := build(row)
	if err == nil {
		t.Fatal("a tenant row with a loop that emits log records must fail assembly")
	}
	for _, s := range []string{"tenant-a.primary", "logs_export", "emits log records"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q must name %q", err, s)
		}
	}
}

// TestTenantBlockedGauge (AC4): with main's wiring (OnTenantBlocked is selfobs.Metrics.TenantBlocked),
// tenant_blocked is 1 for a blocked tenant under the reason that blocks it and 0 under the other, 0 under
// both reasons for every built tenant, with exactly the labels tenant and reason.
func TestTenantBlockedGauge(t *testing.T) {
	const name = "genai_otel_bridge_tenant_blocked"
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := selfobs.NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	w := newSecretWorld(t,
		secretTenant{"tenant-a", "primary", "key-a-value", "token-a-value"},
		secretTenant{"tenant-b", "primary", "shared-key-value", "token-b-value"},
		secretTenant{"tenant-c", "primary", "shared-key-value", "token-c-value"},
		secretTenant{"tenant-d", "primary", "key-d-value", "token-d-value"},
	)
	w.unreadable("tenant-a", "otlp-token")
	if _, err := w.build(Options{OnTenantBlocked: m.TenantBlocked}); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != name {
				continue
			}
			for _, dp := range mm.Data.(metricdata.Gauge[float64]).DataPoints {
				var keys []string
				for _, kv := range dp.Attributes.ToSlice() {
					keys = append(keys, string(kv.Key))
				}
				if !slices.Equal(keys, []string{"reason", "tenant"}) {
					t.Errorf("label keys %v, want [reason tenant]", keys)
				}
				got[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
			}
		}
	}
	key := func(tn, r string) string { return "reason=" + r + ",tenant=" + tn }
	want := map[string]float64{
		key("tenant-a", "secret_unreadable"): 1, key("tenant-a", "duplicate_key"): 0,
		key("tenant-b", "secret_unreadable"): 0, key("tenant-b", "duplicate_key"): 1,
		key("tenant-c", "secret_unreadable"): 0, key("tenant-c", "duplicate_key"): 1,
		key("tenant-d", "secret_unreadable"): 0, key("tenant-d", "duplicate_key"): 0,
	}
	if !maps.Equal(got, want) {
		t.Errorf("tenant_blocked = %v, want %v", got, want)
	}
}
