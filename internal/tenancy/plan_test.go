// SPDX-License-Identifier: Apache-2.0

package tenancy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// Distinctive values that must never reach the plan table (or, through it, the plan hash input,
// CI logs or the config check's output).
const (
	sentinelAuthValue = "sentinel-auth-value-must-not-print"
	sentinelToken     = "sentinel-otlp-token-must-not-print"
	sentinelUserinfo  = "sentinel-userinfo-password-must-not-print"
	sentinelQuery     = "sentinel-query-secret-must-not-print"
)

func loops(enabled map[string]bool) map[string]config.LoopConfig {
	out := make(map[string]config.LoopConfig, len(enabled))
	for name, on := range enabled {
		out[name] = config.LoopConfig{Enabled: on}
	}
	return out
}

// legacyConfig has three sources whose source_instance values do NOT sort in config order, the
// middle one disabled, so a resolver that sorted rows or kept disabled sources is caught.
func legacyConfig() *config.Config {
	return &config.Config{
		Emit: config.EmitConfig{Telemetry: config.OTLPTarget{OTLP: config.OTLPConn{
			Endpoint: "https://otlp.example/otlp", InstanceID: "100", Token: sentinelToken,
		}}},
		Identity: config.IdentityConfig{ServiceNamespace: "ns-example", DeploymentEnvironment: "test"},
		Sources: []config.SourceConfig{
			{
				Type: "portkey", Enabled: true, BaseURL: "https://vendor-a.example/v1", SourceInstance: "zeta-a",
				Auth:  config.AuthConfig{Header: "x-api-key", Value: sentinelAuthValue},
				Loops: loops(map[string]bool{"analytics": true, "logs_export": false}),
			},
			{
				Type: "portkey", Enabled: false, BaseURL: "https://vendor-b.example/v1", SourceInstance: "mid-b",
				Loops: loops(map[string]bool{"analytics": true}),
			},
			{
				Type: "langsmith", Enabled: true, BaseURL: "https://vendor-c.example", SourceInstance: "alpha-c",
				Loops: loops(map[string]bool{"runs": true, "feedback": true}),
			},
		},
	}
}

// AC1 (#522): the legacy shape resolves to one row per ENABLED source in config order; a disabled
// source gives no row; each row carries the source entry unchanged (SourceInstance untouched), the
// emit.telemetry destination and the product identity, with tenant, target and pipeline empty.
func TestResolveLegacyRowsInConfigOrder(t *testing.T) {
	cfg := legacyConfig()
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.TenantShape {
		t.Fatal("a legacy config resolved with TenantShape=true")
	}
	var got []string
	for _, r := range plan.Rows {
		got = append(got, r.Source.SourceInstance)
	}
	if want := []string{"zeta-a", "alpha-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("row source instances = %v, want %v (config order, disabled source dropped)", got, want)
	}
	for i, r := range plan.Rows {
		want := cfg.Sources[[]int{0, 2}[i]]
		if !reflect.DeepEqual(r.Source, want) {
			t.Errorf("row %d Source = %+v, want the config entry unchanged %+v", i, r.Source, want)
		}
		if !r.Source.Enabled {
			t.Errorf("row %d Source.Enabled = false", i)
		}
		if r.Destination != cfg.Emit.Telemetry.OTLP {
			t.Errorf("row %d Destination = %+v, want emit.telemetry.otlp", i, r.Destination)
		}
		if !reflect.DeepEqual(r.Identity, cfg.Identity.ProductIdentity()) {
			t.Errorf("row %d Identity = %v, want %v", i, r.Identity, cfg.Identity.ProductIdentity())
		}
		if r.Tenant != "" || r.Target != "" || r.Pipeline != "" || r.DestinationName != "" || r.AuthValueFile != "" || r.TokenFile != "" {
			t.Errorf("row %d has tenant-shape fields set: %+v", i, r)
		}
	}
	// Each row owns its identity map: changing one must not change another or the next resolve.
	plan.Rows[0].Identity["service.namespace"] = "changed"
	if plan.Rows[1].Identity["service.namespace"] != "ns-example" {
		t.Error("rows share one identity map")
	}
}

func TestResolveNoEnabledSourcesGivesNoRows(t *testing.T) {
	cfg := legacyConfig()
	for i := range cfg.Sources {
		cfg.Sources[i].Enabled = false
	}
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(plan.Rows))
	}
}

func TestResolveNilConfig(t *testing.T) {
	if _, err := tenancy.Resolve(nil); err == nil {
		t.Fatal("Resolve(nil) returned no error")
	}
}

