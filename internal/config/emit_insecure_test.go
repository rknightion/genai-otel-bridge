// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

// [CP-M7] emit.{telemetry,self}.otlp.allow_insecure opts an OTLP emit endpoint out of the https-only
// gate for an in-cluster cleartext receiver (the regulated/EKS topology). The flag permits an http
// non-loopback endpoint ONLY token-less (no credential rides cleartext) and ONLY for a private target
// (an IP literal must be RFC-1918/loopback/link-local; a DNS host is permitted, unresolvable at config
// time, under the token-less guarantee). https is always fine; the flag is then a no-op.
func TestValidateEmitAllowInsecure(t *testing.T) {
	cases := []struct {
		name     string
		ep, id   string
		tok      string
		insecure bool
		wantErr  string // "" ⇒ Validate must return nil
	}{
		{"https ok, flag off", "https://otlp.example/v1", "1", "t", false, ""},
		{"http public rejected without flag", "http://1.2.3.4:4318", "", "", false, "must be https"},
		{"loopback ok without flag", "http://127.0.0.1:4318", "", "", false, ""},
		{"http private allowed with flag, token-less", "http://10.1.2.3:4318", "", "", true, ""},
		{"http cluster-dns allowed with flag, token-less", "http://alloy.monitoring.svc:4318", "", "", true, ""},
		{"http with flag but token set rejected", "http://10.1.2.3:4318", "id", "tok", true, "credential"},
		{"http with flag but instance_id set rejected", "http://10.1.2.3:4318", "id", "", true, "credential"},
		{"http public IP rejected even with flag", "http://8.8.8.8:4318", "", "", true, "private"},
		{"https with flag is a no-op", "https://otlp.example/v1", "1", "t", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t)
			cfg, err := Load("testdata/valid.yaml")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Emit.Telemetry.OTLP.Endpoint = tc.ep
			cfg.Emit.Telemetry.OTLP.InstanceID = tc.id
			cfg.Emit.Telemetry.OTLP.Token = tc.tok
			cfg.Emit.Telemetry.OTLP.AllowInsecure = tc.insecure
			err = cfg.Validate(map[string]struct{}{"portkey": {}})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// The same gate applies to the optional separate self-telemetry endpoint (it carries the same Basic
// auth shape as the product endpoint).
func TestValidateEmitSelfAllowInsecure(t *testing.T) {
	setEnv(t)
	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// token-less http private self endpoint with the flag ⇒ OK
	cfg.Emit.Self = &OTLPTarget{OTLP: OTLPConn{Endpoint: "http://10.0.0.5:4318", AllowInsecure: true}}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err != nil {
		t.Fatalf("token-less http private self endpoint with flag should pass, got %v", err)
	}
	// a credential on the cleartext self endpoint ⇒ rejected
	cfg.Emit.Self = &OTLPTarget{OTLP: OTLPConn{Endpoint: "http://10.0.0.5:4318", Token: "tok", AllowInsecure: true}}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("credential on cleartext self endpoint must be rejected, got %v", err)
	}
	// http self endpoint WITHOUT the flag ⇒ rejected (https required)
	cfg.Emit.Self = &OTLPTarget{OTLP: OTLPConn{Endpoint: "http://10.0.0.5:4318"}}
	if err := cfg.Validate(map[string]struct{}{"portkey": {}}); err == nil || !strings.Contains(err.Error(), "must be https") {
		t.Fatalf("http self endpoint without flag must require https, got %v", err)
	}
}
