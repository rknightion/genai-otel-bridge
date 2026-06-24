// SPDX-License-Identifier: AGPL-3.0-only

//go:build envtest

// Package integration holds Layer-1 tests that run against a REAL kube-apiserver + etcd via envtest
// (no kubelet, no cluster). They exercise lease election + ConfigMap RMW under genuine
// optimistic-concurrency semantics that the fake clientset does not enforce. The ONLY import of
// sigs.k8s.io/controller-runtime in this repo lives behind this `envtest` build tag, so it never
// links into the production binary. Run via `make ci-envtest`.
package integration

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lease "github.com/grafana-ps/aip-oi/internal/coordinate/lease"
)

// startEnv boots an envtest apiserver+etcd and returns a clientset + a created namespace.
func startEnv(t *testing.T) (*kubernetes.Clientset, string) {
	t.Helper()
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start (is KUBEBUILDER_ASSETS set? run via `make ci-envtest`): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	const ns = "aip-oi-test"
	_, err = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ns: %v", err)
	}
	return cs, ns
}

// Invariant 1 under a REAL apiserver: two coordinators contending for one Lease never both lead.
func TestRealApiserverSingleLeader(t *testing.T) {
	cs, ns := startEnv(t)
	var active, maxActive int32
	lead := func(id string) context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		c := lease.New(cs, ns, "aip-oi-leader", id, 4*time.Second, 3*time.Second, 1*time.Second)
		go func() {
			_ = c.Run(ctx, func(leaderCtx context.Context) {
				n := atomic.AddInt32(&active, 1)
				for {
					for {
						m := atomic.LoadInt32(&maxActive)
						if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
							break
						}
					}
					select {
					case <-leaderCtx.Done():
						atomic.AddInt32(&active, -1)
						return
					case <-time.After(200 * time.Millisecond):
						n = atomic.LoadInt32(&active)
					}
				}
			})
		}()
		return cancel
	}
	c1 := lead("pod-1")
	defer c1()
	c2 := lead("pod-2")
	defer c2()

	// Run for several lease durations; the peak number of simultaneously-active leaders must be 1.
	time.Sleep(12 * time.Second)
	if m := atomic.LoadInt32(&maxActive); m != 1 {
		t.Fatalf("expected exactly one active leader at peak, got %d", m)
	}
}
