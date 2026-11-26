// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// The tenant shape's load rules (multi-tenancy design §2 and §3, unit 6a #530). Each case is a variant
// of testdata/tenants.yaml, which is itself valid, so a case fails only for the rule it names.

var tenantKnownTypes = map[string]struct{}{"portkey": {}, "langsmith": {}}

const tenantFixtureSelfTokenEnv = "TENANT_FIXTURE_SELF_TOKEN"

func tenantFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/tenants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tenantVariant replaces old with next in body, failing unless old occurs exactly once, so a fixture
// edit cannot leave a case testing the unchanged fixture.
func tenantVariant(t *testing.T, body, old, next string) string {
	t.Helper()
	if n := strings.Count(body, old); n != 1 {
		t.Fatalf("variant anchor %q occurs %d times, want 1", old, n)
	}
	return strings.Replace(body, old, next, 1)
}

// loadAndValidate is what startup does: load (here from bytes), then Validate.
func loadAndValidate(t *testing.T, body string) error {
	t.Helper()
	t.Setenv(tenantFixtureSelfTokenEnv, "self-token-example")
	cfg, err := LoadBytes([]byte(body))
	if err != nil {
		return err
	}
	return cfg.Validate(tenantKnownTypes)
}

func TestTenantFixtureValid(t *testing.T) {
	if err := loadAndValidate(t, tenantFixture(t)); err != nil {
		t.Fatalf("testdata/tenants.yaml must load and validate: %v", err)
	}
}

type tenantEdit struct{ old, next string }