// AC2 (#522): the plan table never prints a source's API key or a destination's token, nor a secret
// carried in an endpoint's userinfo or query.
func TestTableNeverPrintsSecrets(t *testing.T) {
	cfg := legacyConfig()
	cfg.Emit.Telemetry.OTLP.Endpoint = "https://user:" + sentinelUserinfo + "@otlp.example/otlp?k=" + sentinelQuery
	cfg.Sources[2].Auth = config.AuthConfig{Header: "x-api-key", Value: sentinelAuthValue}
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Tenant-shape rows carry their secrets as file names only; the values never enter a Row, but a
	// row built by hand with one in the value fields must still not print it.
	plan.Rows = append(plan.Rows, tenancy.Row{
		Tenant: "t1", Target: "x", Pipeline: tenancy.PipelineID("t1", "x"), DestinationName: "d",
		Source: config.SourceConfig{
			Type: "portkey", Enabled: true, SourceInstance: tenancy.SourceInstance("t1", "x"),
			Auth: config.AuthConfig{Header: "x-api-key", Value: sentinelAuthValue},
		},
		Destination: config.OTLPConn{Endpoint: "https://otlp.example", InstanceID: "1", Token: sentinelToken},
	})
	table := plan.Table()
	for _, s := range []string{sentinelAuthValue, sentinelToken, sentinelUserinfo, sentinelQuery} {
		if strings.Contains(table, s) {
			t.Errorf("plan table prints %q:\n%s", s, table)
		}
	}
	// The table still says where data goes, so a check that printed nothing would also fail.
	for _, s := range []string{"zeta-a", "alpha-c", "otlp.example/otlp", "t1.x"} {
		if !strings.Contains(table, s) {
			t.Errorf("plan table lacks %q:\n%s", s, table)
		}
	}
}

// The columns of design §3, the legacy row's source_instance in the pipeline-id column, and only
// enabled loops listed, sorted.
func TestTableColumnsAndLegacyPipelineColumn(t *testing.T) {
	plan, err := tenancy.Resolve(legacyConfig())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(plan.Table(), "\n"), "\n")
	if len(lines) != 1+len(plan.Rows) {
		t.Fatalf("table has %d lines, want header + %d rows:\n%s", len(lines), len(plan.Rows), plan.Table())
	}
	wantHeader := []string{"tenant", "target", "source_type", "source_url", "scope_name", "scope_id", "pipeline",
		"destination", "endpoint", "instance_id", "identity", "loops"}
	if got := strings.Fields(lines[0]); !reflect.DeepEqual(got, wantHeader) {
		t.Fatalf("header = %v, want %v", got, wantHeader)
	}
	first := strings.Fields(lines[1])
	if len(first) != len(wantHeader) {
		t.Fatalf("row has %d cells, want %d: %q", len(first), len(wantHeader), lines[1])
	}
	col := func(name string) string {
		for i, h := range wantHeader {
			if h == name {
				return first[i]
			}
		}
		t.Fatalf("no column %s", name)
		return ""
	}
	if got := col("pipeline"); got != "zeta-a" {
		t.Errorf("legacy pipeline column = %q, want the source_instance", got)
	}
	if got := col("tenant"); got != "-" {
		t.Errorf("legacy tenant column = %q, want -", got)
	}
	if got := col("loops"); got != "analytics" {
		t.Errorf("loops column = %q, want only the enabled loop", got)
	}
	if got := col("identity"); got != "deployment.environment.name=test,service.name=genai-otel-bridge,service.namespace=ns-example" {
		t.Errorf("identity column = %q", got)
	}
	if got := col("endpoint"); got != "https://otlp.example/otlp" {
		t.Errorf("endpoint column = %q", got)
	}
	if got := strings.Fields(lines[2])[len(wantHeader)-1]; got != "feedback,runs" {
		t.Errorf("second row loops = %q, want sorted enabled loops", got)
	}
}

// A value with white space or a control character is quoted, so every row stays one line with one
// cell per column and two different plans cannot render the same text.
func TestTableQuotesAwkwardValues(t *testing.T) {
	cfg := legacyConfig()
	cfg.Identity.DeploymentEnvironment = "a b\nc"
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(plan.Table(), "\n"), "\n")
	if len(lines) != 1+len(plan.Rows) {
		t.Fatalf("a newline in a value split a row:\n%s", plan.Table())
	}
	if !strings.Contains(lines[1], "\"deployment.environment.name=a b\\nc,") {
		t.Errorf("value not quoted: %q", lines[1])
	}
}

var hexSHA256 = regexp.MustCompile("^[0-9a-f]{64}$")

