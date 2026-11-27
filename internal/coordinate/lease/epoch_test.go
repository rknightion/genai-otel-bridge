// SPDX-License-Identifier: Apache-2.0

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

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// leaseWith returns a Lease carrying the given LeaseTransitions value.
func leaseWith(ns, name string, transitions int32) *coordv1.Lease {
	return &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       coordv1.LeaseSpec{LeaseTransitions: &transitions},
	}
}

// A transient Lease Get error must be retried, not turned into epoch 0 (which would force the
// runner's forward-write fence — round3-#3). After the blip clears, the real epoch
// (LeaseTransitions + 1, #375) is read.
func TestEpochRetriesTransientGetError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var calls int32
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return true, nil, errors.New("transient API blip")
		}
		return true, leaseWith("genai-otel-bridge", "genai-otel-bridge-leader", 7), nil
	})
	c := New(cs, "genai-otel-bridge", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	got := c.epoch(context.Background())

	if got != 8 {
		t.Fatalf("epoch = %d, want 8 (transient Get errors must be retried past, not fenced)", got)
	}
	if n := atomic.LoadInt32(&calls); n < 3 {
		t.Fatalf("epoch made %d Get calls, expected it to retry past the transient errors", n)
	}
}

// [#376] A sustained Lease Get failure no longer gives up after a fixed budget and guesses epoch 0
// (the decision this reverses: a leader at epoch 0 is fenced for its whole term while it blocks every
// standby). The read keeps retrying until the context is done, and only then returns 0.
func TestEpochRetriesUntilContextDone(t *testing.T) {
	const wantCalls = 6 // past the old ~800ms retry budget
	// The timeout only bounds a broken retry loop. The failed reads drive cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs := fake.NewSimpleClientset()
	var calls int32
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		if atomic.AddInt32(&calls, 1) == wantCalls {
			cancel()
		}
		return true, nil, errors.New("sustained API outage")
	})
	c := New(cs, "genai-otel-bridge", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	if got := c.epoch(ctx); got != 0 {
		t.Fatalf("epoch = %d, want 0 once the context is done", got)
	}
	if err := ctx.Err(); err != context.Canceled {
		t.Fatalf("epoch returned after %d Get calls with context error %v; want cancellation on failed Get %d",
			atomic.LoadInt32(&calls), err, wantCalls)
	}
}

// nil LeaseTransitions (no transitions recorded) is a legitimate epoch 1 (#375), not a transient
// error — it must be returned immediately without retrying.
func TestEpochNilTransitionsIsOneWithoutRetry(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var calls int32
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		atomic.AddInt32(&calls, 1)
		return true, &coordv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "genai-otel-bridge-leader", Namespace: "genai-otel-bridge"},
			Spec:       coordv1.LeaseSpec{LeaseTransitions: nil},
		}, nil
	})
	c := New(cs, "genai-otel-bridge", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	if got := c.epoch(context.Background()); got != 1 {
		t.Fatalf("epoch = %d, want 1 for nil LeaseTransitions", got)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("epoch made %d Get calls for nil transitions, want exactly 1 (no retry — it is not an error)", n)
	}
}

// A cancelled context during the inter-attempt backoff must abort the retry promptly and return 0
// (the caller's onElected then runs under the cancelled context and exits).
func TestEpochAbortsRetryOnContextCancel(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("transient API blip")
	})
	c := New(cs, "genai-otel-bridge", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan int64, 1)
	go func() { done <- c.epoch(ctx) }()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("epoch = %d, want 0 on cancelled context", got)
		}
	case <-time.After(5 * epochReadBackoff):
		t.Fatal("epoch did not abort promptly on context cancellation")
	}
}

// --- #375 (lease-epoch-baseline) and #376 (lease-epoch-read-failure) ---

