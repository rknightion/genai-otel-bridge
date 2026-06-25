// SPDX-License-Identifier: AGPL-3.0-only

package cleanup

import (
	"context"
	"fmt"
	"testing"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNS    = "decant"
	testLease = "decant-leader"
	testCM    = "decant-checkpoints"
)

func newLease(ns, name string) *coordv1.Lease {
	return &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func newCM(ns, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func leaseExists(t *testing.T, cs *fake.Clientset, ns, name string) bool {
	t.Helper()
	_, err := cs.CoordinationV1().Leases(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("lease get: %v", err)
	}
	return err == nil
}

func cmExists(t *testing.T, cs *fake.Clientset, ns, name string) bool {
	t.Helper()
	_, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("cm get: %v", err)
	}
	return err == nil
}

// Default cleanup removes BOTH the lease and the checkpoint configmap.
func TestRunDeletesLeaseAndCheckpoint(t *testing.T) {
	cs := fake.NewSimpleClientset(newLease(testNS, testLease), newCM(testNS, testCM))
	if err := Run(context.Background(), cs, testNS, testLease, testCM, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if leaseExists(t, cs, testNS, testLease) {
		t.Error("lease should be deleted")
	}
	if cmExists(t, cs, testNS, testCM) {
		t.Error("checkpoint configmap should be deleted")
	}
}

// retainCheckpoint=true keeps the durable watermark but still drops the ephemeral lease.
func TestRunRetainCheckpointKeepsConfigMap(t *testing.T) {
	cs := fake.NewSimpleClientset(newLease(testNS, testLease), newCM(testNS, testCM))
	if err := Run(context.Background(), cs, testNS, testLease, testCM, true); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if leaseExists(t, cs, testNS, testLease) {
		t.Error("lease should always be deleted (ephemeral coordination state)")
	}
	if !cmExists(t, cs, testNS, testCM) {
		t.Error("checkpoint configmap should be retained when retainCheckpoint=true")
	}
}

// Missing objects are not an error — uninstall must be idempotent (the app may never have become
// leader, or a prior cleanup already ran).
func TestRunIdempotentWhenAbsent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if err := Run(context.Background(), cs, testNS, testLease, testCM, false); err != nil {
		t.Fatalf("Run on empty cluster should be a no-op, got: %v", err)
	}
}

// A genuine API failure (not NotFound) must propagate, so a failed uninstall hook is visible rather
// than silently leaving orphans.
func TestRunPropagatesUnexpectedError(t *testing.T) {
	cs := fake.NewSimpleClientset(newLease(testNS, testLease), newCM(testNS, testCM))
	cs.PrependReactor("delete", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("boom"))
	})
	if err := Run(context.Background(), cs, testNS, testLease, testCM, false); err == nil {
		t.Fatal("expected a non-NotFound delete error to propagate")
	}
}
