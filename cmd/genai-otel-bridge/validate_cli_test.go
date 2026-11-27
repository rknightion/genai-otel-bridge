// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateConfigCLI(t *testing.T) {
	// Re-exec the test binary to exercise the real flags, output streams and os.Exit in main.
	if args := flag.Args(); len(args) > 0 && args[0] == "-validate-config" {
		os.Args = append(os.Args[:1], args...)
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		main()
		t.Fatal("-validate-config returned without exiting")
	}

	raw, err := os.ReadFile("../../internal/config/testdata/tenants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Point every source and destination at a local probe, not a live service. The distinct
	// instance IDs in the fixture still give the tenants and self-telemetry separate stacks.
	var requests atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(func() {
		probe.Close()
		if n := requests.Load(); n != 0 {
			t.Errorf("-validate-config sent %d HTTP requests, want none", n)
		}
	})
	fixture := strings.NewReplacer(
		"https://api.portkey.ai/v1", probe.URL+"/v1\n          http: { allow_private: true }",
		"https://otlp-self.example.com", probe.URL,
		"https://otlp-a.example.com", probe.URL,
		"https://otlp-b.example.com", probe.URL,
	).Replace(string(raw))
	// On the base, two targets referencing one auth file received the same fabricated key,
	// so the CLI skipped team-a before it could check the team's sources and loops.
	sharedFile := replaceConfigOnce(t, fixture, "value_file: staging-key", "value_file: prod-key")

	t.Run("scope-capability", func(t *testing.T) {
		body := replaceConfigOnce(t, sharedFile, "tenants:\n", `  langsmith:
    loops:
      sessions:
        enabled: true
        cadence: 60s

tenants:
`)
		body = replaceConfigOnce(t, body, "          type: portkey\n          base_url: "+probe.URL+`/v1
          http: { allow_private: true }
          auth: { header: x-portkey-api-key, value_file: prod-key }
          rate_limit: { rps: 1, burst: 3 }
`, "          type: langsmith\n          base_url: "+probe.URL+`
          http: { allow_private: true }
          auth: { header: x-api-key, value_file: prod-key }
`)
		assertValidationFailure(t, body, `source "mt.team-a.prod" (type "langsmith") has a scope, but the source type cannot assert one`)
	})

	t.Run("tenant-logs", func(t *testing.T) {
		// With only team-a, the base failed for its manufactured duplicate key, obscuring the
		// log-loop error. A constructor-valid logs_export loop reaches the actual tenant rule.
		body, _, ok := strings.Cut(sharedFile, "  team-b:\n")
		if !ok {
			t.Fatal("fixture has no team-b")
		}
		body = replaceConfigOnce(t, body, "tenants:\n", `      logs_export:
        enabled: true
        cadence: 60s
        settings:
          workspace_id: example-workspace-id
          signed_url_allow_hosts: storage.example.com
          window: 1h
          settle: 10m

tenants:
`)
		assertValidationFailure(t, body, `pipeline "team-a.prod" (source type "portkey"): loop "logs_export" emits log records`)
	})

	t.Run("tenant-plan-output", func(t *testing.T) {
		// Files are not mounted in CI. Sharing a filename is not a credential check here;
		// it must not skip pipelines or change the table and hash compared with distinct names.
		path := filepath.Join(t.TempDir(), "config.yaml")
		want, stderr, code := invokeValidateCLI(t, path, fixture)
		if code != 0 || stderr != "" || !strings.Contains(want, "validate-config: OK ("+path+") plan ") {
			t.Fatalf("valid tenant config: exit %d, stdout %q, stderr %q", code, want, stderr)
		}
		for _, id := range []string{"team-a.prod", "team-a.staging", "team-b.prod"} {
			if !strings.Contains(want, id) {
				t.Fatalf("valid tenant plan is missing %s: %q", id, want)
			}
		}
		got, stderr, code := invokeValidateCLI(t, path, sharedFile)
		if code != 0 || stderr != "" || got != want {
			t.Fatalf("shared filename must not change validation or plan output; exit %d, stdout %q, stderr %q, want stdout %q", code, got, stderr, want)
		}
	})
}

func assertValidationFailure(t *testing.T, body, diagnostic string) {
	t.Helper()
	stdout, stderr, code := invokeValidateCLI(t, filepath.Join(t.TempDir(), "config.yaml"), body)
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "validate-config: FAIL: ") ||
		!strings.Contains(stderr, diagnostic) || strings.Contains(stderr, "duplicate_key") {
		t.Fatalf("want %q; exit %d, stdout %q, stderr %q", diagnostic, code, stdout, stderr)
	}
}

func invokeValidateCLI(t *testing.T, path, body string) (stdout, stderr string, code int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestValidateConfigCLI$", "--", "-validate-config", "-config", path)
	cmd.Dir = filepath.Dir(path)
	cmd.Env = append(cmd.Environ(), "TENANT_FIXTURE_SELF_TOKEN=", "HOME="+cmd.Dir, "NO_PROXY=*")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok && ctx.Err() == nil {
			return out.String(), errOut.String(), exit.ExitCode()
		}
		t.Fatalf("run -validate-config: %v (context: %v, stderr: %s)", err, ctx.Err(), errOut.String())
	}
	return out.String(), errOut.String(), 0
}

func replaceConfigOnce(t *testing.T, body, old, new string) string {
	t.Helper()
	if strings.Count(body, old) != 1 {
		t.Fatalf("fixture anchor %q is not unique", old)
	}
	return strings.Replace(body, old, new, 1)
}
