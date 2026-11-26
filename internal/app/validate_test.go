// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const validCfg = `
emit:
  telemetry:
    otlp:
      endpoint: ${TEST_ENDPOINT}
      instance_id: ${TEST_ID}
      token: ${TEST_TOK}
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
    source_instance: portkey-test
    auth: { header: x-portkey-api-key, value: ${TEST_PK} }
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

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A valid overlay full of ${ENV} refs validates WITHOUT any secrets set — placeholders are injected.
func TestValidateConfigFile_OK(t *testing.T) {
	if _, _, err := ValidateConfigFile(writeCfg(t, validCfg)); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

// A schema violation (unknown key) is caught by Load.
func TestValidateConfigFile_UnknownKey(t *testing.T) {
	_, _, err := ValidateConfigFile(writeCfg(t, validCfg+"\nbogus_top_level_key: 1\n"))
	if err == nil {
		t.Fatal("expected unknown-key config to fail")
	}
}

// A semantic violation (emit_workers != 1) is caught by Validate — proving Validate runs, not just Load.
func TestValidateConfigFile_SemanticInvalid(t *testing.T) {
	bad := strings.Replace(validCfg, "emit_workers: 1", "emit_workers: 2", 1)
	_, _, err := ValidateConfigFile(writeCfg(t, bad))
	if err == nil {
		t.Fatal("expected emit_workers=2 to fail validation")
	}
	if !strings.Contains(err.Error(), "emit_workers") {
		t.Fatalf("expected an emit_workers error, got: %v", err)
	}
}

// A bad source type is rejected (proves the registry's Known() set is used).
func TestValidateConfigFile_UnknownSourceType(t *testing.T) {
	bad := strings.Replace(validCfg, "type: portkey", "type: nosuchvendor", 1)
	if _, _, err := ValidateConfigFile(writeCfg(t, bad)); err == nil {
		t.Fatal("expected unknown source type to fail")
	}
}

// -validate-config must fail wherever startup (app.Build) would: checks living in source constructors
// and Build were previously skipped, so a config could pass the flag and then crash-loop the pod.
func TestValidateConfigRejectsWhatBuildRejects(t *testing.T) {
	cases := map[string]string{
		"typo'd enabled loop name": strings.Replace(validCfg, "      analytics:\n",
			"      analytics:\n        enabled: true\n        cadence: 60s\n        window: 50m\n        bucket_settle: 10m\n        bootstrap_lookback: 50m\n        max_backfill: 90m\n        metric_prefix: portkey_api\n        graphs: [requests]\n      log_export:\n", 1),
		"unknown graph":              strings.Replace(validCfg, "graphs: [requests, cost, tokens, latency, errors]", "graphs: [requests, nonsense]", 1),
		"content-floor label key":    validCfg + "governance:\n  allow_label_keys: [gen_ai.prompt]\n",
		"missing bootstrap_lookback": strings.Replace(validCfg, "        bootstrap_lookback: 50m\n", "", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if body == validCfg {
				t.Fatal("test case did not alter the config")
			}
			if _, _, err := ValidateConfigFile(writeCfg(t, body)); err == nil {
				t.Fatal("-validate-config reports OK for a config Build refuses")
			}
		})
	}
}

// The shipped valid fixture still passes with no secrets set (Build runs with inert dependencies).
func TestValidateConfigFile_ValidFixture(t *testing.T) {
	if _, _, err := ValidateConfigFile("../config/testdata/valid.yaml"); err != nil {
		t.Fatalf("valid.yaml should pass: %v", err)
	}
}

// AC4 (#522): the config check returns the plan table, which names the source instance, and the plan
// hash, 64 lower-case hex characters; a failed check returns neither.
func TestValidateConfigFile_PrintsPlan(t *testing.T) {
	table, hash, err := ValidateConfigFile(writeCfg(t, validCfg))
	if err != nil {
		t.Fatalf("ValidateConfigFile: %v", err)
	}
	if !strings.Contains(table, "portkey-test") {
		t.Errorf("plan table does not name the source instance portkey-test:\n%s", table)
	}
	if !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(hash) {
		t.Errorf("plan hash %q is not 64 lower-case hex characters", hash)
	}
	bad := strings.Replace(validCfg, "emit_workers: 1", "emit_workers: 2", 1)
	if table, hash, err := ValidateConfigFile(writeCfg(t, bad)); err == nil || table != "" || hash != "" {
		t.Errorf("invalid config: table=%q hash=%q err=%v, want only an error", table, hash, err)
	}
}

// AC5 (#530), flipped by unit 6e-2 (#536): the config check accepts a tenant-shape config that
// validates and assembles, and returns the plan's table (one row per target, named by pipeline id) and
// hash with no error. Until 6e-2 it returned them with tenancy.ErrTenantShapeNotEnabled, which 6e-2
// deleted. main's half is TestTenantShapeAccepted in cmd/genai-otel-bridge.
func TestTenantShapeAccepted(t *testing.T) {
	t.Setenv("TENANT_FIXTURE_SELF_TOKEN", "") // ValidateConfigFile placeholders it; t.Setenv restores it
	const fixture = "../config/testdata/tenants.yaml"
	table, hash, err := ValidateConfigFile(fixture)
	if err != nil {
		t.Fatalf("ValidateConfigFile(%s) = %v, want the tenant shape accepted", fixture, err)
	}
	if !strings.HasPrefix(table, "tenant") || !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(hash) {
		t.Errorf("accepted config carries table %q and hash %q, want the plan's", table, hash)
	}
	for _, p := range []string{"team-a.prod", "team-a.staging", "team-b.prod"} {
		if !strings.Contains(table, p) {
			t.Errorf("plan table does not name pipeline %s:\n%s", p, table)
		}
	}

	// A tenant-shape config that breaks a load rule reports that rule.
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), "  coordinator: lease\n", "  coordinator: none\n", 1)
	if bad == string(raw) {
		t.Fatal("test case did not alter the fixture")
	}
	_, _, err = ValidateConfigFile(writeCfg(t, bad))
	if err == nil || !strings.Contains(err.Error(), "ha.coordinator must be lease") {
		t.Fatalf("an invalid tenant config: got %v, want its load-rule error", err)
	}
}