// AC3 (#522): the plan hash is the lower-case hex SHA-256 of the table, the same for the same
// config, and different when a destination endpoint changes.
func TestHashChangesWithEndpoint(t *testing.T) {
	a, err := tenancy.Resolve(legacyConfig())
	if err != nil {
		t.Fatal(err)
	}
	again, err := tenancy.Resolve(legacyConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := legacyConfig()
	cfg.Emit.Telemetry.OTLP.Endpoint = "https://otlp-other.example/otlp"
	b, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !hexSHA256.MatchString(a.Hash()) {
		t.Fatalf("hash %q is not 64 lower-case hex characters", a.Hash())
	}
	sum := sha256.Sum256([]byte(a.Table()))
	if a.Hash() != hex.EncodeToString(sum[:]) {
		t.Fatal("Hash is not the SHA-256 of Table")
	}
	if a.Hash() != again.Hash() {
		t.Fatal("the same config hashed differently")
	}
	if a.Hash() == b.Hash() {
		t.Fatal("changing the destination endpoint did not change the plan hash")
	}
}

func TestStackKey(t *testing.T) {
	base := tenancy.StackKey("https://otlp.example/otlp", "100")
	for _, ep := range []string{"https://OTLP.Example/otlp", "https://otlp.example/otlp/", "HTTPS://otlp.EXAMPLE/otlp/"} {
		if got := tenancy.StackKey(ep, "100"); got != base {
			t.Errorf("StackKey(%q) = %q, want %q (same stack)", ep, got, base)
		}
	}
	for name, got := range map[string]string{
		"other instance id": tenancy.StackKey("https://otlp.example/otlp", "101"),
		"other host":        tenancy.StackKey("https://otlp2.example/otlp", "100"),
		"other path":        tenancy.StackKey("https://otlp.example/otlp2", "100"),
		"path case":         tenancy.StackKey("https://otlp.example/OTLP", "100"),
	} {
		if got == base {
			t.Errorf("%s: StackKey = %q equals the base stack", name, got)
		}
	}
}

func TestPipelineNames(t *testing.T) {
	if got := tenancy.PipelineID("t1", "main"); got != "t1.main" {
		t.Errorf("PipelineID = %q", got)
	}
	if got := tenancy.SourceInstance("t1", "main"); got != "mt.t1.main" {
		t.Errorf("SourceInstance = %q", got)
	}
	if tenancy.PipelineAttribute != "genai_otel_bridge.pipeline" {
		t.Errorf("PipelineAttribute = %q", tenancy.PipelineAttribute)
	}
}

// Unit 6a (#530): a config with at least one tenant resolves to the tenant shape (this tenant has no
// targets, so no rows); an empty tenants map is the legacy shape.
func TestResolveTenantShape(t *testing.T) {
	cfg := legacyConfig()
	cfg.Sources = nil
	cfg.Tenants = map[string]config.TenantConfig{"team-a": {}}
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !plan.TenantShape || len(plan.Rows) != 0 {
		t.Fatalf("plan = %+v, want TenantShape and no rows", plan)
	}
	legacy := legacyConfig()
	legacy.Tenants = map[string]config.TenantConfig{}
	plan, err = tenancy.Resolve(legacy)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.TenantShape || len(plan.Rows) == 0 {
		t.Fatalf("an empty tenants map: TenantShape %v with %d rows, want the legacy shape", plan.TenantShape, len(plan.Rows))
	}
}

// The two scope columns (unit 4b, #526): a row with a scope shows its name and id; a row without one
// (every legacy row) shows "-" in both, so a legacy plan's table and hash do not change.
func TestTableShowsScope(t *testing.T) {
	plan, err := tenancy.Resolve(legacyConfig())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	plan.Rows[0].Source.Scope = config.ScopeConfig{Name: "ws-example-001", ID: "ws-id-example"}
	lines := strings.Split(strings.TrimSuffix(plan.Table(), "\n"), "\n")
	header := strings.Fields(lines[0])
	cellOf := func(line, name string) string {
		t.Helper()
		cells := strings.Fields(line)
		for i, h := range header {
			if h == name && i < len(cells) {
				return cells[i]
			}
		}
		t.Fatalf("no column %s in %q", name, line)
		return ""
	}
	if got := cellOf(lines[1], "scope_name"); got != "ws-example-001" {
		t.Errorf("scope_name = %q, want the row's scope name", got)
	}
	if got := cellOf(lines[1], "scope_id"); got != "ws-id-example" {
		t.Errorf("scope_id = %q, want the row's scope id", got)
	}
	if got, gotID := cellOf(lines[2], "scope_name"), cellOf(lines[2], "scope_id"); got != "-" || gotID != "-" {
		t.Errorf("a row with no scope shows (%q, %q), want (-, -)", got, gotID)
	}
}

// tenantConfig is a tenant-shape config that passes every resolve rule. team-a has two destinations
// and three targets: prod and staging share the main stack with different identities (staging has its
// own block), and mirror sends prod's scope to a second stack (one tenant may name one scope twice).
// The tenants are declared out of id order; Go map order is random anyway.
func tenantConfig() *config.Config {
	return &config.Config{
		Identity: config.IdentityConfig{ServiceNamespace: "bridge-example", DeploymentEnvironment: "test"},
		Emit: config.EmitConfig{Self: &config.OTLPTarget{OTLP: config.OTLPConn{
			Endpoint: "https://otlp-self.example/otlp", InstanceID: "900", Token: sentinelToken,
		}}},
		Defaults: map[string]config.DefaultsConfig{"portkey": {Loops: map[string]config.LoopConfig{
			"analytics": {Enabled: true, Graphs: []string{"requests", "cost"}, Settings: map[string]string{"knob": "v1"}},
			"groups":    {Enabled: true},
			"spare":     {Enabled: false},
		}}},
		Tenants: map[string]config.TenantConfig{
			"team-b": {
				Identity: config.IdentityConfig{ServiceNamespace: "team-b-llm", DeploymentEnvironment: "prod"},
				Destinations: map[string]config.TenantDestinationConfig{
					"main": {OTLP: config.TenantOTLPConfig{Endpoint: "https://otlp-b.example/otlp", InstanceID: "201", TokenFile: "b-token"}},
				},
				Targets: map[string]config.TargetConfig{
					"prod": target("b-key", "scope-b1", "scope-id-b1", "main"),
				},
			},
			"team-a": {
				Identity: config.IdentityConfig{ServiceNamespace: "team-a-llm", DeploymentEnvironment: "prod"},
				Destinations: map[string]config.TenantDestinationConfig{
					"main": {OTLP: config.TenantOTLPConfig{Endpoint: "https://otlp-a.example/otlp", InstanceID: "200", TokenFile: "a-token"}},
					"alt":  {OTLP: config.TenantOTLPConfig{Endpoint: "https://otlp-a2.example/otlp", InstanceID: "202", TokenFile: "a-token-2"}},
				},
				Targets: map[string]config.TargetConfig{
					"prod": target("a-prod-key", "scope-a1", "scope-id-a1", "main"),
					"staging": func() config.TargetConfig {
						tg := target("a-staging-key", "scope-a2", "scope-id-a2", "main")
						tg.Identity = &config.IdentityConfig{ServiceNamespace: "team-a-llm", DeploymentEnvironment: "staging"}
						return tg
					}(),
					"mirror": target("a-mirror-key", "scope-a1", "scope-id-a1", "alt"),
				},
			},
		},
	}
}

func target(keyFile, scopeName, scopeID, dest string) config.TargetConfig {
	return config.TargetConfig{
		Source: config.TenantSourceConfig{
			Type: "portkey", BaseURL: "https://vendor.example/v1",
			Auth:      config.TenantAuthConfig{Header: "x-api-key", ValueFile: keyFile},
			RateLimit: config.RateLimitConfig{RPS: 1, Burst: 3},
			HTTP:      config.HTTPConfig{UserAgent: "ua-example", AllowHosts: []string{"vendor.example"}},
		},
		Scope:       config.ScopeConfig{Name: scopeName, ID: scopeID},
		Destination: dest,
	}
}

// editTarget changes one target of one tenant in place (map values are structs).
func editTarget(cfg *config.Config, tenant, name string, f func(*config.TargetConfig)) {
	tc := cfg.Tenants[tenant]
	tg := tc.Targets[name]
	f(&tg)
	tc.Targets[name] = tg
}

// editDestination changes one destination of one tenant in place.
func editDestination(cfg *config.Config, tenant, name string, f func(*config.TenantOTLPConfig)) {
	tc := cfg.Tenants[tenant]
	d := tc.Destinations[name]
	f(&d.OTLP)
	tc.Destinations[name] = d
}

func mustResolve(t *testing.T, cfg *config.Config) tenancy.Plan {
	t.Helper()
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return plan
}

// wantResolveError resolves cfg and requires an error holding every fragment, and an empty plan.
func wantResolveError(t *testing.T, cfg *config.Config, fragments ...string) {
	t.Helper()
	plan, err := tenancy.Resolve(cfg)
	if err == nil {
		t.Fatalf("Resolve succeeded with %d rows, want an error holding %q", len(plan.Rows), fragments)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not hold %q", err, f)
		}
	}
	if len(plan.Rows) != 0 {
		t.Errorf("a failed resolve returned %d rows", len(plan.Rows))
	}
}

