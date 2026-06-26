// SPDX-License-Identifier: AGPL-3.0-only

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
