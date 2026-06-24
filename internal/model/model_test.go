// SPDX-License-Identifier: AGPL-3.0-only

package model

import "testing"

func TestCheckpointKeyStringStable(t *testing.T) {
	k := CheckpointKey{SourceInstance: "portkey-prod-eu", Loop: "analytics", OutputFingerprint: "abc123"}
	got := k.String()
	if got != "portkey-prod-eu/analytics/abc123" {
		t.Fatalf("unstable key: %q", got)
	}
	// Same fields ⇒ same string (used as a map/store key).
	if k.String() != (CheckpointKey{"portkey-prod-eu", "analytics", "abc123"}).String() {
		t.Fatal("CheckpointKey.String not deterministic")
	}
}

func TestFingerprintDeterministicAndOrderInsensitive(t *testing.T) {
	a := Fingerprint([]string{"portkey_api_requests", "portkey_api_cost_usd"}, "prefix=portkey_api")
	b := Fingerprint([]string{"portkey_api_cost_usd", "portkey_api_requests"}, "prefix=portkey_api")
	if a != b {
		t.Fatalf("fingerprint must be order-insensitive: %q vs %q", a, b)
	}
	c := Fingerprint([]string{"portkey_api_requests"}, "prefix=portkey_api")
	if a == c {
		t.Fatal("adding a series must change the fingerprint (new history bootstraps, F37)")
	}
}