// Unit 6b-1 (#531) step 1: one row per target, sorted by tenant then target, each with the scope, the
// loops of defaults.<type>, the destination without a token, the identity (the target's block whole,
// else the tenant's), the two secret FILE NAMES and the pipeline id. Red on base (no rows).
func TestResolveTenantRows(t *testing.T) {
	cfg := tenantConfig()
	plan := mustResolve(t, cfg)
	if !plan.TenantShape {
		t.Fatal("TenantShape = false")
	}
	identity := func(ns, env string) map[string]string {
		return config.IdentityConfig{ServiceNamespace: ns, DeploymentEnvironment: env}.ProductIdentity()
	}
	row := func(tenant, tgt, dest, endpoint, instance, tokenFile, keyFile, scopeName, scopeID string, id map[string]string) tenancy.Row {
		return tenancy.Row{
			Tenant: tenant, Target: tgt, Pipeline: tenant + "." + tgt, DestinationName: dest,
			AuthValueFile: keyFile, TokenFile: tokenFile,
			Source: config.SourceConfig{
				Type: "portkey", Enabled: true, BaseURL: "https://vendor.example/v1",
				SourceInstance: "mt." + tenant + "." + tgt,
				Auth:           config.AuthConfig{Header: "x-api-key"},
				RateLimit:      config.RateLimitConfig{RPS: 1, Burst: 3},
				HTTP:           config.HTTPConfig{UserAgent: "ua-example", AllowHosts: []string{"vendor.example"}},
				Loops: map[string]config.LoopConfig{
					"analytics": {Enabled: true, Graphs: []string{"requests", "cost"}, Settings: map[string]string{"knob": "v1"}},
					"groups":    {Enabled: true},
					"spare":     {Enabled: false},
				},
				Scope: config.ScopeConfig{Name: scopeName, ID: scopeID},
			},
			Destination: config.OTLPConn{Endpoint: endpoint, InstanceID: instance},
			Identity:    id,
		}
	}
	want := []tenancy.Row{
		row("team-a", "mirror", "alt", "https://otlp-a2.example/otlp", "202", "a-token-2", "a-mirror-key", "scope-a1", "scope-id-a1", identity("team-a-llm", "prod")),
		row("team-a", "prod", "main", "https://otlp-a.example/otlp", "200", "a-token", "a-prod-key", "scope-a1", "scope-id-a1", identity("team-a-llm", "prod")),
		row("team-a", "staging", "main", "https://otlp-a.example/otlp", "200", "a-token", "a-staging-key", "scope-a2", "scope-id-a2", identity("team-a-llm", "staging")),
		row("team-b", "prod", "main", "https://otlp-b.example/otlp", "201", "b-token", "b-key", "scope-b1", "scope-id-b1", identity("team-b-llm", "prod")),
	}
	if len(plan.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d: %+v", len(plan.Rows), len(want), plan.Rows)
	}
	for i := range want {
		if !reflect.DeepEqual(plan.Rows[i], want[i]) {
			t.Errorf("row %d =\n%+v\nwant\n%+v", i, plan.Rows[i], want[i])
		}
	}
}

