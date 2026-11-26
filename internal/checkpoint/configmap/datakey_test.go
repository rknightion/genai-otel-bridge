// SPDX-License-Identifier: Apache-2.0

package configmap

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// [ext-review-1] CheckpointKey.String() joins fields with '/', which is a valid YAML map key (file
// store) and human-readable for logs, but is NOT a valid ConfigMap data key (k8s requires
// [-._a-zA-Z0-9]+). The configmap store must derive a valid key — and the fake clientset does NOT
// enforce this, so a plain round-trip test would not catch it. Assert the derived key directly.
func TestDataKeyIsValidConfigMapKey(t *testing.T) {
	keys := []model.CheckpointKey{
		{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"},
		{SourceInstance: "portkey-prod", Loop: "analytics", OutputFingerprint: "a1b2c3d4e5f60718"},
		// defensive: even an unsanitary instance must yield a valid key (String() forbids '/' upstream,
		// but the derivation must not depend on that).
		{SourceInstance: "weird inst/with:chars", Loop: "lp", OutputFingerprint: "deadbeef"},
	}
	for _, k := range keys {
		if errs := validation.IsConfigMapKey(dataKey(k)); len(errs) != 0 {
			t.Fatalf("dataKey(%q) = %q is not a valid ConfigMap data key: %v", k.String(), dataKey(k), errs)
		}
	}
}

// Logical keys with DISTINCT String() values must never collide after sanitisation — the hash
// suffix guarantees this even when sanitisation maps different characters to the same '_'.
// (Two keys with the same String() are the same logical key by definition, hence the same data key.)
func TestDataKeyNoCollision(t *testing.T) {
	a := model.CheckpointKey{SourceInstance: "a-b", Loop: "c", OutputFingerprint: "d"}  // String "a-b/c/d"
	c := model.CheckpointKey{SourceInstance: "a:b", Loop: "c", OutputFingerprint: "d"}  // String "a:b/c/d" → same sanitised prefix as...
	dd := model.CheckpointKey{SourceInstance: "a;b", Loop: "c", OutputFingerprint: "d"} // String "a;b/c/d" → both sanitise to "a_b_c_d"
	if a.String() == c.String() || a.String() == dd.String() || c.String() == dd.String() {
		t.Fatal("test setup: keys must have distinct String() values")
	}
	if dataKey(c) == dataKey(dd) {
		t.Fatalf("dataKey collision between distinct keys that sanitise to the same prefix: %q vs %q", dataKey(c), dataKey(dd))
	}
	// Same logical key is stable.
	if dataKey(a) != dataKey(model.CheckpointKey{SourceInstance: "a-b", Loop: "c", OutputFingerprint: "d"}) {
		t.Fatal("dataKey not stable for the same logical key")
	}
}

// Literal goldens for the derived ConfigMap data key. [unit 1, G6] The rule is: replace every byte
// outside [-._a-zA-Z0-9] with '_', cut at 200 characters, append "." and the first 12 hex characters of
// the SHA-256 of the FULL unsanitised String(). The expected values below were computed once, by hand,
// from that rule with an independent tool, and pasted. They are never derived by calling dataKey: a
// change to the cut or the hash length silently resets every watermark of every installed bridge.
func TestDataKeyGolden(t *testing.T) {
	cases := []struct {
		name string
		key  model.CheckpointKey
		want string
	}{
		{
			name: "short key, one separator sanitised",
			key:  model.CheckpointKey{SourceInstance: "portkey-prod", Loop: "analytics", OutputFingerprint: "a1b2c3d4e5f60718"},
			want: "portkey-prod_analytics_a1b2c3d4e5f60718.b44aa579ebe2",
		},
		{
			// 229 characters of String(): the sanitised form is cut at 200 (12 + 188 'a'), the hash still
			// covers all 229.
			name: "sanitised form past the 200 character cut",
			key:  model.CheckpointKey{SourceInstance: "mt.tenant-a." + strings.Repeat("a", 190), Loop: "analytics", OutputFingerprint: "0123456789abcdef"},
			want: "mt.tenant-a.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.108e88837062",
		},
	}
	for _, c := range cases {
		if got := dataKey(c.key); got != c.want {
			t.Errorf("%s: dataKey(%q) = %q, want %q", c.name, c.key.String(), got, c.want)
		}
	}
}

// TestDataKeyTenantGolden [unit 6b-2, G8] pins the ConfigMap data keys of the tenant shape's checkpoint
// keys for the fixture pipeline team-a.prod (internal/config/testdata/tenants.yaml): its analytics loop
// (the key string is pinned in internal/app's tenant_golden_test.go) and its remembered scope match. The
// expected values were computed by hand from the rule TestDataKeyGolden states, with an independent tool,
// and pasted; they are never derived by calling dataKey. Unit 1's cases above are untouched.
func TestDataKeyTenantGolden(t *testing.T) {
	cases := []struct {
		name string
		key  model.CheckpointKey
		want string
	}{
		{
			name: "tenant analytics loop",
			key:  model.CheckpointKey{SourceInstance: "mt.team-a.prod", Loop: "analytics", OutputFingerprint: "a2e79cb406df867d"},
			want: "mt.team-a.prod_analytics_a2e79cb406df867d.7fb1a7e81e29",
		},
		{
			name: "tenant remembered scope match",
			key:  model.CheckpointKey{SourceInstance: "mt.team-a.prod", Loop: "scope-match", OutputFingerprint: "v1"},
			want: "mt.team-a.prod_scope-match_v1.a313b29916b8",
		},
	}
	for _, c := range cases {
		if got := dataKey(c.key); got != c.want {
			t.Errorf("%s: dataKey(%q) = %q, want %q", c.name, c.key.String(), got, c.want)
		}
	}
}
