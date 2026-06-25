// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// leaseWith returns a Lease carrying the given LeaseTransitions value.
func leaseWith(ns, name string, transitions int32) *coordv1.Lease {
	return &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       coordv1.LeaseSpec{LeaseTransitions: &transitions},
	}
}

// A transient Lease Get error must be retried, not turned into epoch 0 (which would force the
// runner's forward-write fence — round3-#3). After the blip clears, the real LeaseTransitions
// value is read.
func TestEpochRetriesTransientGetError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var calls int32
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return true, nil, errors.New("transient API blip")
		}
		return true, leaseWith("decant", "decant-leader", 7), nil
	})
	c := New(cs, "decant", "decant-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	got := c.epoch(context.Background())

	if got != 7 {
		t.Fatalf("epoch = %d, want 7 (transient Get errors must be retried past, not fenced)", got)
	}
	if n := atomic.LoadInt32(&calls); n < 3 {
		t.Fatalf("epoch made %d Get calls, expected it to retry past the transient errors", n)
	}
}

// A sustained Lease Get failure genuinely cannot establish the epoch — it returns 0 so the runner's
// forward-write fence trips LOUDLY (never silent). The fence is the correct, honest outcome here.
func TestEpochReturnsZeroAfterRetriesExhausted(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var calls int32
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		atomic.AddInt32(&calls, 1)
		return true, nil, errors.New("sustained API outage")
	})
	c := New(cs, "decant", "decant-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	if got := c.epoch(context.Background()); got != 0 {
		t.Fatalf("epoch = %d, want 0 (a genuine read failure must fall back to the loud fence)", got)
	}
	if n := atomic.LoadInt32(&calls); n != epochReadAttempts {
		t.Fatalf("epoch made %d Get calls, want exactly %d (bounded retry budget)", n, epochReadAttempts)
	}
}

// nil LeaseTransitions (a freshly created lease, no transitions yet) is a legitimate epoch 0, not a
// transient error — it must be returned immediately without burning the retry budget.
func TestEpochNilTransitionsIsZeroWithoutRetry(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var calls int32
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		atomic.AddInt32(&calls, 1)
		return true, &coordv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "decant-leader", Namespace: "decant"},
			Spec:       coordv1.LeaseSpec{LeaseTransitions: nil},
		}, nil
	})
	c := New(cs, "decant", "decant-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	if got := c.epoch(context.Background()); got != 0 {
		t.Fatalf("epoch = %d, want 0 for nil LeaseTransitions", got)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("epoch made %d Get calls for nil transitions, want exactly 1 (no retry — it is not an error)", n)
	}
}

// A cancelled context during the inter-attempt backoff must abort the retry promptly (and trip the
// loud fence), not block for the full retry budget.
func TestEpochAbortsRetryOnContextCancel(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("transient API blip")
	})
	c := New(cs, "decant", "decant-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan int64, 1)
	go func() { done <- c.epoch(ctx) }()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("epoch = %d, want 0 on cancelled context", got)
		}
	case <-time.After(epochReadBackoff * time.Duration(epochReadAttempts)):
		t.Fatal("epoch did not abort promptly on context cancellation")
	}
}
