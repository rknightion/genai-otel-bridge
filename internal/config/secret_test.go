// SPDX-License-Identifier: AGPL-3.0-only

package config

import "testing"

func TestReviewEnvSecretPreservesYAMLSpecialCharacters(t *testing.T) {
	setEnv(t)
	want := "tok # not a yaml comment"
	t.Setenv("GC_OTLP_TOKEN", want)

	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Emit.Telemetry.OTLP.Token; got != want {
		t.Fatalf("token corrupted during secret resolution: got %q want %q", got, want)
	}
}
