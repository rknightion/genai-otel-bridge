// SPDX-License-Identifier: Apache-2.0

package selfobs

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"os"
)

// perPipelineRules are the bundled rules that judge one pipeline's series. Each carries tenant and
// pipeline in every by (...) so two pipelines never merge, and a series with no tenant label is
// grouped as before (design section 8, unit 6d).
var perPipelineRules = []string{
	"alertrule-auth-errors.yaml",
	"alertrule-bucket-revised-after-settle.yaml",
	"alertrule-data-loss.yaml",
	"alertrule-emit-failing.yaml",
	"alertrule-loop-degraded.yaml",
	"alertrule-poller-stale.yaml",
	"alertrule-queue-backpressure.yaml",
	"alertrule-window-truncated.yaml",
	"alertrule-cardinality-spike.yaml",
	"alertrule-partial-reject.yaml",
	"recordingrule-freshness-ratio.yaml",
	"recordingrule-last-success-age.yaml",
	"recordingrule-last-success-age-baseline.yaml",
	"recordingrule-scrape-healthy.yaml",
	"recordingrule-window-truncated-rate5m.yaml",
}

// unchangedRules stay as they are: per replica, per upstream host, or refused in the tenant shape.
var unchangedRules = []string{
	"alertrule-leader-absent.yaml",
	"alertrule-memory-near-limit.yaml",
	"alertrule-no-standby.yaml",
	"alertrule-upstream-error-budget.yaml",
	"recordingrule-upstream-error-ratio-5m.yaml",
	"recordingrule-scrape-present.yaml",
	"alertrule-export-pipeline-data-incomplete.yaml",
	"alertrule-workspace-scope-permission-denied.yaml",
}

var byClauseRe = regexp.MustCompile(`\bby\s*\(([^)]*)\)`)

func hasLabel(clause, label string) bool {
	for _, l := range strings.Split(clause, ",") {
		if strings.TrimSpace(l) == label {
			return true
		}
	}
	return false
}

// ruleSpec is the part of a bundled alert rule file the tests pin.
type ruleSpec struct {
	Spec struct {
		Title  string            `yaml:"title"`
		For    string            `yaml:"for"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"spec"`
}

func TestBundledRuleGroupings(t *testing.T) {
	dir := "../../deploy/grafana/self-obs"
	for _, name := range perPipelineRules {
		es, _ := exprs(t, filepath.Join(dir, name))
		if len(es) == 0 {
			t.Fatalf("%s: no expression found", name)
		}
		clauses := 0
		for _, e := range es {
			for _, m := range byClauseRe.FindAllStringSubmatch(e, -1) {
				clauses++
				for _, l := range []string{"tenant", "pipeline"} {
					if !hasLabel(m[1], l) {
						t.Errorf("%s: by (%s) does not name %s", name, m[1], l)
					}
				}
			}
		}
		if clauses == 0 {
			t.Errorf("%s: no by (...) clause found; the check proved nothing", name)
		}
	}
	for _, name := range unchangedRules {
		es, _ := exprs(t, filepath.Join(dir, name))
		for _, e := range es {
			if strings.Contains(e, "tenant") {
				t.Errorf("%s names tenant but is on the unchanged list: %s", name, e)
			}
		}
	}

	// The stale-poller alert and its baseline judge only pipelines whose scope is matched, and keep
	// series with no tenant label as today.
	for _, name := range []string{"alertrule-poller-stale.yaml", "recordingrule-last-success-age-baseline.yaml"} {
		es, _ := exprs(t, filepath.Join(dir, name))
		e := strings.Join(es, "\n")
		if !regexp.MustCompile(`and on\s*\(tenant, pipeline\)`).MatchString(e) || !strings.Contains(e, `genai_otel_bridge_scope_state{state="matched"}`) {
			t.Errorf("%s: no scope_state matched join (and on (tenant, pipeline)): %s", name, e)
		}
		if !strings.Contains(e, `tenant=""`) {
			t.Errorf("%s: no branch for series with no tenant label (tenant=\"\"): %s", name, e)
		}
	}

	for _, want := range []struct{ file, title, forDur, severity, metric string }{
		{"alertrule-scope-mismatched.yaml", "GenaiOtelBridgeScopeMismatched", "5m", "critical", "genai_otel_bridge_scope_state"},
		{"alertrule-tenant-blocked.yaml", "GenaiOtelBridgeTenantBlocked", "5m", "critical", "genai_otel_bridge_tenant_blocked"},
		{"alertrule-scope-assertion-errors.yaml", "GenaiOtelBridgeScopeAssertionErrors", "30m", "warning", "genai_otel_bridge_scope_assertion_total"},
		{"alertrule-scope-unverified.yaml", "GenaiOtelBridgeScopeUnverified", "24h", "info", "genai_otel_bridge_scope_state"},
	} {
		b, err := os.ReadFile(filepath.Join(dir, want.file))
		if err != nil {
			t.Errorf("new rule file missing: %v", err)
			continue
		}
		var r ruleSpec
		if err := yaml.Unmarshal(b, &r); err != nil {
			t.Fatalf("%s: %v", want.file, err)
		}
		if r.Spec.Title != want.title || r.Spec.For != want.forDur || r.Spec.Labels["severity"] != want.severity {
			t.Errorf("%s: title %q for %q severity %q, want %q %q %q", want.file, r.Spec.Title, r.Spec.For, r.Spec.Labels["severity"], want.title, want.forDur, want.severity)
		}
		es, _ := exprs(t, filepath.Join(dir, want.file))
		e := strings.Join(es, "\n")
		if !strings.Contains(e, want.metric) {
			t.Errorf("%s: expression does not read %s: %s", want.file, want.metric, e)
		}
		if !strings.Contains(e, "max by") {
			t.Errorf("%s: expression does not take the max across replicas: %s", want.file, e)
		}
		if !strings.Contains(string(b), "noDataState: Ok") {
			t.Errorf("%s: no data must be healthy (noDataState: Ok)", want.file)
		}
	}
}