// TestNoneToLeaseMigrationIsNotFenced: a single-replica deployment (coordinator none) writes its
// watermarks at the no-op coordinator's epoch (>= 1). When the operator enables HA, the first leader
// on a freshly created Lease must be able to write over that checkpoint. If the lease epoch were the
// raw LeaseTransitions count (0 on a fresh Lease), CheckMonotonic would reject every write as stale
// and the loop would re-emit the same window every cadence. [#375 AC1]
func TestNoneToLeaseMigrationIsNotFenced(t *testing.T) {
	var noopEpoch int64
	nctx, ncancel := context.WithCancel(context.Background())
	ncancel()
	_ = coordinate.Noop{}.Run(nctx, func(lc context.Context) { noopEpoch = coordinate.EpochFromContext(lc) })
	stored := model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: noopEpoch}

	cs := fake.NewSimpleClientset() // no Lease yet: this is the first leader
	c := New(cs, "ns", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	elected := make(chan int64, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = c.Run(ctx, func(lc context.Context) { elected <- coordinate.EpochFromContext(lc); <-lc.Done() })
	}()
	var leaseEpoch int64
	select {
	case leaseEpoch = <-elected:
	case <-time.After(5 * time.Second):
		t.Fatal("never elected")
	}
	cancel()
	<-runDone

	if leaseEpoch != 1 {
		t.Errorf("first leader on a fresh Lease runs at epoch %d, want 1 (no-op stored epoch %d)", leaseEpoch, noopEpoch)
	}
	next := model.Watermark{Time: stored.Time.Add(time.Minute), Epoch: leaseEpoch}
	if err := checkpoint.CheckMonotonic(stored, next); err != nil {
		t.Fatalf("the new lease leader's forward write is fenced: %v", err)
	}
}

// TestEpochIsLeaseTransitionsPlusOne: a Lease with N transitions yields epoch N+1, and a nil count
// (never written by client-go, which always sets it, but legal in the API) yields 1. [#375 AC2]
func TestEpochIsLeaseTransitionsPlusOne(t *testing.T) {
	for _, tc := range []struct {
		name        string
		transitions *int32
		want        int64
	}{
		{"nil", nil, 1},
		{"zero", ptr(0), 1},
		{"one", ptr(1), 2},
		{"seven", ptr(7), 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := &coordv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: "genai-otel-bridge-leader", Namespace: "ns"},
				Spec:       coordv1.LeaseSpec{LeaseTransitions: tc.transitions},
			}
			c := New(fake.NewSimpleClientset(lease), "ns", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)
			if got := c.epoch(context.Background()); got != tc.want {
				t.Fatalf("epoch = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOldNumberingCheckpointStaysWritable: a checkpoint written by the previous release stored the raw
// LeaseTransitions count N as its epoch. The new numbering is a monotonic upward shift, so a new-release
// leader on that same Lease (N transitions, or N+1 after it takes over) is never below N. In a mixed-version
// rollout an old-release replica that takes over from a new-release leader bumps the count to N+1 and so
// writes at N+1, equal to what the new release stored: equal epochs with a later time are accepted.
// [#375 AC3]
func TestOldNumberingCheckpointStaysWritable(t *testing.T) {
	const n = 3
	t0 := time.Unix(1000, 0).UTC()
	oldStored := model.Watermark{Time: t0, Epoch: n} // previous release: epoch == LeaseTransitions

	c := New(fake.NewSimpleClientset(leaseWith("ns", "genai-otel-bridge-leader", n)), "ns", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)
	newEpoch := c.epoch(context.Background())
	newWrite := model.Watermark{Time: t0.Add(time.Minute), Epoch: newEpoch}
	if err := checkpoint.CheckMonotonic(oldStored, newWrite); err != nil {
		t.Fatalf("new release cannot write over the old release's checkpoint: %v", err)
	}

	// Mixed-version rollout: an old-release replica takes the Lease from the new-release leader, so
	// LeaseTransitions becomes n+1 and the old release writes at n+1.
	oldTakeover := model.Watermark{Time: t0.Add(2 * time.Minute), Epoch: n + 1}
	if err := checkpoint.CheckMonotonic(newWrite, oldTakeover); err != nil {
		t.Fatalf("an old-release leader taking over in a mixed rollout is fenced: %v", err)
	}
}

// TestEpochReadRetriesUntilSuccess: once elected, a Lease Get that fails ten times (longer than the
// old bounded budget of about 800ms) and then succeeds must still stamp the real epoch on the leader
// context, rather than giving up and starting the scheduler at epoch 0. [#376 AC1]
func TestEpochReadRetriesUntilSuccess(t *testing.T) {
	// An unheld Lease that has seen 4 transitions: acquiring it makes 5, so the real epoch is 6.
	lease := &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "genai-otel-bridge-leader", Namespace: "ns"},
		Spec:       coordv1.LeaseSpec{LeaseTransitions: ptr(4)},
	}
	cs := fake.NewSimpleClientset(lease)
	var acquired atomic.Bool
	var failures int32
	cs.PrependReactor("update", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		acquired.Store(true)
		return false, nil, nil // fall through to the tracker
	})
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		if acquired.Load() && atomic.LoadInt32(&failures) < 10 {
			atomic.AddInt32(&failures, 1)
			return true, nil, errors.New("API blip at election")
		}
		return false, nil, nil
	})
	c := New(cs, "ns", "genai-otel-bridge-leader", "replica-a", 5*time.Second, 4*time.Second, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	elected := make(chan int64, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = c.Run(ctx, func(lc context.Context) { elected <- coordinate.EpochFromContext(lc); <-lc.Done() })
	}()
	var got int64
	select {
	case got = <-elected:
	case <-time.After(10 * time.Second):
		t.Fatal("never elected")
	}
	cancel()
	<-runDone

	if got != 6 {
		t.Fatalf("leader context epoch = %d after %d failed reads, want 6 (the real epoch, read once the Get recovers)", got, atomic.LoadInt32(&failures))
	}
	if n := atomic.LoadInt32(&failures); n != 10 {
		t.Fatalf("saw %d failed epoch reads, want 10", n)
	}
}

