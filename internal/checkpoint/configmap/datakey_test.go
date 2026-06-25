// SPDX-License-Identifier: AGPL-3.0-only

package configmap

import (
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
