// SPDX-License-Identifier: AGPL-3.0-only

package config

import "testing"

// TestProductIdentityEmitsStableEnvKey guards M1: the emitted environment resource attribute must be the
// Stable semconv key `deployment.environment.name` (semconv v1.42.0 registry/attributes/deployment.md:17
// Stable; the bare `deployment.environment` is Deprecated, :58). This closes the gap where no test
// asserted the product-plane identity key string, so a bare-key regression now fails the gate.
func TestProductIdentityEmitsStableEnvKey(t *testing.T) {
	id := IdentityConfig{ServiceNamespace: "aip-oi", DeploymentEnvironment: "prod"}.ProductIdentity()
	if got, ok := id["deployment.environment.name"]; !ok || got != "prod" {
		t.Fatalf("ProductIdentity must emit deployment.environment.name=prod, got %v", id)
	}
	if _, ok := id["deployment.environment"]; ok {
		t.Fatalf("ProductIdentity must NOT emit the deprecated bare deployment.environment key: %v", id)
	}
}