// TestEpochReadFailureHoldsSchedulerUntilCancel: when the epoch can never be read, the scheduler must
// not start at a guessed epoch. Cancelling the context returns promptly and onElected observes an
// already-cancelled context, so it exits without collecting. [#376 AC2]
func TestEpochReadFailureHoldsSchedulerUntilCancel(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var acquired atomic.Bool
	var failures int32
	cs.PrependReactor("create", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		acquired.Store(true)
		return false, nil, nil
	})
	cs.PrependReactor("get", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		if acquired.Load() {
			atomic.AddInt32(&failures, 1)
			return true, nil, errors.New("sustained API outage")
		}
		return false, nil, nil
	})
	c := New(cs, "ns", "genai-otel-bridge-leader", "replica-a", 5*time.Second, 4*time.Second, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type entry struct {
		at  time.Time
		err error
	}
	entered := make(chan entry, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = c.Run(ctx, func(lc context.Context) { entered <- entry{time.Now(), lc.Err()} })
	}()

	// Wait well past the old ~800ms budget: eight failed reads, or onElected starting early.
	deadline := time.After(10 * time.Second)
wait:
	for atomic.LoadInt32(&failures) < 8 {
		select {
		case e := <-entered:
			t.Fatalf("onElected started after %d failed epoch reads with ctx err %v; it must wait for the epoch", atomic.LoadInt32(&failures), e.err)
		case <-deadline:
			break wait
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancelled := time.Now()
	cancel()

	select {
	case e := <-entered:
		if e.err == nil {
			t.Fatal("onElected observed a live context; want it cancelled after a failed epoch read")
		}
		if d := e.at.Sub(cancelled); d > time.Second {
			t.Fatalf("onElected ran %v after cancel; want prompt", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("epoch read did not return after the context was cancelled")
	}
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func ptr(v int32) *int32 { return &v }
