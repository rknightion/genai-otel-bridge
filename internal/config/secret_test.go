// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validWithAuth returns valid.yaml with the portkey auth line replaced by authYAML.
func validWithAuth(t *testing.T, authYAML string) []byte {
	t.Helper()
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const old = "auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }"
	if !strings.Contains(string(base), old) {
		t.Fatal("fixture auth line moved")
	}
	return []byte(strings.Replace(string(base), old, authYAML, 1))
}

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// [#381] a file: secret that resolves to nothing is fatal, like an unset ${VAR} (F21).
func TestEmptyFileSecretIsFatal(t *testing.T) {
	setEnv(t)
	p := writeSecretFile(t, "\n")
	raw := strings.Replace(string(mustRead(t, "testdata/valid.yaml")), "token: ${GC_OTLP_TOKEN}", "token: file:"+p, 1)
	_, err := LoadBytes([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "is empty") || !strings.Contains(err.Error(), p) {
		t.Fatalf("empty file secret must be fatal and name the path %s, got %v", p, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// [#381] a decode error on a resolved secret must carry no part of it: not the 7-byte prefix the
// YAML library prints for long scalars, nor a whole short scalar, whether env- or file-sourced.
func TestDecodeErrorNeverEchoesSecret(t *testing.T) {
	setEnv(t)
	const long = "zq9Wk3LmXv7Rt2Bn8Hc5Dj1Fg4Ps6Ya0Eu3Ti9Ox" // 41 chars
	t.Setenv("L1B_SECRET", long)
	longFile := writeSecretFile(t, long+"\n")
	shortFile := writeSecretFile(t, "k4Tq9Zx\n") // 7 chars
	cases := []struct {
		name, auth string
		leaks      []string
	}{
		{"env long", "auth: ${L1B_SECRET}", []string{long[:7], long}},
		{"file long", "auth: file:" + longFile, []string{long[:7], long}},
		{"file short", "auth: file:" + shortFile, []string{"k4Tq9Zx"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadBytes(validWithAuth(t, c.auth))
			if err == nil {
				t.Fatal("expected a decode error")
			}
			for _, l := range c.leaks {
				if strings.Contains(err.Error(), l) {
					t.Fatalf("error leaks secret material %q: %v", l, err)
				}
			}
		})
	}
}

// [#381] a file: secret still resolves trimmed, and a missing file names its path.
func TestLoadBytesResolvesFileSecret(t *testing.T) {
	setEnv(t)
	p := writeSecretFile(t, "  file-tok-123 \n")
	cfg, err := LoadBytes(validWithAuth(t, "auth: { header: x-portkey-api-key, value: file:"+p+" }"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sources[0].Auth.Value; got != "file-tok-123" {
		t.Fatalf("file secret not resolved/trimmed: %q", got)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	_, err = LoadBytes(validWithAuth(t, "auth: { header: x-portkey-api-key, value: file:"+missing+" }"))
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing file secret must error naming the path, got %v", err)
	}
}

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
