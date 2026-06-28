// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config/gen/helmgen"
)

// ecsConfigPath is the generated ECS config example. The byte-compare drift gate that re-renders it
// lives in the EXTERNAL test (ecs_config_example_test.go) because it imports the source packages to
// build the examples block; this internal test only loads the committed file.
const ecsConfigPath = "../../deploy/ecs/terraform/config.example.yaml"

// TestECSProfileDefaultsMatchLoad ties the ECS render profile's hard-coded DynamoDB-default STRINGS
// (helmgen.ECSProfile, which cannot import config) to the values Load actually applies — so the
// generated example shows the real defaults and the two cannot silently diverge. It loads a minimal
// dynamodb config and checks each defaulted field equals the corresponding profile string.
func TestECSProfileDefaultsMatchLoad(t *testing.T) {
	cfg, err := LoadBytes([]byte("ha:\n  coordinator: dynamodb\n  checkpoint: dynamodb\n  dynamodb:\n    table: t\n"))
	if err != nil {
		t.Fatalf("LoadBytes minimal dynamodb config: %v", err)
	}
	prof := helmgen.ECSProfile()

	if prof.Defaults["HAConfig.Coordinator"] != "dynamodb" || prof.Defaults["HAConfig.Checkpoint"] != "dynamodb" {
		t.Errorf("ECSProfile must flip both HA backends to dynamodb, got coordinator=%q checkpoint=%q",
			prof.Defaults["HAConfig.Coordinator"], prof.Defaults["HAConfig.Checkpoint"])
	}
	if got := prof.Defaults["DynamoDBHAConfig.LockName"]; got != cfg.HA.DynamoDB.LockName {
		t.Errorf("lock_name: profile=%q but Load default=%q — update helmgen.ECSProfile", got, cfg.HA.DynamoDB.LockName)
	}

	durs := []struct {
		key  string
		load Duration
	}{
		{"DynamoDBHAConfig.LeaseDuration", cfg.HA.DynamoDB.LeaseDuration},
		{"DynamoDBHAConfig.RenewDeadline", cfg.HA.DynamoDB.RenewDeadline},
		{"DynamoDBHAConfig.RetryPeriod", cfg.HA.DynamoDB.RetryPeriod},
	}
	for _, d := range durs {
		parsed, err := time.ParseDuration(prof.Defaults[d.key])
		if err != nil {
			t.Errorf("%s: profile default %q is not a valid duration: %v", d.key, prof.Defaults[d.key], err)
			continue
		}
		if Duration(parsed) != d.load {
			t.Errorf("%s: profile=%q (%v) but Load default=%v — update helmgen.ECSProfile",
				d.key, prof.Defaults[d.key], parsed, time.Duration(d.load))
		}
	}
}

// TestECSConfigLoadsAgainstSchema round-trips the committed ECS example through LoadBytes (the same
// in-memory path the ECS container uses for GENAI_OTEL_BRIDGE_CONFIG) — proving the generated default
// config actually parses against the current schema, with the dynamodb HA block present. Dummy env
// values are injected for every ${VAR} the file references (endpoints get https; the rest a non-empty
// placeholder). Mirrors test/eks/values_sync_test.go's loads-against-schema check.
func TestECSConfigLoadsAgainstSchema(t *testing.T) {
	raw, err := os.ReadFile(ecsConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", ecsConfigPath, err)
	}
	for _, m := range envRefRe.FindAllStringSubmatch(string(raw), -1) {
		name := m[1]
		val := "x"
		if strings.Contains(name, "ENDPOINT") || strings.Contains(name, "URL") {
			val = "https://placeholder.example"
		}
		t.Setenv(name, val)
	}
	cfg, err := LoadBytes(raw)
	if err != nil {
		t.Fatalf("generated ECS config no longer loads against the current schema: %v", err)
	}
	if cfg.HA.Coordinator != "dynamodb" || cfg.HA.Checkpoint != "dynamodb" {
		t.Errorf("generated ECS config must use dynamodb HA, got coordinator=%q checkpoint=%q",
			cfg.HA.Coordinator, cfg.HA.Checkpoint)
	}
	if cfg.HA.DynamoDB.Table == "" {
		t.Error("generated ECS config must set ha.dynamodb.table")
	}
}