// AC1 (#530): one case per load-phase rule. Each variant must be refused, with the rule's own message.
func TestTenantLoadRules(t *testing.T) {
	const teamBTarget = "          type: portkey\n          base_url: https://api.portkey.ai/v1\n          auth: { header: x-portkey-api-key, value_file: source-key }\n"
	const teamBDestination = "          endpoint: https://otlp-b.example.com/otlp\n          instance_id: \"200002\"\n          token_file: otlp-token\n"
	cases := []struct {
		name  string
		edits []tenantEdit
		extra string // appended to the fixture
		want  string
	}{
		// Shape.
		{name: "sources beside tenants", extra: "sources:\n  - type: portkey\n    enabled: true\n    source_instance: legacy-a\n",
			want: "sources must be empty in the tenant shape"},
		{name: "emit.telemetry endpoint", edits: []tenantEdit{{"emit:\n", "emit:\n  telemetry:\n    otlp:\n      endpoint: https://otlp.example.com/otlp\n"}},
			want: "emit.telemetry must be empty in the tenant shape"},
		{name: "emit.telemetry instance id", edits: []tenantEdit{{"emit:\n", "emit:\n  telemetry:\n    otlp:\n      instance_id: \"300000\"\n"}},
			want: "emit.telemetry must be empty in the tenant shape"},
		{name: "emit.telemetry token", edits: []tenantEdit{{"emit:\n", "emit:\n  telemetry:\n    otlp:\n      token: token-example\n"}},
			want: "emit.telemetry must be empty in the tenant shape"},
		{name: "emit.self absent", edits: []tenantEdit{{"  self:\n    otlp:\n      endpoint: https://otlp-self.example.com/otlp\n      instance_id: \"100000\"\n      token: ${TENANT_FIXTURE_SELF_TOKEN}\n", ""}},
			want: "emit.self is required in the tenant shape"},
		{name: "install identity incomplete", edits: []tenantEdit{{"  deployment_environment: test\n", ""}},
			want: "identity: service_namespace and deployment_environment are required in the tenant shape"},
		{name: "ha coordinator none", edits: []tenantEdit{{"  coordinator: lease\n", "  coordinator: none\n"}},
			want: "ha.coordinator must be lease and ha.checkpoint must be configmap in the tenant shape"},
		{name: "ha dynamodb", edits: []tenantEdit{{"  coordinator: lease\n  checkpoint: configmap\n", "  coordinator: dynamodb\n  checkpoint: dynamodb\n  dynamodb:\n    table: example-table\n"}},
			want: "ha.coordinator must be lease and ha.checkpoint must be configmap in the tenant shape"},

		// Ids and names.
		{name: "tenant id upper case", edits: []tenantEdit{{"  team-b:\n", "  Team-B:\n"}}, want: `tenant id "Team-B" must match`},
		{name: "tenant id trailing hyphen", edits: []tenantEdit{{"  team-b:\n", "  team-b-:\n"}}, want: `tenant id "team-b-" must match`},
		{name: "tenant id over 40 characters", edits: []tenantEdit{{"  team-b:\n", "  " + strings.Repeat("b", 41) + ":\n"}}, want: `tenant id "` + strings.Repeat("b", 41) + `" must match`},
		{name: "destination name", edits: []tenantEdit{{"      main:\n        otlp:\n          endpoint: https://otlp-b", "      main_b:\n        otlp:\n          endpoint: https://otlp-b"}},
			want: `tenants.team-b: destination name "main_b" must match`},
		{name: "target name", edits: []tenantEdit{{"      staging:\n", "      staging.eu:\n"}}, want: `tenants.team-a: target name "staging.eu" must match`},
		{name: "tenant with no targets", edits: []tenantEdit{{"    targets:\n      prod:\n        source:\n" + teamBTarget + "        scope:\n          name: example-scope-b1\n          id: example-scope-id-b1\n        destination: main\n", "    targets: {}\n"}},
			want: "tenants.team-b: at least one target is required"},

		// Scope.
		{name: "scope block absent", edits: []tenantEdit{{"        scope:\n          name: example-scope-b1\n          id: example-scope-id-b1\n", ""}},
			want: "tenants.team-b.targets.prod.scope.name is required"},
		{name: "scope name empty", edits: []tenantEdit{{"          name: example-scope-a1\n", "          name: \"\"\n"}},
			want: "tenants.team-a.targets.prod.scope.name is required"},
		{name: "scope id empty", edits: []tenantEdit{{"          id: example-scope-id-a1\n", "          id: \"\"\n"}},
			want: "tenants.team-a.targets.prod.scope.id is required"},
		{name: "scope name control character", edits: []tenantEdit{{"          name: example-scope-a1\n", "          name: \"example\\tscope-a1\"\n"}},
			want: "tenants.team-a.targets.prod.scope.name must not contain a control character"},
		{name: "scope id control character", edits: []tenantEdit{{"          id: example-scope-id-a1\n", "          id: \"example-scope-id-a1\\x7f\"\n"}},
			want: "tenants.team-a.targets.prod.scope.id must not contain a control character"},
		{name: "scope name in two tenants", edits: []tenantEdit{{"          name: example-scope-b1\n", "          name: example-scope-a1\n"}},
			want: `scope name "example-scope-a1" is used by tenants team-a and team-b`},
		{name: "scope id in two tenants", edits: []tenantEdit{{"          id: example-scope-id-b1\n", "          id: example-scope-id-a2\n"}},
			want: `scope id "example-scope-id-a2" is used by tenants team-a and team-b`},

		// Identities.
		{name: "tenant identity absent", edits: []tenantEdit{{"    identity:\n      service_namespace: team-b-llm\n      deployment_environment: prod\n", ""}},
			want: "tenants.team-b.identity: service_namespace and deployment_environment are required"},
		{name: "tenant identity incomplete", edits: []tenantEdit{{"      service_namespace: team-b-llm\n", ""}},
			want: "tenants.team-b.identity: service_namespace and deployment_environment are required"},
		{name: "target identity incomplete", edits: []tenantEdit{{"          deployment_environment: staging\n", ""}},
			want: "tenants.team-a.targets.staging.identity: service_namespace and deployment_environment are both required"},
		{name: "target identity empty block", edits: []tenantEdit{{"        identity:\n          service_namespace: team-a-llm\n          deployment_environment: staging\n", "        identity: {}\n"}},
			want: "tenants.team-a.targets.staging.identity: service_namespace and deployment_environment are both required"},

		// Secret file names.
		{name: "token_file absent", edits: []tenantEdit{{teamBDestination, "          endpoint: https://otlp-b.example.com/otlp\n          instance_id: \"200002\"\n"}},
			want: "tenants.team-b.destinations.main.otlp.token_file is required"},
		{name: "token_file with a path separator", edits: []tenantEdit{{teamBDestination, "          endpoint: https://otlp-b.example.com/otlp\n          instance_id: \"200002\"\n          token_file: ../team-a/otlp-token\n"}},
			want: "tenants.team-b.destinations.main.otlp.token_file must be a bare file name"},
		{name: "token_file dot", edits: []tenantEdit{{teamBDestination, "          endpoint: https://otlp-b.example.com/otlp\n          instance_id: \"200002\"\n          token_file: .\n"}},
			want: "tenants.team-b.destinations.main.otlp.token_file must be a bare file name"},
		{name: "token_file dot dot", edits: []tenantEdit{{teamBDestination, "          endpoint: https://otlp-b.example.com/otlp\n          instance_id: \"200002\"\n          token_file: ..\n"}},
			want: "tenants.team-b.destinations.main.otlp.token_file must be a bare file name"},
		{name: "value_file absent", edits: []tenantEdit{{"x-portkey-api-key, value_file: source-key }", "x-portkey-api-key }"}},
			want: "tenants.team-b.targets.prod.source.auth.value_file is required"},
		{name: "value_file absolute path", edits: []tenantEdit{{"value_file: source-key }", "value_file: /etc/source-key }"}},
			want: "tenants.team-b.targets.prod.source.auth.value_file must be a bare file name"},

		// Destinations.
		{name: "destination endpoint absent", edits: []tenantEdit{{"          endpoint: https://otlp-b.example.com/otlp\n", ""}},
			want: "tenants.team-b.destinations.main.otlp.endpoint is required"},
		{name: "destination endpoint plain text", edits: []tenantEdit{{"          endpoint: https://otlp-b.example.com/otlp\n", "          endpoint: http://otlp-b.example.com/otlp\n"}},
			want: "tenants.team-b.destinations.main.otlp.endpoint must be https://"},
		{name: "destination endpoint loopback look-alike", edits: []tenantEdit{{"          endpoint: https://otlp-b.example.com/otlp\n", "          endpoint: http://localhost.otlp-b.example.com/otlp\n"}},
			want: "tenants.team-b.destinations.main.otlp.endpoint must be https://"},
		{name: "destination instance_id absent", edits: []tenantEdit{{"          instance_id: \"200002\"\n", ""}},
			want: "tenants.team-b.destinations.main.otlp.instance_id is required"},
		{name: "destination allow_insecure is not a key", edits: []tenantEdit{{teamBDestination, teamBDestination + "          allow_insecure: true\n"}},
			want: "field allow_insecure not found"},

		// Target source.
		{name: "source type absent", edits: []tenantEdit{{teamBTarget, strings.Replace(teamBTarget, "          type: portkey\n", "", 1)}},
			want: "tenants.team-b.targets.prod.source.type is required"},
		{name: "source type unknown", edits: []tenantEdit{{teamBTarget, strings.Replace(teamBTarget, "type: portkey", "type: nosuchtype", 1)}},
			want: `tenants.team-b.targets.prod.source.type: unknown source type "nosuchtype"`},
		{name: "source type without defaults", edits: []tenantEdit{{teamBTarget, strings.Replace(teamBTarget, "type: portkey", "type: langsmith", 1)}},
			want: `tenants.team-b.targets.prod.source.type "langsmith" has no defaults.langsmith block`},
		{name: "source base_url absent", edits: []tenantEdit{{teamBTarget, strings.Replace(teamBTarget, "          base_url: https://api.portkey.ai/v1\n", "", 1)}},
			want: "tenants.team-b.targets.prod.source.base_url must be a non-empty https:// URL"},
		{name: "source base_url plain text", edits: []tenantEdit{{teamBTarget, strings.Replace(teamBTarget, "https://api.portkey.ai/v1", "http://api.example.com/v1", 1)}},
			want: "tenants.team-b.targets.prod.source.base_url must be a non-empty https:// URL"},
		{name: "source auth header absent", edits: []tenantEdit{{"{ header: x-portkey-api-key, value_file: source-key }", "{ value_file: source-key }"}},
			want: "tenants.team-b.targets.prod.source.auth.header is required"},
		{name: "source rate_limit rps negative", edits: []tenantEdit{{"rate_limit: { rps: 1, burst: 3 }", "rate_limit: { rps: -1, burst: 3 }"}},
			want: "tenants.team-a.targets.prod.source.rate_limit.rps must be > 0"},
		{name: "source rate_limit burst negative", edits: []tenantEdit{{"rate_limit: { rps: 1, burst: 3 }", "rate_limit: { rps: 1, burst: -1 }"}},
			want: "tenants.team-a.targets.prod.source.rate_limit.burst must be >= 1"},
		{name: "source enabled is not a key", edits: []tenantEdit{{teamBTarget, teamBTarget + "          enabled: true\n"}},
			want: "field enabled not found"},
		{name: "source source_instance is not a key", edits: []tenantEdit{{teamBTarget, teamBTarget + "          source_instance: pk\n"}},
			want: "field source_instance not found"},
		{name: "target loops is not a key", edits: []tenantEdit{{"          id: example-scope-id-b1\n", "          id: example-scope-id-b1\n        loops: {}\n"}},
			want: "field loops not found"},
		{name: "target destination absent", edits: []tenantEdit{{"          id: example-scope-id-b1\n        destination: main\n", "          id: example-scope-id-b1\n"}},
			want: "tenants.team-b.targets.prod.destination is required"},

		// Defaults.
		{name: "defaults unknown type", edits: []tenantEdit{{"defaults:\n", "defaults:\n  nosuchtype:\n    loops:\n      x:\n        enabled: true\n        cadence: 60s\n"}},
			want: `defaults.nosuchtype: unknown source type "nosuchtype"`},
		{name: "defaults loop cadence under the floor", edits: []tenantEdit{{"        cadence: 60s\n        window: 50m\n", "        cadence: 5s\n        window: 50m\n"}},
			want: "defaults.portkey.loops.analytics: cadence 5s < floor 10s"},
		{name: "defaults loop window over the clamp", edits: []tenantEdit{{"        window: 50m\n", "        window: 60m\n"}},
			want: "defaults.portkey.loops.analytics: window 1h0m0s > 55m0s"},
		{name: "defaults loop negative settle", edits: []tenantEdit{{"        bucket_settle: 10m\n", "        bucket_settle: -10m\n"}},
			want: "defaults.portkey.loops.analytics: bucket_settle must be >= 0"},
		{name: "defaults loop lookback over backfill", edits: []tenantEdit{{"        bootstrap_lookback: 50m\n", "        bootstrap_lookback: 100m\n"}},
			want: "defaults.portkey.loops.analytics: bootstrap_lookback 1h40m0s > max_backfill 1h30m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tenantFixture(t)
			for _, e := range tc.edits {
				body = tenantVariant(t, body, e.old, e.next)
			}
			body += tc.extra
			err := loadAndValidate(t, body)
			if err == nil {
				t.Fatalf("accepted; want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q\ndoes not contain %q", err, tc.want)
			}
		})
	}
}

