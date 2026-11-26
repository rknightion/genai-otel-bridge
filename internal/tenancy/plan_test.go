// SPDX-License-Identifier: Apache-2.0

package tenancy_test

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"regexp"
	"strings"
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
