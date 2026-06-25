// SPDX-License-Identifier: AGPL-3.0-only

// Package eks holds the drift gates that keep the EKS internal-test Helm overrides
// (test/eks/values-eks.yaml) in lockstep with the production config SCHEMA as it evolves.
//
// The EKS file is a real deployment fixture: its VALUES diverge from production on purpose (real
// creds via ${ENV} refs, product/self telemetry split, two live sources, EKS network/image knobs).
// What must NOT drift is the set of config KEYS and the layout — when a new config key is added to the
// schema (and `make generate` refreshes deploy/helm/values.yaml), this gate fails until the key is also
// reflected in the EKS file, so the internal test deploy never silently runs an out-of-date config shape.
//
// Two complementary checks (per the 2026-06-21 decision to keep keys/layout — not values — in sync):
//   - TestEKSValuesKeyParity: every GLOBAL config key path in the generated production block exists in
//     the EKS configOverride. Catches MISSING newly-added keys.
//   - TestEKSValuesLoadsAgainstSchema: the EKS configOverride round-trips through config.Load
//     (KnownFields(true) + secret substitution + type-check). Catches RENAMED/REMOVED/retyped keys.
package eks

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rknightion/genai-otel-bridge/internal/config"
)

const (
	prodValuesPath = "../../deploy/helm/values.yaml"
	eksValuesPath  = "values-eks.yaml"
)

// keyPathExclusions are config subtrees where the EKS deployment LEGITIMATELY diverges from the
// generated production default, so the key-parity gate must not require them to match:
//   - "sources": deployment-specific. The EKS file runs two real sources with all loops + real
//     per-loop settings; production ships one example source. Loop/settings keys differ by design.
//   - "selfobs.profiling.pull" / ".push": MODE-variant. Production renders the default `pull` block;
//     the EKS deploy uses `push` (profiles → Pyroscope). Only one block exists per mode.
var keyPathExclusions = []string{"sources", "selfobs.profiling.pull", "selfobs.profiling.push"}

var envRefRe = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// prodConfigMap returns the generated `config:` map from deploy/helm/values.yaml.
func prodConfigMap(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(prodValuesPath)
	if err != nil {
		t.Fatalf("read prod values: %v", err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(b, &top); err != nil {
		t.Fatalf("parse prod values: %v", err)
	}
	cfg, ok := top["config"].(map[string]any)
	if !ok {
		t.Fatal("prod values.yaml has no `config:` map")
	}
	return cfg
}

// eksConfigOverride returns (the raw configOverride string, its parsed map) from values-eks.yaml.
func eksConfigOverride(t *testing.T) (string, map[string]any) {
	t.Helper()
	b, err := os.ReadFile(eksValuesPath)
	if err != nil {
		t.Fatalf("read eks values: %v", err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(b, &top); err != nil {
		t.Fatalf("parse eks values: %v", err)
	}
	raw, ok := top["configOverride"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		t.Fatal("values-eks.yaml has no `configOverride:` string")
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("parse eks configOverride: %v", err)
	}
	return raw, parsed
}

// keyPaths collects the dotted key paths of a config map (intermediate AND leaf keys), skipping any
// path at or under an excluded prefix. Values are ignored — this compares structure only.
func keyPaths(m map[string]any, prefix string, out map[string]bool) {
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if excluded(path) {
			continue
		}
		out[path] = true
		if child, ok := v.(map[string]any); ok {
			keyPaths(child, path, out)
		}
	}
}

func excluded(path string) bool {
	for _, ex := range keyPathExclusions {
		if path == ex || strings.HasPrefix(path, ex+".") {
			return true
		}
	}
	return false
}

// TestEKSValuesKeyParity fails if the EKS configOverride is missing any GLOBAL config key the current
// generated production block carries — i.e. a schema key was added but not propagated to the internal
// test deploy. Values are not compared; `sources` and the mode-variant profiling blocks are excluded.
func TestEKSValuesKeyParity(t *testing.T) {
	prod := map[string]bool{}
	keyPaths(prodConfigMap(t), "", prod)
	eks := map[string]bool{}
	_, eksCfg := eksConfigOverride(t)
	keyPaths(eksCfg, "", eks)

	var missing []string
	for p := range prod {
		if !eks[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("test/eks/values-eks.yaml is missing config keys present in the production schema "+
			"(deploy/helm/values.yaml) — add them (values may differ, keys/layout must match):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestEKSValuesLoadsAgainstSchema round-trips the EKS configOverride through config.Load — proving it
// still parses against the current schema (KnownFields(true) rejects an unknown/renamed/removed key;
// the type-checked decode catches a retyped key; secret substitution resolves the ${ENV} refs). Dummy
// env values are injected for every ${VAR} the override references (endpoints get an https value to
// satisfy the endpoint gate; the rest get a non-empty placeholder). Semantic validation (window math,
// etc.) is deliberately NOT run here — that is about VALUES, which the EKS deploy is free to differ on.
func TestEKSValuesLoadsAgainstSchema(t *testing.T) {
	raw, _ := eksConfigOverride(t)
	for _, m := range envRefRe.FindAllStringSubmatch(raw, -1) {
		name := m[1]
		val := "x"
		if strings.Contains(name, "ENDPOINT") || strings.Contains(name, "URL") {
			val = "https://placeholder.example"
		}
		t.Setenv(name, val)
	}
	f, err := os.CreateTemp(t.TempDir(), "eks-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := config.Load(f.Name()); err != nil {
		t.Fatalf("EKS configOverride no longer loads against the current schema "+
			"(a key was renamed/removed/retyped, or a ref is malformed): %v", err)
	}
}