// What the rules allow: each variant must still load and validate.
func TestTenantLoadRulesAllow(t *testing.T) {
	cases := []struct {
		name  string
		edits []tenantEdit
	}{
		// One tenant may name one scope in two targets, with two keys, to send it to two destinations.
		{name: "one scope in two targets of one tenant", edits: []tenantEdit{
			{"          name: example-scope-a2\n", "          name: example-scope-a1\n"},
			{"          id: example-scope-id-a2\n", "          id: example-scope-id-a1\n"},
		}},
		{name: "loopback destination for development", edits: []tenantEdit{{"          endpoint: https://otlp-b.example.com/otlp\n", "          endpoint: http://127.0.0.1:4318/otlp\n"}}},
		{name: "plain-text source with allow_private", edits: []tenantEdit{{
			"          base_url: https://api.portkey.ai/v1\n          auth: { header: x-portkey-api-key, value_file: source-key }\n",
			"          base_url: http://10.0.0.5/v1\n          auth: { header: x-portkey-api-key, value_file: source-key }\n          http: { allow_private: true }\n"}}},
		{name: "rate_limit omitted takes the defaults", edits: []tenantEdit{{"          rate_limit: { rps: 1, burst: 3 }\n", ""}}},
		{name: "disabled defaults loop is not checked", edits: []tenantEdit{{"      groups:\n        enabled: true\n        cadence: 60s\n", "      groups:\n        enabled: false\n        cadence: 1s\n"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tenantFixture(t)
			for _, e := range tc.edits {
				body = tenantVariant(t, body, e.old, e.next)
			}
			if err := loadAndValidate(t, body); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// The legacy shape: defaults must be absent. An empty tenants or defaults map counts as absent (the
// design's shape rule counts entries), so a chart can render either empty for a legacy install.
func TestTenantLegacyShapeRules(t *testing.T) {
	setEnv(t)
	cfg, err := Load(withExtraYAML(t, "defaults:\n  portkey:\n    loops:\n      groups:\n        enabled: true\n        cadence: 60s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(tenantKnownTypes); err == nil || !strings.Contains(err.Error(), "defaults is allowed only in the tenant shape") {
		t.Fatalf("a legacy config with defaults: got %v, want the defaults-only-in-the-tenant-shape error", err)
	}
	for _, extra := range []string{"tenants: {}\ndefaults: {}\n", "tenants: {}\n", "defaults: {}\n"} {
		cfg, err := Load(withExtraYAML(t, extra))
		if err != nil {
			t.Fatalf("%q: Load: %v", extra, err)
		}
		if err := cfg.Validate(tenantKnownTypes); err != nil {
			t.Fatalf("%q: a legacy config with an empty map must validate: %v", extra, err)
		}
	}
}

// AC2 (#530): no value under tenants or defaults may be an ${ENV} or a file: reference. The rule runs
// after placeholder injection and parsing and before secret resolution, so it names the key path and
// never reads the file. Under config.Load an unset variable fails first, in placeholder injection.
// The ValidateConfigFile half is TestTenantRefsRejected in internal/app.
func TestTenantRefsRejected(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("file-secret-value-never-shown"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingFile := filepath.Join(t.TempDir(), "absent")
	cases := []struct {
		name     string
		edits    []tenantEdit
		env      map[string]string
		wantPath string
	}{
		{name: "env ref under tenants", edits: []tenantEdit{{"          id: example-scope-id-b1\n", "          id: ${TENANT_REF_SCOPE_ID}\n"}},
			env: map[string]string{"TENANT_REF_SCOPE_ID": "example-scope-id-b1"}, wantPath: "tenants.team-b.targets.prod.scope.id"},
		{name: "env ref inside a flow mapping", edits: []tenantEdit{{"value_file: source-key }", "value_file: ${TENANT_REF_KEY} }"}},
			env: map[string]string{"TENANT_REF_KEY": "env-secret-value-never-shown"}, wantPath: "tenants.team-b.targets.prod.source.auth.value_file"},
		{name: "env ref as a tenant id", edits: []tenantEdit{{"  team-b:\n", "  ${TENANT_REF_ID}:\n"}},
			env: map[string]string{"TENANT_REF_ID": "team-b"}, wantPath: "tenants (a key)"},
		{name: "env ref under defaults", edits: []tenantEdit{{"        metric_prefix: portkey_api\n", "        metric_prefix: ${TENANT_REF_PREFIX}\n"}},
			env: map[string]string{"TENANT_REF_PREFIX": "portkey_api"}, wantPath: "defaults.portkey.loops.analytics.metric_prefix"},
		{name: "env ref in a defaults sequence", edits: []tenantEdit{{"graphs: [requests, cost, tokens, latency, errors]", "graphs: [requests, ${TENANT_REF_GRAPH}]"}},
			env: map[string]string{"TENANT_REF_GRAPH": "cost"}, wantPath: "defaults.portkey.loops.analytics.graphs[1]"},
		{name: "env ref reached through an alias", edits: []tenantEdit{
			{"  deployment_environment: test\n", "  deployment_environment: &install_env ${TENANT_REF_ENV}\n"},
			{"      service_namespace: team-b-llm\n", "      service_namespace: *install_env\n"},
		}, env: map[string]string{"TENANT_REF_ENV": "test"}, wantPath: "tenants.team-b.identity.service_namespace"},
		{name: "env ref merged in with <<", edits: []tenantEdit{
			{"identity:\n  service_namespace: genai-otel-bridge\n  deployment_environment: test\n", "identity: &install_id\n  service_namespace: genai-otel-bridge\n  deployment_environment: ${TENANT_REF_MERGED}\n"},
			{"    identity:\n      service_namespace: team-b-llm\n      deployment_environment: prod\n", "    identity:\n      <<: *install_id\n"},
		}, env: map[string]string{"TENANT_REF_MERGED": "test"}, wantPath: "tenants.team-b.identity.deployment_environment"},
		{name: "file ref under tenants", edits: []tenantEdit{{"value_file: source-key }", "value_file: file:" + secretFile + " }"}},
			wantPath: "tenants.team-b.targets.prod.source.auth.value_file"},
		{name: "file ref to a missing file under tenants", edits: []tenantEdit{{"          name: example-scope-b1\n", "          name: file:" + missingFile + "\n"}},
			wantPath: "tenants.team-b.targets.prod.scope.name"},
		{name: "file ref under defaults", edits: []tenantEdit{{"        metric_prefix: portkey_api\n", "        metric_prefix: file:" + secretFile + "\n"}},
			wantPath: "defaults.portkey.loops.analytics.metric_prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tenantFixture(t)
			for _, e := range tc.edits {
				body = tenantVariant(t, body, e.old, e.next)
			}
			t.Setenv(tenantFixtureSelfTokenEnv, "self-token-example")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted a reference under tenants or defaults")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantPath+": an ${ENV} or file: reference is not allowed under tenants or defaults") {
				t.Fatalf("error %q\ndoes not name %s as a refused reference", msg, tc.wantPath)
			}
			for _, secret := range []string{"file-secret-value-never-shown", "env-secret-value-never-shown"} {
				if strings.Contains(msg, secret) {
					t.Fatalf("error carries the referenced value: %q", msg)
				}
			}

			// Precedence (Change 2): with the variable unset, placeholder injection fails first and its
			// error, which names the variable, is the one returned.
			for k := range tc.env {
				t.Setenv(k, "")
				_, err := Load(path)
				if err == nil || !strings.Contains(err.Error(), "required env \""+k+"\" is unset") {
					t.Fatalf("with %s unset: got %v, want the unset-variable error first", k, err)
				}
			}
		})
	}
	// References outside tenants and defaults keep working: the fixture's emit.self token is one.
	t.Setenv(tenantFixtureSelfTokenEnv, "self-token-example")
	if _, err := LoadBytes([]byte(tenantFixture(t))); err != nil {
		t.Fatalf("an env ref under emit.self must still resolve: %v", err)
	}
}

// The fixture decodes into the tenant types, and Load applies the legacy rate-limit and settle defaults
// to tenant sources and defaults loop blocks (design §2: complete blocks after the loader's defaults).
func TestTenantDefaultsApplied(t *testing.T) {
	t.Setenv(tenantFixtureSelfTokenEnv, "self-token-example")
	body := tenantVariant(t, tenantFixture(t), "rate_limit: { rps: 1, burst: 3 }", "rate_limit: { rps: 5 }")
	cfg, err := LoadBytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Tenants["team-a"]
	if got := a.Targets["prod"].Source.RateLimit; got.RPS != 5 || got.Burst != defaultRateLimitBurst {
		t.Errorf("team-a prod rate_limit = %+v, want rps 5 kept and burst defaulted to %d", got, defaultRateLimitBurst)
	}
	if got := a.Targets["staging"].Source.RateLimit; got.RPS != defaultRateLimitRPS || got.Burst != defaultRateLimitBurst {
		t.Errorf("team-a staging rate_limit = %+v, want the defaults", got)
	}
	loops := cfg.Defaults["portkey"].Loops
	if got := time.Duration(loops["groups"].BucketSettle); got != defaultBucketSettle {
		t.Errorf("defaults groups bucket_settle = %s, want the default %s", got, defaultBucketSettle)
	}
	if got := time.Duration(loops["analytics"].BucketSettle); got != 10*time.Minute {
		t.Errorf("defaults analytics bucket_settle = %s, want the explicit 10m", got)
	}

	// The rest of the fixture as decoded.
	prod := a.Targets["prod"]
	if prod.Scope != (ScopeConfig{Name: "example-scope-a1", ID: "example-scope-id-a1"}) || prod.Destination != "main" ||
		prod.Source.Auth != (TenantAuthConfig{Header: "x-portkey-api-key", ValueFile: "prod-key"}) || prod.Identity != nil {
		t.Errorf("team-a prod = %+v", prod)
	}
	if id := a.Targets["staging"].Identity; id == nil || *id != (IdentityConfig{ServiceNamespace: "team-a-llm", DeploymentEnvironment: "staging"}) {
		t.Errorf("team-a staging identity = %+v", id)
	}
	if d := a.Destinations["main"].OTLP; d != (TenantOTLPConfig{Endpoint: "https://otlp-a.example.com/otlp", InstanceID: "200001", TokenFile: "otlp-token"}) {
		t.Errorf("team-a main destination = %+v", d)
	}
	if a.Identity != (IdentityConfig{ServiceNamespace: "team-a-llm", DeploymentEnvironment: "prod"}) {
		t.Errorf("team-a identity = %+v", a.Identity)
	}
}

// tenantMapKeyNames names the map keys in a tenant config key path, as the design writes them.
var tenantMapKeyNames = map[string]string{
	"tenants": "<id>", "defaults": "<type>", "destinations": "<name>", "targets": "<name>", "loops": "<loop>", "settings": "<key>",
}

// tenantConfigKeyPaths reflects the tenant config schema: every leaf key path under the Config fields
// tagged tenants and defaults, with each map key written as tenantMapKeyNames says.
func tenantConfigKeyPaths(t *testing.T) []string {
	t.Helper()
	cfgType := reflect.TypeOf(Config{})
	var paths []string
	for _, root := range []string{"tenants", "defaults"} {
		found := false
		for i := 0; i < cfgType.NumField(); i++ {
			f := cfgType.Field(i)
			if yamlKey(f) == root {
				found = true
				paths = append(paths, keyPaths(root, root, f.Type)...)
			}
		}
		if !found {
			t.Fatalf("Config has no field tagged yaml:%q", root)
		}
	}
	sort.Strings(paths)
	return paths
}

func yamlKey(f reflect.StructField) string { return strings.Split(f.Tag.Get("yaml"), ",")[0] }

// keyPaths returns the leaf key paths of typ under path; name is the YAML key typ sits under.
func keyPaths(path, name string, typ reflect.Type) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch {
	case typ.Kind() == reflect.Map:
		key, ok := tenantMapKeyNames[name]
		if !ok {
			key = "<key>"
		}
		return keyPaths(path+"."+key, name, typ.Elem())
	case typ.Kind() == reflect.Struct && typ != reflect.TypeOf(Duration(0)):
		var out []string
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			k := yamlKey(f)
			if k == "" || k == "-" {
				continue
			}
			out = append(out, keyPaths(path+"."+k, k, f.Type)...)
		}
		return out
	default: // a scalar or a list of scalars is a leaf
		return []string{path}
	}
}

// forbiddenTenantKeys returns the key paths with a key that names a vendor or a vendor concept (G9).
func forbiddenTenantKeys(paths []string) []string {
	var bad []string
	for _, p := range paths {
		for _, seg := range strings.Split(p, ".") {
			low := strings.ToLower(seg)
			for _, word := range []string{"portkey", "workspace", "langsmith"} {
				if strings.Contains(low, word) {
					bad = append(bad, p)
				}
			}
		}
	}
	return bad
}

// AC3 (#530), G9: no key in the tenant config schema names a vendor. The second half seeds a forbidden key
// in a stand-in type, so a walk that stopped looking would fail here too.
func TestTenantKeysVendorNeutral(t *testing.T) {
	paths := tenantConfigKeyPaths(t)
	if len(paths) == 0 {
		t.Fatal("the reflected tenant schema has no keys")
	}
	if bad := forbiddenTenantKeys(paths); len(bad) > 0 {
		t.Fatalf("tenant config keys name a vendor (G9): %v", bad)
	}
	type seededScope struct {
		WorkspaceSlug string `yaml:"workspace_slug"`
	}
	type seededTarget struct {
		Scope seededScope `yaml:"scope"`
	}
	type seededTenant struct {
		Targets map[string]seededTarget `yaml:"targets"`
	}
	seeded := keyPaths("tenants", "tenants", reflect.TypeOf(map[string]seededTenant{}))
	if bad := forbiddenTenantKeys(seeded); len(bad) != 1 || bad[0] != "tenants.<id>.targets.<name>.scope.workspace_slug" {
		t.Fatalf("a seeded forbidden key was not reported: paths %v, reported %v", seeded, bad)
	}
}

// AC4 (#530), G8: the tenant config key paths are a published contract. A change here is a migration.
func TestTenantConfigKeyPathsGolden(t *testing.T) {
	want := []string{
		"defaults.<type>.loops.<loop>.bootstrap_lookback",
		"defaults.<type>.loops.<loop>.bucket_settle",
		"defaults.<type>.loops.<loop>.cadence",
		"defaults.<type>.loops.<loop>.enabled",
		"defaults.<type>.loops.<loop>.graphs",
		"defaults.<type>.loops.<loop>.max_backfill",
		"defaults.<type>.loops.<loop>.metric_prefix",
		"defaults.<type>.loops.<loop>.settings.<key>",
		"defaults.<type>.loops.<loop>.window",
		"tenants.<id>.destinations.<name>.otlp.endpoint",
		"tenants.<id>.destinations.<name>.otlp.instance_id",
		"tenants.<id>.destinations.<name>.otlp.token_file",
		"tenants.<id>.identity.deployment_environment",
		"tenants.<id>.identity.service_namespace",
		"tenants.<id>.targets.<name>.destination",
		"tenants.<id>.targets.<name>.identity.deployment_environment",
		"tenants.<id>.targets.<name>.identity.service_namespace",
		"tenants.<id>.targets.<name>.scope.id",
		"tenants.<id>.targets.<name>.scope.name",
		"tenants.<id>.targets.<name>.source.auth.header",
		"tenants.<id>.targets.<name>.source.auth.value_file",
		"tenants.<id>.targets.<name>.source.base_url",
		"tenants.<id>.targets.<name>.source.http.allow_hosts",
		"tenants.<id>.targets.<name>.source.http.allow_private",
		"tenants.<id>.targets.<name>.source.http.user_agent",
		"tenants.<id>.targets.<name>.source.rate_limit.burst",
		"tenants.<id>.targets.<name>.source.rate_limit.rps",
		"tenants.<id>.targets.<name>.source.type",
	}
	got := tenantConfigKeyPaths(t)
	if !slices.Equal(got, want) {
		t.Fatalf("tenant config key paths changed (G8):\ngot  %q\nwant %q", got, want)
	}
}

// Repair of #554 (Codex P1): a reference or an alias used as a mapping key is resolved like a value, so a
// top-level key spelled ${SECTION} (or file:<path>) could become tenants or defaults after the reference
// check had passed it by, and the whole subtree would go unchecked. Load refuses a reference as a top-level
// key outright, resolves an alias key before comparing it, and refuses an alias key inside tenants or
// defaults that carries a reference. Each case also carries a reference inside the subtree, which must
// never load.
func TestTenantRefAsKeyRejected(t *testing.T) {
	sectionFile := filepath.Join(t.TempDir(), "section")
	if err := os.WriteFile(sectionFile, []byte("tenants"), 0o600); err != nil {
		t.Fatal(err)
	}
	innerTenantRef := tenantEdit{"          id: example-scope-id-b1\n", "          id: ${TENANT_KEY_INNER}\n"}
	cases := []struct {
		name  string
		edits []tenantEdit
		env   map[string]string
		want  string
	}{
		{name: "env ref as the tenants key", edits: []tenantEdit{{"\ntenants:\n", "\n${TENANT_KEY_SECTION}:\n"}, innerTenantRef},
			env: map[string]string{"TENANT_KEY_SECTION": "tenants", "TENANT_KEY_INNER": "example-scope-id-b1"}, want: "config: a top-level key is an ${ENV} or file: reference"},
		{name: "env ref as the defaults key", edits: []tenantEdit{{"\ndefaults:\n", "\n${TENANT_KEY_SECTION}:\n"}, {"        metric_prefix: portkey_api\n", "        metric_prefix: ${TENANT_KEY_INNER}\n"}},
			env: map[string]string{"TENANT_KEY_SECTION": "defaults", "TENANT_KEY_INNER": "portkey_api"}, want: "config: a top-level key is an ${ENV} or file: reference"},
		{name: "file ref as the tenants key", edits: []tenantEdit{{"\ntenants:\n", "\nfile:" + sectionFile + ":\n"}, innerTenantRef},
			env: map[string]string{"TENANT_KEY_INNER": "example-scope-id-b1"}, want: "config: a top-level key is an ${ENV} or file: reference"},
		{name: "alias as the tenants key", edits: []tenantEdit{{"  service_namespace: genai-otel-bridge\n", "  service_namespace: &section tenants\n"}, {"\ntenants:\n", "\n*section :\n"}, innerTenantRef},
			env: map[string]string{"TENANT_KEY_INNER": "example-scope-id-b1"}, want: "config: tenants.team-b.targets.prod.scope.id: an ${ENV} or file: reference is not allowed"},
		{name: "alias carrying a ref as a tenant id", edits: []tenantEdit{{"      token: ${TENANT_FIXTURE_SELF_TOKEN}\n", "      token: &self_token ${TENANT_FIXTURE_SELF_TOKEN}\n"}, {"  team-b:\n", "  *self_token :\n"}},
			want: "config: tenants (a key): an ${ENV} or file: reference is not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tenantFixture(t)
			for _, e := range tc.edits {
				body = tenantVariant(t, body, e.old, e.next)
			}
			t.Setenv(tenantFixtureSelfTokenEnv, "self-token-never-shown")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted a config whose tenants or defaults key hid a reference")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q\ndoes not contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "self-token-never-shown") {
				t.Fatalf("error carries a referenced value: %q", err)
			}
		})
	}

	// A legacy config with references in values, not keys, still loads and validates.
	setEnv(t)
	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("legacy valid.yaml: %v", err)
	}
	if err := cfg.Validate(tenantKnownTypes); err != nil {
		t.Fatalf("legacy valid.yaml: %v", err)
	}
}

