// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

// The tenant phase of the k3d suite. scripts/k3d-e2e.sh run_tenant_tests applies
// manifests/tenant-secrets.yaml, upgrades the legacy release with values-e2e-tenants.yaml (two replicas,
// four tenants, the fourth with no Secret) and runs only the tests named ^TestTenant; the legacy phase
// skips them. Each pod's loopback OTLP sink records every export's Basic-auth credential and the
// resource's pipeline attribute, and the mock Portkey adds each API key's marker to every value, so a
// point read under a tenant's credential shows both the stack it reached and the key that read it.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// releaseSelector lists the release's pods by the chart's instance label (the release name).
	releaseSelector = "app.kubernetes.io/instance=genai-otel-bridge"
	// tenantMaxPods is goal G7: the launch install is a leader and one hot standby.
	tenantMaxPods = 2
	// tenantMarkerStep repeats markerStep in test/e2e/harness/main.go.
	tenantMarkerStep = 10000
	// selfCred is emit.self's credential in values-e2e-tenants.yaml.
	selfCred = "900:e2e-self"
	// tenantBlockedSeries is the self-metric a tenant left unbuilt reports on.
	tenantBlockedSeries = "genai_otel_bridge_tenant_blocked"
)

// e2eTenant is one tenant of values-e2e-tenants.yaml: its sink credential ("<instance id>:<token>",
// the token from manifests/tenant-secrets.yaml) and its API key's marker (apiKeys in the harness).
type e2eTenant struct {
	id     string
	cred   string
	marker int
}

func (c e2eTenant) pipeline() string { return c.id + ".prod" }

// deliveringTenants have Secrets; blockedTenant has none, so nothing may ever arrive under its
// credential.
var (
	deliveringTenants = []e2eTenant{
		{"team-a", "901:e2e-token-team-a", 1},
		{"team-b", "902:e2e-token-team-b", 2},
		{"team-c", "903:e2e-token-team-c", 3},
	}
	blockedTenant = e2eTenant{"team-d", "904:e2e-token-team-d", 4}
)

// tenantPoint is one gauge point a pod's sink recorded (harness /points), with its credential and the
// resource's pipeline attribute.
type tenantPoint struct {
	Series   string    `json:"series"`
	Labels   string    `json:"labels"`
	Time     time.Time `json:"time"`
	Value    float64   `json:"value"`
	Seq      int64     `json:"seq"`
	Cred     string    `json:"cred"`
	Pipeline string    `json:"pipeline"`
}

