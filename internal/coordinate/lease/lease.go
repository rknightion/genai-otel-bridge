// SPDX-License-Identifier: AGPL-3.0-only

// Package lease implements the Coordinator with a Kubernetes Lease (client-go leaderelection).
// The Lease reduces overlap; the actual double-emit guarantee is the checkpoint fence (Cdx-C14).
package lease

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
)

type Coordinator struct {
	cs                     kubernetes.Interface
	ns, name, identity     string
	leaseDur, renew, retry time.Duration
}

func New(cs kubernetes.Interface, ns, name, identity string, leaseDur, renew, retry time.Duration) *Coordinator {
	return &Coordinator{cs: cs, ns: ns, name: name, identity: identity, leaseDur: leaseDur, renew: renew, retry: retry}
}

func (c *Coordinator) Run(ctx context.Context, onElected func(context.Context)) error {
	var elected atomic.Bool         // [round3 HIGH-1] set iff we started leading
	leadDone := make(chan struct{}) // closed when onElected (the scheduler drain) returns
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: c.name, Namespace: c.ns},
		Client:     c.cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: c.identity},
	}
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: c.leaseDur,
		RenewDeadline: c.renew,
		RetryPeriod:   c.retry,
		// ReleaseOnCancel is intentionally false: on SIGTERM we persist watermarks first and let
		// the lease expire rather than release into a standby mid-drain (F35).
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				elected.Store(true)
				slog.Info("leader elected; starting loops", "identity", c.identity)
				defer close(leadDone)
				onElected(coordinate.WithEpoch(leaderCtx, c.epoch(leaderCtx)))
			},
			// Fires only on actual leadership loss (renewal lapse), NOT on a clean ctx-cancel shutdown —
			// an unambiguous "we lost the lease" signal, distinct from a normal SIGTERM exit.
			OnStoppedLeading: func() { slog.Info("leadership lost; loops stopped", "identity", c.identity) },
		},
	})
	if err != nil {
		return err
	}
	le.Run(ctx) // blocks; on leadership loss cancels OnStartedLeading's ctx and returns.
	// [round3 HIGH-1] client-go's LeaderElector.Run launches OnStartedLeading in a goroutine it does
	// NOT join — it returns as soon as renewal stops, while our onElected (Scheduler.Run) may still be
	// in its wg.Wait() drain. Barrier on that drain before returning so app.Run cannot re-enter a new
	// election (or the process exit) while a prior leadership's emit worker is still running — which
	// would let a re-election's Reset() race the old worker over the shared LoopRunner. We only wait if
	// we actually started leading (a never-elected standby returns immediately on ctx cancel).
	//
	// [ext-review-3] `elected` is set inside the OnStartedLeading goroutine, which client-go launches
	// (go ...) but does not join — so there is a window where acquire() has succeeded (the goroutine
	// WILL run and close leadDone) but elected is still false because that goroutine has not been
	// scheduled yet. If ctx cancels in that window, Run would skip the barrier and return before the
	// scheduler drains. le.IsLeader() is set synchronously inside acquire() (before the goroutine is
	// launched) and stays true while we hold the lease (ReleaseOnCancel=false), so the union covers
	// both "goroutine already running" (elected) and "acquired, goroutine not yet scheduled"
	// (IsLeader). leadDone is always closed once the launched goroutine runs, so the wait terminates.
	if elected.Load() || le.IsLeader() {
		<-leadDone
	}
	return ctx.Err()
}

// epochReadAttempts / epochReadBackoff bound the retry on a TRANSIENT Lease Get error when reading
// the LeaseTransitions epoch fence. Hardcoded (like the lease timings in main.go) — no config knob.
// Total worst-case wait is (attempts-1)*backoff (~800ms), comfortably under the renew deadline, so a
// brief API blip at election no longer forces the runner's forward-write fence (round3-#3). A
// sustained failure still falls back to epoch 0 → the fence trips LOUDLY (never silent).
const (
	epochReadAttempts = 5
	epochReadBackoff  = 200 * time.Millisecond
)

// epoch reads the Lease's LeaseTransitions (incremented on each leadership change) as a COARSE
// monotonic fence epoch. [AR-C4] A transient GET error is retried (epochReadAttempts/Backoff); only
// a sustained failure returns 0 — we log + fall back rather than guessing, which makes the runner's
// forward-write fence LOUD (round3-#3) instead of silently fabricating an epoch. This epoch is read
// ONCE at election (not re-checked before each Save). The real in-flight fence is leaderCtx
// cancellation on lease loss (client-go cancels OnStartedLeading's ctx → aborts in-flight
// Collect/Emit/Save); the durable fence is the monotonic-Time + epoch checkpoint write (DESIGN §9).
// The value-changed-(series,ts) safety rests on emit-once-after-settle + monotonic Time, NOT on the
// epoch being a per-write fence — LeaseTransitions is coarser than that.
func (c *Coordinator) epoch(ctx context.Context) int64 {
	var lastErr error
	for attempt := 1; attempt <= epochReadAttempts; attempt++ {
		l, err := c.cs.CoordinationV1().Leases(c.ns).Get(ctx, c.name, metav1.GetOptions{})
		if err == nil {
			// nil LeaseTransitions is a legitimate epoch 0 (a fresh lease, no transitions yet) — not
			// an error, so return immediately without retrying.
			if l.Spec.LeaseTransitions == nil {
				return 0
			}
			return int64(*l.Spec.LeaseTransitions)
		}
		lastErr = err
		if attempt == epochReadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			slog.Warn("lease: epoch read cancelled before LeaseTransitions could be established; forward-write fence will trip", "err", ctx.Err())
			return 0
		case <-time.After(epochReadBackoff):
		}
	}
	slog.Warn("lease: could not read LeaseTransitions for epoch fence after retries; forward-write fence will trip (round3-#3)",
		"attempts", epochReadAttempts, "err", lastErr)
	return 0
}

var _ coordinate.Coordinator = (*Coordinator)(nil)