// Repair of #554, round 2 (Codex P1): resolveSecretsNode resolves every scalar of the tree, keys included,
// so a reference inside a complex key (a list or a mapping used as a key) was resolved without being
// checked. Load now refuses any key that is not a scalar, anywhere in the config, before references
// resolve, and the tenant reference check covers every scalar key form. One case per key form.
func TestConfigKeyFormsChecked(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("file-secret-value-never-shown"), 0o600); err != nil {
		t.Fatal(err)
	}
	const teamBDestination = "          endpoint: https://otlp-b.example.com/otlp\n          instance_id: \"200002\"\n          token_file: otlp-token\n"
	const complexKey = "a mapping key must be a single value, not a list or a mapping"
	cases := []struct {
		name  string
		edits []tenantEdit
		extra string
		env   map[string]string
		want  string
	}{
		// Complex keys, refused anywhere.
		{name: "list key with a file ref under tenants", edits: []tenantEdit{{"\ntenants:\n", "\ntenants:\n  ? [file:" + secretFile + "]\n  : {}\n"}},
			want: "config: tenants: " + complexKey},
		{name: "mapping key with an env ref under defaults", edits: []tenantEdit{{"\ndefaults:\n", "\ndefaults:\n  ? {x: ${TENANT_FORM_REF}}\n  : {}\n"}},
			env: map[string]string{"TENANT_FORM_REF": "env-secret-value-never-shown"}, want: "config: defaults: " + complexKey},
		{name: "list key with a missing file ref, never read", edits: []tenantEdit{{"\ntenants:\n", "\ntenants:\n  ? [file:" + filepath.Join(t.TempDir(), "absent") + "]\n  : {}\n"}},
			want: "config: tenants: " + complexKey},
		{name: "flow list key under tenants", edits: []tenantEdit{{"  team-b:\n", "  team-b:\n    [file:" + secretFile + "]: {}\n"}},
			want: "config: tenants.team-b: " + complexKey},
		{name: "alias to a list as a key under tenants", edits: []tenantEdit{
			{"graphs: [requests, cost, tokens, latency, errors]", "graphs: &graph_list [requests, cost, tokens, latency, errors]"},
			{"  team-b:\n", "  *graph_list : {}\n  team-b:\n"},
		}, want: "config: tenants: " + complexKey},
		{name: "list key at the top level", extra: "? [file:" + secretFile + "]\n: {}\n", want: "config: (top level): " + complexKey},
		{name: "mapping key in a legacy section", edits: []tenantEdit{{"  service_namespace: genai-otel-bridge\n", "  service_namespace: genai-otel-bridge\n  ? {k: v}\n  : x\n"}},
			want: "config: identity: " + complexKey},

		// Scalar keys in other forms, under tenants and defaults.
		{name: "tagged key with an env ref", edits: []tenantEdit{{"  team-b:\n", "  !!str ${TENANT_FORM_REF}:\n"}},
			env: map[string]string{"TENANT_FORM_REF": "team-b"}, want: "config: tenants (a key): an ${ENV} or file: reference is not allowed"},
		{name: "quoted key with a file ref", edits: []tenantEdit{{"      service_namespace: team-b-llm\n", "      \"file:" + secretFile + "\": team-b-llm\n"}},
			want: "config: tenants.team-b.identity (a key): an ${ENV} or file: reference is not allowed"},
		{name: "flow mapping key with an env ref", edits: []tenantEdit{{"        scope:\n          name: example-scope-b1\n          id: example-scope-id-b1\n", "        scope: { \"${TENANT_FORM_REF}\": example-scope-b1, id: example-scope-id-b1 }\n"}},
			env: map[string]string{"TENANT_FORM_REF": "name"}, want: "config: tenants.team-b.targets.prod.scope (a key): an ${ENV} or file: reference is not allowed"},
		{name: "explicit-key indicator with a file ref under defaults", edits: []tenantEdit{{"    loops:\n      analytics:\n", "    loops:\n      ? file:" + secretFile + "\n      : {enabled: false}\n      analytics:\n"}},
			want: "config: defaults.portkey.loops (a key): an ${ENV} or file: reference is not allowed"},
		{name: "merge of a list of aliases carrying a ref", edits: []tenantEdit{
			{"  self:\n    otlp:\n", "  self:\n    otlp: &self_otlp\n"},
			{"ha:\n  coordinator", "ha: &ha_block\n  coordinator"},
			{teamBDestination, "          <<: [*ha_block, *self_otlp]\n"},
		}, want: "config: tenants.team-b.destinations.main.otlp[1].token: an ${ENV} or file: reference is not allowed"},
		{name: "merge of a list of aliases carrying a ref under defaults", edits: []tenantEdit{
			{"  self:\n    otlp:\n", "  self:\n    otlp: &self_otlp\n"},
			{"      groups:\n        enabled: true\n", "      groups:\n        <<: [*self_otlp]\n        enabled: true\n"},
		}, want: "config: defaults.portkey.loops.groups[0].token: an ${ENV} or file: reference is not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tenantFixture(t)
			for _, e := range tc.edits {
				body = tenantVariant(t, body, e.old, e.next)
			}
			body += tc.extra
			t.Setenv(tenantFixtureSelfTokenEnv, "self-token-never-shown")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted the key form")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q\ndoes not contain %q", err, tc.want)
			}
			for _, secret := range []string{"self-token-never-shown", "file-secret-value-never-shown", "env-secret-value-never-shown"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error carries a referenced value: %q", err)
				}
			}
		})
	}

	// Legacy configs with ordinary keys, including a merge key, still load.
	setEnv(t)
	if _, err := Load("testdata/valid.yaml"); err != nil {
		t.Fatalf("legacy testdata/valid.yaml: %v", err)
	}
	merged := strings.Replace(string(mustRead(t, "testdata/valid.yaml")), "queue:\n  max_batches: 256\n", "queue: &q\n  max_batches: 256\n", 1) +
		"governance:\n  <<: {max_dpm: 1}\n"
	if _, err := LoadBytes([]byte(merged)); err != nil {
		t.Fatalf("a legacy config with an anchor and a merge key must load: %v", err)
	}
}
