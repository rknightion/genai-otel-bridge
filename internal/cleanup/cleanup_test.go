// SPDX-License-Identifier: AGPL-3.0-only

package cleanup

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/configmap"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

const (
	testNS    = "genai-otel-bridge"
	testLease = "genai-otel-bridge-leader"
	testCM    = "genai-otel-bridge-checkpoints"
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

// retainCheckpoint=true keeps BOTH durable halves of the HA state — the checkpoint watermark AND the
// Lease that carries its epoch fence (LeaseTransitions). Dropping the Lease would reset the epoch to 0
// and permanently fence the retained watermark on reinstall (#33), so it must survive too.
func TestRunRetainCheckpointKeepsConfigMapAndLease(t *testing.T) {
	cs := fake.NewSimpleClientset(newLease(testNS, testLease), newCM(testNS, testCM))
	if err := Run(context.Background(), cs, testNS, testLease, testCM, true); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !leaseExists(t, cs, testNS, testLease) {
		t.Error("lease must be retained alongside the checkpoint (it carries the durable epoch fence, #33)")
	}
	if !cmExists(t, cs, testNS, testCM) {
		t.Error("checkpoint configmap should be retained when retainCheckpoint=true")
	}
}

// The #33 acceptance scenario end-to-end over the real configmap store: install → a leader at epoch N≥1
// saves a watermark → uninstall with retainCheckpoint → reinstall (reads the retained Lease's epoch) →
// the FIRST commit ADVANCES the watermark instead of being fenced. The guard assertion shows the bug:
// had the Lease been deleted, the reinstall's fresh epoch 0 would be fenced against the retained epoch N.
func TestRetainCheckpointResumesWithoutFenceStorm(t *testing.T) {
	ctx := context.Background()
	transitions := int32(2) // a prior install with ≥1 leadership transition (any rollout/restart)
	lease := &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testLease, Namespace: testNS},
		Spec:       coordv1.LeaseSpec{LeaseTransitions: &transitions},
	}
	cs := fake.NewSimpleClientset(lease)
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}

	// Install: leader at epoch 2 persists a watermark.
	store := configmap.New(cs, testNS, testCM)
	w1 := model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 2}
	if err := store.Save(ctx, key, w1); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	// Uninstall with retainCheckpoint=true.
	if err := Run(ctx, cs, testNS, testLease, testCM, true); err != nil {
		t.Fatalf("cleanup(retain): %v", err)
	}
	if !cmExists(t, cs, testNS, testCM) || !leaseExists(t, cs, testNS, testLease) {
		t.Fatal("both checkpoint and lease must survive a retain cleanup")
	}

	// Guard: a DELETED lease would give a fresh reinstall epoch 0, which is fenced against the retained
	// checkpoint at epoch 2 — the exact stall the fix prevents.
	if err := checkpoint.CheckMonotonic(w1, model.Watermark{Time: time.Unix(2000, 0).UTC(), Epoch: 0}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("guard: fresh-lease epoch 0 must be fenced against the retained checkpoint, got %v", err)
	}

	// Reinstall: the new leader reads the retained Lease's LeaseTransitions as its epoch. Because the
	// Lease survived, that epoch (≥ the stored epoch) makes the first commit advance, not fence.
	got, err := cs.CoordinationV1().Leases(testNS).Get(ctx, testLease, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read retained lease: %v", err)
	}
	electedEpoch := int64(*got.Spec.LeaseTransitions) // == 2, ≥ stored 2 (and only ever increases)
	resume := model.Watermark{Time: time.Unix(2000, 0).UTC(), Epoch: electedEpoch}
	store2 := configmap.New(cs, testNS, testCM)
	if err := store2.Save(ctx, key, resume); err != nil {
		t.Fatalf("first commit after retain-reinstall must ADVANCE (no fence storm), got %v", err)
	}
	if loaded, _ := store2.Load(ctx, key); !loaded.Time.Equal(resume.Time) {
		t.Fatalf("watermark did not advance after resume: %+v", loaded)
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
