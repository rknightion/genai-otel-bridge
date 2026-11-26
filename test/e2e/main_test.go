// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package e2e is the k3d 3-node failover suite. It deploys the real Helm chart (lease+ConfigMap HA,
// mock-Portkey source, loopback OTLP sink sidecar) via scripts/k3d-e2e.sh, then proves the three HA
// invariants by observing the Lease `genai-otel-bridge-leader`, the checkpoint ConfigMap `genai-otel-bridge-checkpoints`
// (both via client-go) and the points each pod's OTLP sink recorded (read through the API server's pod
// proxy). Timing budgets respect ReleaseOnCancel=false (followup.md §8): failover is
// gated by LeaseDuration(15s)+RetryPeriod(2s), so budgets are >= 30s — never sub-LeaseDuration.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
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

// busyboxImage is the utility image for the kubectl-debug freeze and resume. scripts/k3d-e2e.sh
// flattens and imports BUSYBOX_IMAGE and exports it; the default matches the script's.
func busyboxImage() string {
	if v := os.Getenv("BUSYBOX_IMAGE"); v != "" {
		return v
	}
	return "busybox:1.36"
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
		t.Fatalf("kubeconfig (set KUBECONFIG; run via `just e2e`): %v", err)
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

// cpRecord is one checkpoint ConfigMap value: the JSON record {"time","cursor","epoch"} written by
// internal/checkpoint/configmap. Epoch is the Lease's LeaseTransitions when the writer was elected.
type cpRecord struct {
	Time  time.Time `json:"time"`
	Epoch int64     `json:"epoch"`
}

// latestRecord reads the checkpoint ConfigMap and returns the record with the MAX watermark time
// across its data values. Returns ok=false until the first commit creates the ConfigMap.
func latestRecord(t *testing.T, cs *kubernetes.Clientset) (cpRecord, bool) {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns()).Get(context.Background(), cpName, metav1.GetOptions{})
	if err != nil {
		return cpRecord{}, false
	}
	var best cpRecord
	var any bool
	for _, raw := range cm.Data {
		var r cpRecord
		if json.Unmarshal([]byte(raw), &r) != nil {
			continue
		}
		if !any || r.Time.After(best.Time) {
			best = r
		}
		any = true
	}
	return best, any
}

// latestWatermark returns the MAX watermark time in the checkpoint ConfigMap (see latestRecord).
func latestWatermark(t *testing.T, cs *kubernetes.Clientset) (time.Time, bool) {
	t.Helper()
	r, ok := latestRecord(t, cs)
	return r.Time, ok
}

// requestsSeries is the analytics series the data checks read: unlabelled, one point per minute.
const requestsSeries = "portkey_api_requests"

// wantRequestsValue is the mock's requests value for the bucket ENDING at end. It repeats
// pointValue in test/e2e/harness/main.go (the mock stamps bucket starts; the bridge emits bucket ends).
func wantRequestsValue(end time.Time) float64 {
	return float64(end.Add(-time.Minute).Unix()/60%1000 + 1)
}

// sinkPoint is one gauge point one export delivered to a pod's OTLP sink (harness /points).
type sinkPoint struct {
	Series string    `json:"series"`
	Labels string    `json:"labels"`
	Time   time.Time `json:"time"`
	Value  float64   `json:"value"`
	Seq    int64     `json:"seq"`
}

type sinkDump struct {
	Exports   int64       `json:"exports"`
	Truncated bool        `json:"truncated"`
	BadBodies int64       `json:"bad_bodies"`
	Points    []sinkPoint `json:"points"`
}

// readSink fetches everything pod's OTLP sink has recorded, through the API server's pod proxy.
func readSink(cs *kubernetes.Clientset, pod string) (sinkDump, error) {
	raw, err := cs.CoreV1().Pods(ns()).ProxyGet("http", pod, "8090", "/points", nil).DoRaw(context.Background())
	if err != nil {
		return sinkDump{}, err
	}
	var d sinkDump
	if err := json.Unmarshal(raw, &d); err != nil {
		return sinkDump{}, fmt.Errorf("decode /points: %w", err)
	}
	if d.Truncated || d.BadBodies != 0 {
		return sinkDump{}, fmt.Errorf("sink unusable: truncated=%v bad_bodies=%d", d.Truncated, d.BadBodies)
	}
	return d, nil
}