// AC1 (#531), rule 1: a target's destination must be one of its own tenant's. Naming another tenant's
// destination is no reference at all. Red on base (Resolve returned no rows and no error).
func TestResolveRuleUnknownDestination(t *testing.T) {
	cfg := tenantConfig()
	editTarget(cfg, "team-a", "prod", func(tg *config.TargetConfig) { tg.Destination = "missing" })
	wantResolveError(t, cfg, `tenants.team-a.targets.prod.destination "missing" is not a destination of tenant team-a`)

	cfg = tenantConfig()
	tb := cfg.Tenants["team-b"]
	tb.Destinations["other"] = config.TenantDestinationConfig{OTLP: config.TenantOTLPConfig{Endpoint: "https://otlp-b2.example/otlp", InstanceID: "203", TokenFile: "t"}}
	editTarget(cfg, "team-a", "prod", func(tg *config.TargetConfig) { tg.Destination = "other" })
	wantResolveError(t, cfg, `tenants.team-a.targets.prod.destination "other" is not a destination of tenant team-a`)
}

// AC1 (#531), rule 2: two tenants may not share a stack. The same endpoint under another instance id is
// another stack. Red on base.
func TestResolveRuleTwoTenantsOneStack(t *testing.T) {
	cfg := tenantConfig()
	editDestination(cfg, "team-b", "main", func(o *config.TenantOTLPConfig) {
		o.Endpoint, o.InstanceID = "https://otlp-a.example/otlp", "200"
	})
	wantResolveError(t, cfg, "tenants.team-b.destinations.main is on a stack", "tenant team-a also sends to")

	cfg = tenantConfig()
	editDestination(cfg, "team-b", "main", func(o *config.TenantOTLPConfig) { o.Endpoint = "https://otlp-a.example/otlp" })
	mustResolve(t, cfg) // instance id 201, not 200: another stack
}

