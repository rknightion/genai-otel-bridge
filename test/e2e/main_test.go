// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

// Package e2e is the k3d 3-node failover suite. It deploys the real Helm chart (lease+ConfigMap HA,
// mock-Portkey source, loopback OTLP sink sidecar) via scripts/k3d-e2e.sh, then proves the three HA
// invariants by observing the Lease `genai-otel-bridge-leader` and the checkpoint ConfigMap `genai-otel-bridge-checkpoints`
// (both via client-go). Timing budgets respect ReleaseOnCancel=false (followup.md §8): failover is
// gated by LeaseDuration(15s)+RetryPeriod(2s), so budgets are >= 30s — never sub-LeaseDuration.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	leaseName = "genai-otel-bridge-leader"
	cpName    = "genai-otel-bridge-checkpoints"
)

func ns() string {
	if v := os.Getenv("E2E_NAMESPACE"); v != "" {
		return v
	}
	return "genai-otel-bridge-e2e"
}

func kubectlBin() string {
	if v := os.Getenv("KUBECTL"); v != "" {
		return v
	}
	return "kubectl"
}

func client(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("kubeconfig (set KUBECONFIG; run via `make k3d-e2e`): %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	return cs
}

func getLease(t *testing.T, cs *kubernetes.Clientset) *coordv1.Lease {
	t.Helper()
	l, err := cs.CoordinationV1().Leases(ns()).Get(context.Background(), leaseName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return l
}

func holder(l *coordv1.Lease) string {
	if l == nil || l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

func transitions(l *coordv1.Lease) int32 {
	if l == nil || l.Spec.LeaseTransitions == nil {
		return 0
	}
	return *l.Spec.LeaseTransitions
}

// latestWatermark reads the checkpoint ConfigMap and returns the MAX watermark time across its data
// values (each value is the JSON record {"time","cursor","epoch"} written by internal/checkpoint/
// configmap). Returns ok=false until the first commit creates the ConfigMap.
func latestWatermark(t *testing.T, cs *kubernetes.Clientset) (time.Time, bool) {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns()).Get(context.Background(), cpName, metav1.GetOptions{})
	if err != nil {
		return time.Time{}, false
	}
	var max time.Time
	var any bool
	for _, raw := range cm.Data {
		var r struct {
			Time time.Time `json:"time"`
		}
		if json.Unmarshal([]byte(raw), &r) != nil {
			continue
		}
		any = true
		if r.Time.After(max) {
			max = r.Time
		}
	}
	return max, any
}

func runningPods(t *testing.T, cs *kubernetes.Clientset) []corev1.Pod {
	t.Helper()
	l, err := cs.CoreV1().Pods(ns()).List(context.Background(), metav1.ListOptions{LabelSelector: "app=genai-otel-bridge"})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	var out []corev1.Pod
	for _, p := range l.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out
}

// waitSteady waits for the deployment to settle: 3 Running integrator pods and an elected leader.
// Used at the start of each mutating test so prior disruption (a deleted/frozen pod) does not leak.
func waitSteady(t *testing.T, cs *kubernetes.Clientset) {
	t.Helper()
	eventually(t, 150*time.Second, func() bool {
		return len(runningPods(t, cs)) >= 3 && holder(getLease(t, cs)) != ""
	}, "deployment never reached 3 running pods + an elected leader")
}

// freezeLeader SIGSTOPs the integrator process in pod `name` via a `kubectl debug` ephemeral container.
// The pod sets shareProcessNamespace=true (e2e values), so the integrator is NOT PID 1 and CAN be
// signalled from a peer container in the shared PID namespace (PID 1 is unsignalable from within its
// own namespace). The debug container runs as root, so it can signal the UID-65532 process. A freeze —
// not a NetworkPolicy partition — is used because k3d's flannel CNI does not enforce NetworkPolicy.
func freezeLeader(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command(kubectlBin(), "debug", "-n", ns(), "pod/"+name,
		"--image=busybox:1.36", "--attach=false", "--",
		"sh", "-c", "kill -STOP $(pidof genai-otel-bridge); sleep 600")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl debug freeze failed: %v\n%s", err, out)
	}
}

// resumeLeader SIGCONTs the previously-frozen integrator in pod `name` via a SECOND `kubectl debug`
// ephemeral container, waking the stale ex-leader so it wakes with its in-flight batch and attempts a
// forward commit against the newer epoch — exercising the real-cluster stale-writer fence path (#138).
// Same shared-PID-namespace mechanism as freezeLeader (the debug container is root, the integrator is
// UID-65532 and not PID 1). Must be called PROMPTLY after takeover: after the ~60s liveness restart the
// process comes back fresh as a standby and never fences.
func resumeLeader(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command(kubectlBin(), "debug", "-n", ns(), "pod/"+name,
		"--image=busybox:1.36", "--attach=false", "--",
		"sh", "-c", "kill -CONT $(pidof genai-otel-bridge)")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl debug resume failed: %v\n%s", err, out)
	}
}

// podLogsContains reports whether pod `name`'s current logs contain substr. Used to detect the fence
// evidence — the `checkpoint forward-write fenced` slog.Warn a resumed stale leader emits when its
// stale-epoch forward Save is rejected (internal/schedule/runner.go). A transient log-fetch error is
// treated as "not yet present" so the caller can keep polling.
func podLogsContains(t *testing.T, cs *kubernetes.Clientset, name, substr string) bool {
	t.Helper()
	raw, err := cs.CoreV1().Pods(ns()).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(context.Background())
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), substr)
}

func eventually(t *testing.T, budget time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("eventually failed after %s: %s", budget, msg)
}

func consistently(t *testing.T, dur time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("consistently failed: %s", msg)
		}
		time.Sleep(2 * time.Second)
	}
}