// requestsAfter returns pod's portkey_api_requests receptions carried by exports after seq, sorted by
// bucket time (receptions of the same bucket stay adjacent, so a duplicate is visible to checkRun).
func requestsAfter(cs *kubernetes.Clientset, pod string, seq int64) ([]sinkPoint, error) {
	d, err := readSink(cs, pod)
	if err != nil {
		return nil, err
	}
	var out []sinkPoint
	for _, p := range d.Points {
		if p.Series == requestsSeries && p.Seq > seq {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// sinkMarks records how many exports each running pod's sink has absorbed, so a later read can keep
// only what that pod emitted afterwards.
func sinkMarks(t *testing.T, cs *kubernetes.Clientset) map[string]int64 {
	t.Helper()
	marks := map[string]int64{}
	for _, p := range runningPods(t, cs) {
		d, err := readSink(cs, p.Name)
		if err != nil {
			t.Fatalf("read sink of %s: %v", p.Name, err)
		}
		marks[p.Name] = d.Exports
	}
	return marks
}

// checkRun fails unless pts (one pod's requests points, sorted by time) are exactly what the mock
// served, once each: unlabelled, one minute apart with no bucket missing or repeated, each carrying
// the mock's value for its bucket.
func checkRun(t *testing.T, pod string, pts []sinkPoint) {
	t.Helper()
	for i, p := range pts {
		if p.Labels != "" {
			t.Fatalf("%s: %s point at %s has labels %q, want none", pod, requestsSeries, p.Time, p.Labels)
		}
		if want := wantRequestsValue(p.Time); p.Value != want {
			t.Fatalf("%s: %s at %s = %v, want the mock's %v", pod, requestsSeries, p.Time, p.Value, want)
		}
		if i == 0 {
			continue
		}
		switch step := p.Time.Sub(pts[i-1].Time); {
		case step == 0:
			t.Fatalf("%s: bucket %s emitted twice (seq %d and %d)", pod, p.Time, pts[i-1].Seq, p.Seq)
		case step != time.Minute:
			t.Fatalf("%s: gap: bucket %s follows %s (step %s, want 1m)", pod, p.Time, pts[i-1].Time, step)
		}
	}
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
		"--image="+busyboxImage(), "--attach=false", "--",
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
		"--image="+busyboxImage(), "--attach=false", "--",
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

// requireLeaderRun waits until leader has emitted at least n requests buckets after its sink's mark,
// checks them with checkRun, and fails if leadership moved meanwhile (the run must be one tenure).
func requireLeaderRun(t *testing.T, cs *kubernetes.Clientset, leader string, trans int32, mark int64, n int) {
	t.Helper()
	var pts []sinkPoint
	var lastErr error
	eventually(t, 150*time.Second, func() bool {
		pts, lastErr = requestsAfter(cs, leader, mark)
		return lastErr == nil && len(pts) >= n
	}, fmt.Sprintf("leader %s's sink never held %d %s buckets (last error: %v)", leader, n, requestsSeries, lastErr))
	if l := getLease(t, cs); holder(l) != leader || transitions(l) != trans {
		t.Fatalf("leadership moved from %s while its emitted run was read", leader)
	}
	checkRun(t, leader, pts)
	t.Logf("leader %s emitted %d contiguous %s buckets %s..%s with the mock's values", leader, len(pts), requestsSeries, pts[0].Time, pts[len(pts)-1].Time)
}

// failoverFrontier follows the checkpoint across a failover. old is the last watermark seen written
// under the old leader's epoch; once a record with a higher epoch appears, the new leader has
// committed and old is final: the old leader was killed, so nothing writes at its epoch any more.
type failoverFrontier struct {
	oldEpoch int64
	old      time.Time
	newSeen  bool
}

func (f *failoverFrontier) observe(t *testing.T, cs *kubernetes.Clientset) {
	t.Helper()
	r, ok := latestRecord(t, cs)
	switch {
	case !ok:
	case r.Epoch == f.oldEpoch && !f.newSeen:
		if r.Time.After(f.old) {
			f.old = r.Time
		}
	case r.Epoch > f.oldEpoch:
		f.newSeen = true
	}
}

// requireResumeAtCheckpoint is the emit-once and gap-free check across a failover: the first bucket
// the new leader emits must be the first one the old checkpoint did NOT cover (the old leader emitted
// every bucket ending at or before the checkpoint), and its run must then be contiguous. A new leader
// that re-bootstrapped or rewound emits a bucket at or before the checkpoint; one that skipped ahead
// leaves a gap. Both fail here.
func requireResumeAtCheckpoint(t *testing.T, cs *kubernetes.Clientset, f *failoverFrontier, leader string, mark int64) {
	t.Helper()
	eventually(t, 120*time.Second, func() bool {
		f.observe(t, cs)
		return f.newSeen
	}, "the new leader never committed a checkpoint at a higher epoch")
	if f.old.IsZero() {
		t.Fatalf("no checkpoint under the old leader's epoch %d was observed", f.oldEpoch)
	}
	var pts []sinkPoint
	var lastErr error
	eventually(t, 150*time.Second, func() bool {
		pts, lastErr = requestsAfter(cs, leader, mark)
		return lastErr == nil && len(pts) >= 2
	}, fmt.Sprintf("new leader %s's sink never held 2 %s buckets (last error: %v)", leader, requestsSeries, lastErr))
	want := f.old.Truncate(time.Minute).Add(time.Minute) // first bucket end strictly after the checkpoint
	first := pts[0].Time
	if !first.After(f.old) {
		t.Fatalf("new leader %s re-emitted bucket %s, which the old checkpoint %s already covered (re-bootstrap or rewind)", leader, first, f.old)
	}
	if !first.Equal(want) {
		t.Fatalf("gap: new leader %s's first bucket is %s, want %s (the first after the old checkpoint %s)", leader, first, want, f.old)
	}
	checkRun(t, leader, pts)
	t.Logf("new leader %s resumed at %s, the first bucket after the old checkpoint %s; %d contiguous buckets", leader, first, f.old, len(pts))
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