// AC1 (#531), rule 3: two targets of one tenant on one stack must differ in identity. staging differs
// from prod only by its own identity block; without it the two collide. Red on base.
func TestResolveRuleTargetsShareStackAndIdentity(t *testing.T) {
	mustResolve(t, tenantConfig()) // same stack, different identity
	cfg := tenantConfig()
	editTarget(cfg, "team-a", "staging", func(tg *config.TargetConfig) { tg.Identity = nil })
	wantResolveError(t, cfg, "tenants.team-a.targets.staging and tenants.team-a.targets.prod send to one stack with one identity")

	// A target identity block equal to the tenant's is the same identity.
	cfg = tenantConfig()
	editTarget(cfg, "team-a", "staging", func(tg *config.TargetConfig) {
		tg.Identity = &config.IdentityConfig{ServiceNamespace: "team-a-llm", DeploymentEnvironment: "prod"}
	})
	wantResolveError(t, cfg, "send to one stack with one identity")
}

// AC1 (#531), rule 4: no target shares both the self-telemetry stack and its identity (the top-level
// identity with config.SelfNamespaceSuffix). The self stack with another identity is allowed, and so is
// the top-level identity without the suffix. Red on base.
func TestResolveRuleSelfTelemetryStackAndIdentity(t *testing.T) {
	onSelfStack := func(cfg *config.Config) {
		editDestination(cfg, "team-a", "alt", func(o *config.TenantOTLPConfig) {
			o.Endpoint, o.InstanceID = "https://otlp-self.example/otlp", "900"
		})
	}
	cfg := tenantConfig()
	onSelfStack(cfg)
	mustResolve(t, cfg) // the tenant's identity on the self stack

	for _, ns := range []string{"bridge-example", "bridge-example-other"} {
		cfg = tenantConfig()
		onSelfStack(cfg)
		editTarget(cfg, "team-a", "mirror", func(tg *config.TargetConfig) {
			tg.Identity = &config.IdentityConfig{ServiceNamespace: ns, DeploymentEnvironment: "test"}
		})
		mustResolve(t, cfg)
	}

	cfg = tenantConfig()
	onSelfStack(cfg)
	editTarget(cfg, "team-a", "mirror", func(tg *config.TargetConfig) {
		tg.Identity = &config.IdentityConfig{ServiceNamespace: "bridge-example" + config.SelfNamespaceSuffix, DeploymentEnvironment: "test"}
	})
	wantResolveError(t, cfg, "tenants.team-a.targets.mirror sends to the self-telemetry stack with the self-telemetry identity")
}

// AC1 (#531), rule 5: no scope name and no scope id in two tenants. config.Validate holds the same rule at
// load; Resolve does not call Validate, so it keeps its own copy, tested here without Validate. One
// tenant naming one scope twice (team-a prod and mirror) is allowed. Red on base.
func TestResolveRuleScopeInTwoTenants(t *testing.T) {
	mustResolve(t, tenantConfig())
	for _, tc := range []struct {
		field string
		edit  func(*config.ScopeConfig)
		want  string
	}{
		{"name", func(s *config.ScopeConfig) { s.Name = "scope-a2" }, `tenants.team-b.targets.prod.scope.name "scope-a2" is also used by tenant team-a`},
		{"id", func(s *config.ScopeConfig) { s.ID = "scope-id-a1" }, `tenants.team-b.targets.prod.scope.id "scope-id-a1" is also used by tenant team-a`},
	} {
		t.Run(tc.field, func(t *testing.T) {
			cfg := tenantConfig()
			editTarget(cfg, "team-b", "prod", func(tg *config.TargetConfig) { tc.edit(&tg.Scope) })
			wantResolveError(t, cfg, tc.want)
		})
	}
}

// AC2 (#531): endpoints that differ only in the host's case or a trailing slash are one stack, for each
// stack rule. Red on base.
func TestResolveStackIgnoresHostCaseAndTrailingSlash(t *testing.T) {
	t.Run("two tenants", func(t *testing.T) {
		cfg := tenantConfig()
		editDestination(cfg, "team-b", "main", func(o *config.TenantOTLPConfig) {
			o.Endpoint, o.InstanceID = "https://OTLP-A.Example/otlp/", "200"
		})
		wantResolveError(t, cfg, "tenants.team-b.destinations.main is on a stack")
	})
	t.Run("two targets of one tenant", func(t *testing.T) {
		cfg := tenantConfig()
		editDestination(cfg, "team-a", "alt", func(o *config.TenantOTLPConfig) {
			o.Endpoint, o.InstanceID = "https://otlp-a.EXAMPLE/otlp/", "200"
		})
		wantResolveError(t, cfg, "tenants.team-a.targets.prod and tenants.team-a.targets.mirror send to one stack with one identity")
	})
	t.Run("self-telemetry", func(t *testing.T) {
		cfg := tenantConfig()
		editDestination(cfg, "team-a", "alt", func(o *config.TenantOTLPConfig) {
			o.Endpoint, o.InstanceID = "https://OTLP-SELF.example/otlp/", "900"
		})
		editTarget(cfg, "team-a", "mirror", func(tg *config.TargetConfig) {
			tg.Identity = &config.IdentityConfig{ServiceNamespace: "bridge-example-meta", DeploymentEnvironment: "test"}
		})
		wantResolveError(t, cfg, "sends to the self-telemetry stack with the self-telemetry identity")
	})
}