// readSinkCred fetches the points pod's sink recorded under cred, through the API server's pod proxy.
func readSinkCred(cs *kubernetes.Clientset, pod, cred string) ([]tenantPoint, error) {
	raw, err := cs.CoreV1().Pods(ns()).ProxyGet("http", pod, "8090", "/points", map[string]string{"cred": cred}).DoRaw(context.Background())
	if err != nil {
		return nil, err
	}
	var d struct {
		Truncated bool          `json:"truncated"`
		BadBodies int64         `json:"bad_bodies"`
		Points    []tenantPoint `json:"points"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode /points: %w", err)
	}
	if d.Truncated || d.BadBodies != 0 {
		return nil, fmt.Errorf("sink of %s unusable: truncated=%v bad_bodies=%d", pod, d.Truncated, d.BadBodies)
	}
	return d.Points, nil
}

// releasePods lists the release's pods that are not being deleted and have not ended.
func releasePods(t *testing.T, cs *kubernetes.Clientset) []corev1.Pod {
	t.Helper()
	l, err := cs.CoreV1().Pods(ns()).List(context.Background(), metav1.ListOptions{LabelSelector: releaseSelector})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	var out []corev1.Pod
	for _, p := range l.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			out = append(out, p)
		}
	}
	return out
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitRolloutSettled waits until the Deployment has observed its spec and every replica is updated,
// ready and available, with no old pod left, and returns it.
func waitRolloutSettled(t *testing.T, cs *kubernetes.Clientset) *appsv1.Deployment {
	t.Helper()
	var d *appsv1.Deployment
	eventually(t, 180*time.Second, func() bool {
		var err error
		d, err = cs.AppsV1().Deployments(ns()).Get(context.Background(), "genai-otel-bridge", metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil {
			return false
		}
		want := *d.Spec.Replicas
		s := d.Status
		return s.ObservedGeneration >= d.Generation && s.Replicas == want && s.UpdatedReplicas == want &&
			s.ReadyReplicas == want && s.AvailableReplicas == want
	}, "the tenant rollout never settled")
	return d
}

// waitTenantSteady waits for every replica to be ready and one of them to hold the Lease.
func waitTenantSteady(t *testing.T, cs *kubernetes.Clientset) {
	t.Helper()
	d := waitRolloutSettled(t, cs)
	eventually(t, 150*time.Second, func() bool {
		pods := releasePods(t, cs)
		if len(pods) != int(*d.Spec.Replicas) {
			return false
		}
		h := holder(getLease(t, cs))
		held := false
		for _, p := range pods {
			if !podReady(p) {
				return false
			}
			held = held || p.Name == h
		}
		return held
	}, "the tenant install never had every replica ready and a leader among them")
}

// eventuallyWhy is eventually for a condition that says why it does not hold yet; the last reason is
// in the failure.
func eventuallyWhy(t *testing.T, budget time.Duration, cond func() (bool, string), msg string) {
	t.Helper()
	why := ""
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var ok bool
		if ok, why = cond(); ok {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("eventually failed after %s: %s (last: %s)", budget, msg, why)
}

// wantTenantValue is the mock's requests value for the bucket ending at end, read with the key whose
// marker is marker.
func wantTenantValue(end time.Time, marker int) float64 {
	return wantRequestsValue(end) + float64(marker*tenantMarkerStep)
}

// TestTenantInstallPodCount (AC3 of #536, goal G7): once the tenant rollout settles, the release runs at
// most two pods, counted by its instance label, and every one is ready.
func TestTenantInstallPodCount(t *testing.T) {
	cs := client(t)
	d := waitRolloutSettled(t, cs)
	pods := releasePods(t, cs)
	if len(pods) == 0 || len(pods) > tenantMaxPods {
		t.Fatalf("the release runs %d pods (spec replicas %d), want 1 to %d", len(pods), *d.Spec.Replicas, tenantMaxPods)
	}
	for _, p := range pods {
		if !podReady(p) {
			t.Fatalf("pod %s is not ready after the rollout settled", p.Name)
		}
	}
	t.Logf("the release runs %d pods", len(pods))
}

// TestTenantMissingSecretBlocked (AC2 of #536, the unreadable-secret case of G3): team-d has no Secret,
// so every replica reports tenant_blocked{tenant="team-d",reason="secret_unreadable"} 1 on its
// self-telemetry, which reaches the sink under the self credential, and reports 0 for the tenants it
// built. Nothing ever arrives under team-d's credential, and the other three tenants deliver.
func TestTenantMissingSecretBlocked(t *testing.T) {
	cs := client(t)
	waitTenantSteady(t, cs)
	blockedLabels := "reason=secret_unreadable,tenant=" + blockedTenant.id
	// Self-telemetry is exported once a minute (emit.self's default interval).
	eventuallyWhy(t, 180*time.Second, func() (bool, string) {
		for _, p := range releasePods(t, cs) {
			pts, err := readSinkCred(cs, p.Name, selfCred)
			if err != nil {
				return false, err.Error()
			}
			latest := map[string]tenantPoint{}
			for _, pt := range pts {
				if pt.Series == tenantBlockedSeries && pt.Seq >= latest[pt.Labels].Seq {
					latest[pt.Labels] = pt
				}
			}
			if v, ok := latest[blockedLabels]; !ok || v.Value != 1 {
				return false, fmt.Sprintf("%s: %s{%s} = %+v (present %v), want 1", p.Name, tenantBlockedSeries, blockedLabels, v, ok)
			}
			for _, c := range deliveringTenants {
				l := "reason=secret_unreadable,tenant=" + c.id
				if v, ok := latest[l]; !ok || v.Value != 0 {
					return false, fmt.Sprintf("%s: %s{%s} = %+v (present %v), want 0", p.Name, tenantBlockedSeries, l, v, ok)
				}
			}
		}
		return true, ""
	}, "tenant_blocked never showed "+blockedTenant.id+" blocked, and only it, on every replica")

	for _, c := range deliveringTenants {
		eventually(t, 150*time.Second, func() bool {
			for _, p := range releasePods(t, cs) {
				pts, err := readSinkCred(cs, p.Name, c.cred)
				if err != nil {
					continue
				}
				for _, pt := range pts {
					if pt.Series == requestsSeries && pt.Pipeline == c.pipeline() && pt.Value == wantTenantValue(pt.Time, c.marker) {
						return true
					}
				}
			}
			return false
		}, c.id+" never delivered a requests point with its own marker and pipeline")
	}
	for _, p := range releasePods(t, cs) {
		pts, err := readSinkCred(cs, p.Name, blockedTenant.cred)
		if err != nil {
			t.Fatalf("read sink of %s: %v", p.Name, err)
		}
		if len(pts) != 0 {
			t.Fatalf("%s: %d points arrived under the blocked tenant's credential, first %+v", p.Name, len(pts), pts[0])
		}
	}
}

// TestTenantDeliveryAfterFailover (AC1 of #536, goal G1): after the leader pod is deleted and a standby
// takes over, the new leader delivers for each of the three tenants with Secrets, and each tenant's
// credential, summed over every running pod's sink, holds only its own data: every point carries its
// own pipeline id and no self-metric, and every requests value carries its own API key's marker. The
// self credential holds no product telemetry, and nothing arrives under the blocked tenant's.
func TestTenantDeliveryAfterFailover(t *testing.T) {
	cs := client(t)
	waitTenantSteady(t, cs)
	l0 := getLease(t, cs)
	oldHolder, oldTrans := holder(l0), transitions(l0)

	zero := int64(0)
	if err := cs.CoreV1().Pods(ns()).Delete(context.Background(), oldHolder, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("delete leader %q: %v", oldHolder, err)
	}
	eventually(t, 60*time.Second, func() bool {
		l := getLease(t, cs)
		return holder(l) != oldHolder && holder(l) != "" && transitions(l) > oldTrans
	}, "no standby took over within the lease-expiry budget")
	newHolder := holder(getLease(t, cs))

	// A standby emits no product telemetry, so whatever the new leader's sink holds under a tenant's
	// credential was delivered after the takeover.
	for _, c := range deliveringTenants {
		eventuallyWhy(t, 180*time.Second, func() (bool, string) {
			pts, err := readSinkCred(cs, newHolder, c.cred)
			if err != nil {
				return false, err.Error()
			}
			n := 0
			for _, pt := range pts {
				if pt.Series == requestsSeries {
					n++
				}
			}
			return n >= 2, fmt.Sprintf("%d buckets", n)
		}, fmt.Sprintf("new leader %s never delivered 2 %s buckets for %s", newHolder, requestsSeries, c.id))
	}

	pods := releasePods(t, cs)
	sum := func(cred string) []tenantPoint {
		t.Helper()
		var all []tenantPoint
		for _, p := range pods {
			pts, err := readSinkCred(cs, p.Name, cred)
			if err != nil {
				t.Fatalf("read sink of %s: %v", p.Name, err)
			}
			all = append(all, pts...)
		}
		return all
	}
	for _, c := range deliveringTenants {
		pts := sum(c.cred)
		requests := 0
		for _, pt := range pts {
			if pt.Pipeline != c.pipeline() {
				t.Fatalf("%s: a point under its credential carries pipeline %q, want %q: %+v", c.id, pt.Pipeline, c.pipeline(), pt)
			}
			if strings.HasPrefix(pt.Series, "genai_otel_bridge_") {
				t.Fatalf("%s: self-telemetry reached its destination: %+v", c.id, pt)
			}
			if pt.Series != requestsSeries {
				continue
			}
			requests++
			if want := wantTenantValue(pt.Time, c.marker); pt.Value != want {
				marker := math.Round((pt.Value - wantRequestsValue(pt.Time)) / tenantMarkerStep)
				t.Fatalf("%s: %s at %s = %v, want %v: it carries marker %v, not its own %d", c.id, requestsSeries, pt.Time, pt.Value, want, marker, c.marker)
			}
		}
		t.Logf("%s: %d points over %d pods, %d requests buckets, all with pipeline %s and marker %d", c.id, len(pts), len(pods), requests, c.pipeline(), c.marker)
	}
	if pts := sum(blockedTenant.cred); len(pts) != 0 {
		t.Fatalf("%d points arrived under the blocked tenant's credential, first %+v", len(pts), pts[0])
	}
	self := sum(selfCred)
	if len(self) == 0 {
		t.Fatal("no self-telemetry reached the self credential")
	}
	for _, pt := range self {
		if pt.Pipeline != "" || strings.HasPrefix(pt.Series, "portkey_api") {
			t.Fatalf("product telemetry reached the self credential: %+v", pt)
		}
	}
}
