// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"testing"
	"time"
)

// [CP-M7] A token-less self endpoint (the in-cluster cleartext Alloy hop, where the collector holds the
// real Grafana Cloud credentials) must export with NO Authorization header — not a useless "Basic Og==".
func TestOTLPAuthHeaders(t *testing.T) {
	if h := otlpAuthHeaders("", ""); len(h) != 0 {
		t.Fatalf("token-less ⇒ no Authorization header, got %v", h)
	}
	h := otlpAuthHeaders("inst", "tok")
	if h["Authorization"] != basicAuth("inst", "tok") {
		t.Fatalf("with creds ⇒ Basic auth header, got %v", h)
	}
}

func TestMinSelfInterval(t *testing.T) {
	cases := []struct {
		maxDPM int
		want   time.Duration
	}{
		{0, time.Minute}, // guard: <1 ⇒ 1
		{1, time.Minute},
		{2, 30 * time.Second},
		{4, 15 * time.Second},
	}
	for _, c := range cases {
		if got := minSelfInterval(c.maxDPM); got != c.want {
			t.Errorf("minSelfInterval(%d)=%v want %v", c.maxDPM, got, c.want)
		}
	}
}

func TestEffectiveSelfInterval(t *testing.T) {
	// configured 0 ⇒ floor; configured below floor ⇒ clamped to floor; configured ≥ floor ⇒ unchanged.
	if got := effectiveSelfInterval(0, 1); got != time.Minute {
		t.Errorf("unset ⇒ floor 60s; got %v", got)
	}
	if got := effectiveSelfInterval(10*time.Second, 1); got != time.Minute {
		t.Errorf("10s @ max_dpm=1 ⇒ clamped 60s; got %v", got)
	}
	if got := effectiveSelfInterval(90*time.Second, 1); got != 90*time.Second {
		t.Errorf("90s ≥ floor ⇒ unchanged; got %v", got)
	}
}
