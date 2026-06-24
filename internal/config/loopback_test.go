// SPDX-License-Identifier: AGPL-3.0-only

package config

import "testing"

func TestReviewLoopbackValidationParsesHost(t *testing.T) {
	setEnv(t)
	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Emit.Telemetry.OTLP.Endpoint = "http://localhost.evil.example:4318"
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil {
		t.Fatal("expected non-loopback localhost-prefix host to be rejected")
	}
}
