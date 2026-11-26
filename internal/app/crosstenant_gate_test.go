// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package app

import (
	"maps"
	"net/http"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// The cross-tenant release gate (unit 5b-1, #528; design §5.3, G2). The tenant shape does not ship while
// it fails (root AGENTS.md). Run: CGO_ENABLED=1 go test -race -tags acceptance ./internal/app/ -run TestCrossTenant
//
// At least three tenants are assembled through BuildFromPlan from hand-made plan rows, with the
// production OTLPEmitterFactory and real OTLP emitters, every destination on the receiver's one URL and
// differing only in instance id and token, and self-telemetry wired as main wires it on that same URL.
// The helpers are in crosstenant_helpers_test.go.

// samplesPerBucket is how many samples the gate's analytics loop emits for one bucket: requests, errors
// and users one each, tokens three (total, input, output).
const samplesPerBucket = 6

// TestCrossTenantScenarios (AC1): every destination shares the receiver's one URL, and all four checks
// hold through each scenario. Each scenario also proves its mechanism ran (a split, a 413, a retry, a
// second leader, a reordered plan, a reused target name), so none passes by not happening.
func TestCrossTenantScenarios(t *testing.T) {
	base := []string{"tenant-a", "tenant-b", "tenant-c"}

	t.Run("a batch split before sending", func(t *testing.T) {
		w := newGateWorld(t)
		w.maxBatchBytes = 64
		w.lead(w.build(w.rows(base...), nil))
		w.requireDelivered(base...)
		for _, n := range base {
			for _, p := range w.recv.under(w.tenant(n).credential()) {
				if p.Status != http.StatusOK {
					t.Errorf("%s: the receiver answered %d; this scenario injects no fault", n, p.Status)
				}
				if p.points() >= samplesPerBucket {
					t.Errorf("%s: a payload of %d points; max_batch_bytes %d should have split every bucket of %d", n, p.points(), w.maxBatchBytes, samplesPerBucket)
				}
			}
		}
		w.requireClean()
	})

	t.Run("a 413 split and resent", func(t *testing.T) {
		w := newGateWorld(t)
		w.recv.setFaults(false, 1)
		w.lead(w.build(w.rows(base...), nil))
		w.requireDelivered(base...)
		for _, n := range base {
			refused := 0
			for _, p := range w.recv.under(w.tenant(n).credential()) {
				if p.Status == http.StatusRequestEntityTooLarge {
					refused++
				}
			}
			if refused == 0 {
				t.Errorf("%s: no payload was refused 413, so nothing was split and resent", n)
			}
		}
		w.requireClean()
	})

	t.Run("a retried send", func(t *testing.T) {
		w := newGateWorld(t)
		w.recv.setFaults(true, 0)
		w.lead(w.build(w.rows(base...), nil))
		w.requireDelivered(base...)
		for _, n := range base {
			got := w.recv.under(w.tenant(n).credential())
			if len(got) < 2 || got[0].Status != http.StatusServiceUnavailable {
				t.Errorf("%s: want a first send refused 503 and a resend, got %d requests", n, len(got))
			}
		}
		w.requireClean()
	})

	t.Run("a change of leader", func(t *testing.T) {
		w := newGateWorld(t)
		rows := w.rows(base...)
		w.lead(w.build(rows, nil))
		first := map[string]time.Time{}
		for _, n := range base {
			first[n] = w.latest(n)
		}
		// A second app on the same checkpoint store, as buildCycleApp does, five minutes later.
		w.advance(5 * time.Minute)
		w.lead(w.build(rows, nil))
		w.requireDelivered(base...)
		for _, n := range base {
			if !w.latest(n).After(first[n]) {
				t.Errorf("%s: the second leader delivered nothing newer than %s", n, first[n])
			}
		}
		w.requireClean()
	})

	t.Run("plan rows in reverse order", func(t *testing.T) {
		w := newGateWorld(t)
		w.lead(w.build(w.rows(base...), nil))
		first := map[string]time.Time{}
		for _, n := range base {
			first[n] = w.latest(n)
		}
		// A restart with the same rows supplied in reverse order.
		w.advance(5 * time.Minute)
		rev := slices.Clone(base)
		slices.Reverse(rev)
		w.lead(w.build(w.rows(rev...), nil))
		w.requireDelivered(base...)
		for _, n := range base {
			if !w.latest(n).After(first[n]) {
				t.Errorf("%s: the restart with reversed rows delivered nothing newer than %s", n, first[n])
			}
		}
		w.requireClean()
	})

	t.Run("a removed tenant whose target name is reused", func(t *testing.T) {
		w := newGateWorld(t)
		w.lead(w.build(w.rows(base...), nil))
		removed := len(w.recv.under(w.tenant("tenant-c").credential()))
		lastOfRemoved := w.latest("tenant-c")
		if w.tenant("tenant-d").Target != w.tenant("tenant-c").Target {
			t.Fatal("tenant-d must reuse tenant-c's target name")
		}
		// tenant-c is removed; tenant-d takes its target name, on the same checkpoint store.
		w.advance(5 * time.Minute)
		w.lead(w.build(w.rows("tenant-a", "tenant-b", "tenant-d"), nil))
		w.requireDelivered("tenant-a", "tenant-b", "tenant-d")
		if got := len(w.recv.under(w.tenant("tenant-c").credential())); got != removed {
			t.Errorf("the removed tenant-c received %d more requests", got-removed)
		}
		// tenant-d starts from its own empty checkpoint, not tenant-c's: it delivers buckets tenant-c had
		// already delivered.
		if e := w.earliest("tenant-d"); e.IsZero() || e.After(lastOfRemoved) {
			t.Errorf("tenant-d's oldest bucket %s is after tenant-c's newest %s: it resumed from tenant-c's checkpoint", e, lastOfRemoved)
		}
		w.requireClean()
	})
}

// faultCase is one seeded fault: how it is seeded and run, what else must hold, and the subjects each
// check must report (a check not named must report nothing).
type faultCase struct {
	name string
	run  func(t *testing.T, w *gateWorld)
	want map[string][]string
}

// TestCrossTenantSeededFaults (AC2): each seeded fault has its stated outcome. These cases are the red
// proof of the four checks: each check reports the faults it is there to catch and nothing else.
func TestCrossTenantSeededFaults(t *testing.T) {
	pa, pb, pc := "tenant-a.primary", "tenant-b.secondary", "tenant-c.spare"
	base := []string{"tenant-a", "tenant-b", "tenant-c"}
	swapKeys := func(w *gateWorld) {
		ka, kb := [2]string{"tenant-a", "api-key-primary"}, [2]string{"tenant-b", "api-key-secondary"}
		w.secrets.values[ka], w.secrets.values[kb] = w.secrets.values[kb], w.secrets.values[ka]
	}
	// behindSlugStripper points a row's base URL at a proxy that strips workspace_slug in front of the
	// vendor, keeping the row's own path so the vendor still knows which row sent each request.
	behindSlugStripper := func(t *testing.T, w *gateWorld, row *tenancy.Row) {
		proxy := newSlugStripProxy(t, w.vendor.URL)
		row.Source.BaseURL = proxy.URL + "/r/" + row.Pipeline
	}
	cases := []faultCase{
		{
			name: "swapped emitters",
			run: func(t *testing.T, w *gateWorld) {
				w.lead(w.build(w.rows(base...), swapEmitters(pa, pb)))
				w.requireDelivered(base...)
			},
			want: map[string][]string{checkDelivery: {pa, pb}},
		},
		{
			name: "swapped API keys, with the gate",
			run: func(t *testing.T, w *gateWorld) {
				swapKeys(w)
				w.lead(w.build(w.rows(base...), nil))
				for _, p := range []string{pa, pb} {
					if !w.sawScopeState(p, "mismatched") {
						t.Errorf("%s: self-telemetry never reported scope_state{state=\"mismatched\"}", p)
					}
				}
				w.requireNothingUnder("tenant-a", "tenant-b")
				w.requireDelivered("tenant-c")
			},
			want: map[string][]string{},
		},
		{
			name: "swapped API keys, scope removed",
			run: func(t *testing.T, w *gateWorld) {
				swapKeys(w)
				rows := w.rows(base...)
				rows[0].Source.Scope, rows[1].Source.Scope = config.ScopeConfig{}, config.ScopeConfig{}
				w.lead(w.build(rows, nil))
				w.requireDelivered(base...)
			},
			// With no scope the two rows send no slug, so the slug check reports them too.
			want: map[string][]string{checkDelivery: {pa, pb}, checkSlug: {pa, pb}},
		},
		{
			name: "a vendor answering with another scope's marker",
			run: func(t *testing.T, w *gateWorld) {
				w.vendor.answerWithForeignMarker(w.tenant("tenant-a").Key, w.tenant("tenant-b").Workspace)
				w.lead(w.build(w.rows(base...), nil))
				if !w.sawScopeState(pa, "matched") {
					t.Errorf("%s: the workspace grouping is true, so the gate must match", pa)
				}
				w.requireDelivered(base...)
			},
			want: map[string][]string{checkDelivery: {pa}},
		},
		{
			name: "a missing slug, with the gate",
			run: func(t *testing.T, w *gateWorld) {
				rows := w.rows(base...)
				behindSlugStripper(t, w, &rows[2])
				w.lead(w.build(rows, nil))
				if !w.sawScopeState(pc, "mismatched") {
					t.Errorf("%s: the gate saw the organisation key's default workspace and must mismatch", pc)
				}
				w.requireNothingUnder("tenant-c")
				w.requireDelivered("tenant-a", "tenant-b")
			},
			want: map[string][]string{checkSlug: {pc}},
		},
		{
			name: "a missing slug, scope removed",
			run: func(t *testing.T, w *gateWorld) {
				rows := w.rows(base...)
				behindSlugStripper(t, w, &rows[2])
				rows[2].Source.Scope = config.ScopeConfig{}
				w.lead(w.build(rows, nil))
				w.requireDelivered(base...)
			},
			want: map[string][]string{checkSlug: {pc}, checkDelivery: {pc}},
		},
		{
			name: "a row given the self destination and credential",
			run: func(t *testing.T, w *gateWorld) {
				rows := w.rows(base...)
				rows[2].Destination.InstanceID, rows[2].TokenFile, rows[2].Destination.Token = w.selfInstance, "", w.selfToken
				w.lead(w.build(rows, nil))
				w.requireNothingUnder("tenant-c")
				w.requireDelivered("tenant-a", "tenant-b")
			},
			want: map[string][]string{checkSelfEndpoint: {pc}},
		},
		{
			// Design §5.5 (unit 6b-2): tenant-b's key file holds tenant-a's key. Both tenants are left unbuilt,
			// so nothing is sent under either credential or to the vendor for either; both are reported
			// duplicate_key, on OnTenantBlocked and on the self-telemetry's tenant_blocked; tenant-c delivers.
			// With the duplicate check skipped, tenant-a builds and delivers under its credential.
			name: "a duplicate API key in two tenants",
			run: func(t *testing.T, w *gateWorld) {
				w.secrets.values[[2]string{"tenant-b", "api-key-secondary"}] = w.tenant("tenant-a").Key
				w.lead(w.build(w.rows(base...), nil))
				w.requireNothingUnder("tenant-a", "tenant-b")
				w.requireDelivered("tenant-c")
				for _, r := range w.vendor.seen() {
					if r.Row != pc {
						t.Errorf("the vendor saw a request from %s, a blocked tenant's row", r.Row)
					}
				}
				for _, n := range []string{"tenant-a", "tenant-b"} {
					if !w.blockedFor(n, "duplicate_key") {
						t.Errorf("%s: OnTenantBlocked never reported duplicate_key", n)
					}
					if !w.sawTenantBlocked(n, "duplicate_key") {
						t.Errorf("%s: self-telemetry never reported tenant_blocked{reason=\"duplicate_key\"} 1", n)
					}
				}
				for _, reason := range []string{"duplicate_key", "secret_unreadable"} {
					if w.blockedFor("tenant-c", reason) || w.sawTenantBlocked("tenant-c", reason) {
						t.Errorf("tenant-c was reported blocked for %s", reason)
					}
				}
			},
			want: map[string][]string{},
		},
		{
			name: "a row given the self identity with its own credential",
			run: func(t *testing.T, w *gateWorld) {
				rows := w.rows(base...)
				rows[2].Identity = config.IdentityConfig{ServiceNamespace: w.selfNamespace, DeploymentEnvironment: w.cfg.Identity.DeploymentEnvironment}.ProductIdentity()
				w.lead(w.build(rows, nil))
				w.requireDelivered(base...)
			},
			want: map[string][]string{checkSelfIdentity: {pc}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newGateWorld(t)
			tc.run(t, w)
			found := w.checks()
			for _, c := range gateChecks {
				want := slices.Clone(tc.want[c])
				sort.Strings(want)
				if got := subjects(found[c]); !slices.Equal(got, want) {
					t.Errorf("the %s check reported %v, want %v; its findings: %v", c, got, want, found[c])
				}
			}
		})
	}
}

// fixtureGateTenants are the targets of config/testdata/tenants.yaml as gate tenants: the scope names and
// ids, instance ids and file names are the fixture's; the keys, tokens and markers are the gate's. team-a's
// two targets send to its one destination, so they share one credential.
func fixtureGateTenants() ([]gateTenant, map[[2]string]string) {
	tenants := []gateTenant{
		{Tenant: "team-a", Target: "prod", Workspace: "example-scope-a1", ScopeID: "example-scope-id-a1", Key: "fixture-key-a1", InstanceID: "200001", Token: "fixture-token-a", Marker: 5551},
		{Tenant: "team-a", Target: "staging", Workspace: "example-scope-a2", ScopeID: "example-scope-id-a2", Key: "fixture-key-a2", InstanceID: "200001", Token: "fixture-token-a", Marker: 5552},
		{Tenant: "team-b", Target: "prod", Workspace: "example-scope-b1", ScopeID: "example-scope-id-b1", Key: "fixture-key-b1", InstanceID: "200002", Token: "fixture-token-b", Marker: 5553},
	}
	files := map[[2]string]string{
		{"team-a", "otlp-token"}: "fixture-token-a", {"team-a", "prod-key"}: "fixture-key-a1", {"team-a", "staging-key"}: "fixture-key-a2",
		{"team-b", "otlp-token"}: "fixture-token-b", {"team-b", "source-key"}: "fixture-key-b1",
	}
	return tenants, files
}

// TestCrossTenantFromFixture (AC7, unit 6b-2): the gate passes on plans that come from the tenant fixture
// through config.Load and tenancy.Resolve, not from hand-made rows: each row's base URL and endpoint are
// rewritten to the fakes after Resolve (with private addresses allowed, the fakes being local), and
// nothing else. The loops are the fixture's defaults (a ten-minute settle, so the vendor serves twenty
// minutes of buckets). Every pipeline delivers, all four checks hold, and the fixture and its copy with the
// tenants in another order give one plan hash and the same deliveries.
func TestCrossTenantFromFixture(t *testing.T) {
	tenants, files := fixtureGateTenants()
	hashes := map[string]string{}
	deliveries := map[string]map[string]int{}
	fixtures := []string{"tenants.yaml", "tenants-reordered.yaml"}
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			w := newGateWorldWith(t, tenants, 20)
			w.secrets.values = maps.Clone(files)
			cfg, plan := loadTenantFixture(t, "../config/testdata/"+fixture)
			hashes[fixture] = plan.Hash()
			if len(plan.Rows) != len(tenants) {
				t.Fatalf("%s resolved to %d rows, want %d", fixture, len(plan.Rows), len(tenants))
			}
			for i := range plan.Rows {
				r := &plan.Rows[i]
				r.Destination.Endpoint = w.recv.URL
				r.Source.BaseURL = w.vendor.URL + "/r/" + r.Pipeline
				r.Source.HTTP.AllowPrivate = true
			}
			w.lead(w.buildPlan(cfg, plan, nil))
			got := map[string]int{}
			for _, g := range tenants {
				pts := w.deliveredTo(g)
				if len(pts) == 0 {
					t.Errorf("%s: nothing was delivered", g.pipeline())
				}
				if !w.sawScopeState(g.pipeline(), "matched") {
					t.Errorf("%s: self-telemetry never reported scope_state{state=\"matched\"}", g.pipeline())
				}
				got[g.pipeline()] = len(pts)
			}
			deliveries[fixture] = got
			w.requireClean()
		})
	}
	if hashes[fixtures[0]] == "" || hashes[fixtures[0]] != hashes[fixtures[1]] {
		t.Errorf("plan hashes differ by tenant order: %v", hashes)
	}
	if !maps.Equal(deliveries[fixtures[0]], deliveries[fixtures[1]]) {
		t.Errorf("deliveries differ by tenant order: %v", deliveries)
	}
}