const orderYAMLHead = `
identity: {service_namespace: bridge-example, deployment_environment: test}
emit:
  self:
    otlp: {endpoint: https://otlp-self.example/otlp, instance_id: "900", token: example-token}
ha: {coordinator: lease, checkpoint: configmap}
defaults:
  portkey:
    loops:
      groups: {enabled: true, cadence: 60s}
      analytics: {enabled: true, cadence: 60s, settings: {knob: v1}}
tenants:
`

const orderTeamA = `  team-a:
    identity: {service_namespace: team-a-llm, deployment_environment: prod}
    destinations:
      main: {otlp: {endpoint: https://otlp-a.example/otlp, instance_id: "200", token_file: a-token}}
      alt: {otlp: {endpoint: https://otlp-a2.example/otlp, instance_id: "202", token_file: a-token-2}}
    targets:
      prod:
        source: {type: portkey, base_url: https://vendor.example/v1, auth: {header: x-api-key, value_file: a-prod-key}}
        scope: {name: scope-a1, id: scope-id-a1}
        destination: main
      mirror:
        source: {type: portkey, base_url: https://vendor.example/v1, auth: {header: x-api-key, value_file: a-mirror-key}}
        scope: {name: scope-a1, id: scope-id-a1}
        destination: alt
`

// orderTeamAReordered is orderTeamA with its keys at every level listed the other way round.
const orderTeamAReordered = `  team-a:
    targets:
      mirror:
        destination: alt
        scope: {id: scope-id-a1, name: scope-a1}
        source: {type: portkey, base_url: https://vendor.example/v1, auth: {header: x-api-key, value_file: a-mirror-key}}
      prod:
        source: {type: portkey, base_url: https://vendor.example/v1, auth: {header: x-api-key, value_file: a-prod-key}}
        scope: {name: scope-a1, id: scope-id-a1}
        destination: main
    destinations:
      alt: {otlp: {endpoint: https://otlp-a2.example/otlp, instance_id: "202", token_file: a-token-2}}
      main: {otlp: {endpoint: https://otlp-a.example/otlp, instance_id: "200", token_file: a-token}}
    identity: {deployment_environment: prod, service_namespace: team-a-llm}
`

const orderTeamB = `  team-b:
    identity: {service_namespace: team-b-llm, deployment_environment: prod}
    destinations:
      main: {otlp: {endpoint: https://otlp-b.example/otlp, instance_id: "201", token_file: b-token}}
    targets:
      prod:
        source: {type: portkey, base_url: https://vendor.example/v1, auth: {header: x-api-key, value_file: b-key}}
        scope: {name: scope-b1, id: scope-id-b1}
        destination: main
`

// orderTable is the plan table of the configs above, as a literal: a renderer that iterated a map
// unsorted, or printed a secret file name or a token, would not produce it.
const orderTable = "" +
	"tenant  target  source_type  source_url                 scope_name  scope_id     pipeline       destination  endpoint                      instance_id  identity                                                                                      loops\n" +
	"team-a  mirror  portkey      https://vendor.example/v1  scope-a1    scope-id-a1  team-a.mirror  alt          https://otlp-a2.example/otlp  202          deployment.environment.name=prod,service.name=genai-otel-bridge,service.namespace=team-a-llm  analytics,groups\n" +
	"team-a  prod    portkey      https://vendor.example/v1  scope-a1    scope-id-a1  team-a.prod    main         https://otlp-a.example/otlp   200          deployment.environment.name=prod,service.name=genai-otel-bridge,service.namespace=team-a-llm  analytics,groups\n" +
	"team-b  prod    portkey      https://vendor.example/v1  scope-b1    scope-id-b1  team-b.prod    main         https://otlp-b.example/otlp   201          deployment.environment.name=prod,service.name=genai-otel-bridge,service.namespace=team-b-llm  analytics,groups\n"