// AC2 (#530), the ValidateConfigFile half: the config check gives an unset variable a placeholder, so the
// tenant reference rule, not the unset-variable error, reports an ${ENV} reference under tenants or
// defaults; a file: reference is refused by key path without the file being read.
func TestTenantRefsRejected(t *testing.T) {
	raw, err := os.ReadFile("../config/testdata/tenants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("file-secret-value-never-shown"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, old, next, envVar, wantPath string
	}{
		{"env ref under tenants", "value_file: source-key }", "value_file: ${TENANT_VCF_KEY} }", "TENANT_VCF_KEY", "tenants.team-b.targets.prod.source.auth.value_file"},
		{"env ref under defaults", "        metric_prefix: portkey_api\n", "        metric_prefix: ${TENANT_VCF_PREFIX}\n", "TENANT_VCF_PREFIX", "defaults.portkey.loops.analytics.metric_prefix"},
		{"file ref under tenants", "          id: example-scope-id-b1\n", "          id: file:" + secretFile + "\n", "", "tenants.team-b.targets.prod.scope.id"},
		{"file ref under defaults", "        metric_prefix: portkey_api\n", "        metric_prefix: file:" + secretFile + "\n", "", "defaults.portkey.loops.analytics.metric_prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Count(string(raw), tc.old) != 1 {
				t.Fatalf("anchor %q is not unique in the fixture", tc.old)
			}
			t.Setenv("TENANT_FIXTURE_SELF_TOKEN", "")
			if tc.envVar != "" {
				t.Setenv(tc.envVar, "") // unset as far as the check is concerned; restored after the test
			}
			_, _, err := ValidateConfigFile(writeCfg(t, strings.Replace(string(raw), tc.old, tc.next, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.wantPath+": an ${ENV} or file: reference is not allowed under tenants or defaults") {
				t.Fatalf("got %v, want the reference at %s refused", err, tc.wantPath)
			}
			if strings.Contains(err.Error(), "file-secret-value-never-shown") {
				t.Fatalf("error carries the file's value: %v", err)
			}
		})
	}
}

// tenantFixtureVariant is the tenant fixture with each replacement applied once; the test fails if an
// anchor is not found exactly once.
func tenantFixtureVariant(t *testing.T, replacements ...[2]string) string {
	t.Helper()
	raw, err := os.ReadFile("../config/testdata/tenants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, r := range replacements {
		if strings.Count(out, r[0]) != 1 {
			t.Fatalf("anchor %q is not unique in the fixture", r[0])
		}
		out = strings.Replace(out, r[0], r[1], 1)
	}
	return out
}

// TestValidateConfigFileReportsCapabilityError (AC5, #532): the config check assembles a tenant-shape
// config, so a target whose source type cannot assert its scope (langsmith implements no
// source.ScopeAsserter) fails with the capability error, although none of the tenant's secret files
// exists: the check reads none. Before unit 6b-2 it returned the not-enabled refusal before assembling;
// unit 6e-2 (#536) deleted that refusal. The same holds for a loop that emits log records.
func TestValidateConfigFileReportsCapabilityError(t *testing.T) {
	t.Setenv("TENANT_FIXTURE_SELF_TOKEN", "")
	langsmithDefaults := [2]string{"tenants:\n", "  langsmith:\n    loops:\n      sessions:\n        enabled: true\n        cadence: 60s\n        metric_prefix: langsmith\n\ntenants:\n"}
	langsmithTarget := [2]string{"          type: portkey\n          base_url: https://api.portkey.ai/v1\n          auth: { header: x-portkey-api-key, value_file: source-key }\n",
		"          type: langsmith\n          base_url: https://api.smith.langchain.com\n          auth: { header: x-api-key, value_file: source-key }\n"}
	logsLoop := [2]string{"      groups:\n        enabled: true\n        cadence: 60s\n",
		"      groups:\n        enabled: true\n        cadence: 60s\n      logs_export:\n        enabled: true\n        cadence: 60s\n        settings:\n          workspace_id: example-workspace-id\n          signed_url_allow_hosts: storage.example.com\n          window: 1h\n          settle: 10m\n"}
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"a scope the source type cannot assert", tenantFixtureVariant(t, langsmithDefaults, langsmithTarget),
			[]string{"mt.team-b.prod", "langsmith", "cannot assert one (it does not implement source.ScopeAsserter)"}},
		{"a loop that emits log records", tenantFixtureVariant(t, logsLoop),
			[]string{"team-a.prod", "logs_export", "emits log records"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "tenants.yaml")
			if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			table, hash, err := ValidateConfigFile(p)
			if err == nil {
				t.Fatalf("ValidateConfigFile = %v, want the assembly error", err)
			}
			for _, s := range c.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q must contain %q", err, s)
				}
			}
			if table != "" || hash != "" {
				t.Errorf("a failed check returned table %q and hash %q, want neither", table, hash)
			}
		})
	}
}
