// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
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
	if err := ValidateConfigFile(writeCfg(t, validCfg)); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

// A schema violation (unknown key) is caught by Load.
func TestValidateConfigFile_UnknownKey(t *testing.T) {
	err := ValidateConfigFile(writeCfg(t, validCfg+"\nbogus_top_level_key: 1\n"))
	if err == nil {
		t.Fatal("expected unknown-key config to fail")
	}
}

// A semantic violation (emit_workers != 1) is caught by Validate — proving Validate runs, not just Load.
func TestValidateConfigFile_SemanticInvalid(t *testing.T) {
	bad := strings.Replace(validCfg, "emit_workers: 1", "emit_workers: 2", 1)
	err := ValidateConfigFile(writeCfg(t, bad))
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
	if err := ValidateConfigFile(writeCfg(t, bad)); err == nil {
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
			if err := ValidateConfigFile(writeCfg(t, body)); err == nil {
				t.Fatal("-validate-config reports OK for a config Build refuses")
			}
		})
	}
}

// The shipped valid fixture still passes with no secrets set (Build runs with inert dependencies).
func TestValidateConfigFile_ValidFixture(t *testing.T) {
	if err := ValidateConfigFile("../config/testdata/valid.yaml"); err != nil {
		t.Fatalf("valid.yaml should pass: %v", err)
	}
}