// AC3 (#531): tenants (and their destinations, targets and keys) listed in another order give the same
// table and hash, the table is the literal above, and resolving again many times (Go randomises map
// iteration on every range) never changes it. Red on base (no rows: the table was the header only).
func TestResolveTenantOrderIndependent(t *testing.T) {
	load := func(doc string) *config.Config {
		t.Helper()
		cfg, err := config.LoadBytes([]byte(doc))
		if err != nil {
			t.Fatalf("LoadBytes: %v", err)
		}
		return cfg
	}
	a := mustResolve(t, load(orderYAMLHead+orderTeamA+orderTeamB))
	b := mustResolve(t, load(orderYAMLHead+orderTeamB+orderTeamAReordered))
	if got := a.Table(); got != orderTable {
		t.Fatalf("table =\n%s\nwant\n%s", got, orderTable)
	}
	if b.Table() != a.Table() || b.Hash() != a.Hash() {
		t.Fatalf("another order changed the plan:\n%s\n%s\nhash %s vs %s", a.Table(), b.Table(), a.Hash(), b.Hash())
	}
	sum := sha256.Sum256([]byte(orderTable))
	if want := hex.EncodeToString(sum[:]); a.Hash() != want {
		t.Fatalf("hash = %s, want %s", a.Hash(), want)
	}
	for i := 0; i < 50; i++ {
		if h := mustResolve(t, load(orderYAMLHead+orderTeamB+orderTeamAReordered)).Hash(); h != a.Hash() {
			t.Fatalf("resolve %d gave hash %s, want %s", i, h, a.Hash())
		}
	}
}

// AC4 (#531): every row owns its loop map, each block's settings map and graphs slice: changing one row
// leaves the other rows, the config's defaults and the next resolve unchanged. Red by mutation: copying
// the loops map shallowly (or not copying settings).
func TestRowsDoNotShareLoopMaps(t *testing.T) {
	cfg := tenantConfig()
	plan := mustResolve(t, cfg)
	if len(plan.Rows) < 2 {
		t.Fatalf("rows = %d, want at least 2", len(plan.Rows))
	}
	pristine := mustResolve(t, tenantConfig()).Rows[1].Source.Loops

	r0 := plan.Rows[0].Source.Loops
	r0["analytics"].Settings["knob"] = "changed"
	r0["analytics"].Settings["added"] = "x"
	r0["analytics"].Graphs[0] = "changed"
	r0["groups"] = config.LoopConfig{Enabled: false, Settings: map[string]string{"new": "x"}}
	delete(r0, "spare")
	plan.Rows[0].Source.HTTP.AllowHosts[0] = "changed.example"

	for i, r := range plan.Rows[1:] {
		if !reflect.DeepEqual(r.Source.Loops, pristine) {
			t.Errorf("row %d loops changed with row 0's: %+v", i+1, r.Source.Loops)
		}
		if r.Source.HTTP.AllowHosts[0] != "vendor.example" {
			t.Errorf("row %d allow_hosts changed with row 0's: %v", i+1, r.Source.HTTP.AllowHosts)
		}
	}
	if !reflect.DeepEqual(cfg.Defaults["portkey"].Loops, pristine) {
		t.Errorf("the config's defaults changed with row 0's loops: %+v", cfg.Defaults["portkey"].Loops)
	}
	if again := mustResolve(t, cfg); !reflect.DeepEqual(again.Rows[0].Source.Loops, pristine) {
		t.Errorf("a second resolve of the same config gave changed loops: %+v", again.Rows[0].Source.Loops)
	}
}

// captureHandler records every slog record it handles.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func captureSlog(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// warnings returns the source_instance attribute of every warning that names one.
func (h *captureHandler) warnings() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "source_instance" {
				out = append(out, a.Value.String())
			}
			return true
		})
	}
	return out
}

// AC5 (#531): Resolve warns once for each enabled legacy source whose source_instance starts with "mt."
// (design §7), naming it, and for nothing else: not a near miss, not a disabled source, not a tenant
// row's own mt. instance. Red on base (no warning).
func TestLegacyMtPrefixWarning(t *testing.T) {
	h := captureSlog(t)
	cfg := legacyConfig()
	cfg.Sources[0].SourceInstance = "mt.team-a.prod"
	cfg.Sources[1].SourceInstance = "mt.disabled.source" // Enabled false
	cfg.Sources[2].SourceInstance = "mtx.alpha-c"
	plan := mustResolve(t, cfg)
	if got, want := h.warnings(), []string{"mt.team-a.prod"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings for source_instance %v, want %v", got, want)
	}
	if got := plan.Rows[0].Source.SourceInstance; got != "mt.team-a.prod" {
		t.Errorf("the warning changed the row: source_instance %q", got)
	}

	h = captureSlog(t)
	mustResolve(t, legacyConfig())
	mustResolve(t, tenantConfig())
	if got := h.warnings(); len(got) != 0 {
		t.Fatalf("warnings %v for configs with no legacy mt. source_instance", got)
	}
}
