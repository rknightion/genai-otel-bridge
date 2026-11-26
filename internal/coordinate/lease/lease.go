// SPDX-License-Identifier: Apache-2.0

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

// leadershipStoppedEvent classifies why leader-election stopped, for logging [#74]. client-go fires
// OnStoppedLeading on every Run() exit, so a bare "leadership lost" would mislead on clean shutdowns
// and never-elected standbys. Extracted as a pure function so the three cases are unit-testable without
// racing client-go's renewal machinery:
//   - never led (a standby exiting on ctx cancel)        → Debug, quiet: nothing was lost.
//   - led and the root ctx is cancelled (SIGTERM/rollout) → Info shutdown message, NOT "leadership lost".
//   - led and the ctx is still live (genuine renewal lapse) → Warn "leadership lost", loud (alertable).
func leadershipStoppedEvent(everLed bool, ctxErr error) (slog.Level, string) {
	switch {
	case !everLed:
		return slog.LevelDebug, "leader-election stopped without ever leading (standby)"
	case ctxErr != nil:
		return slog.LevelInfo, "leadership released on shutdown; loops stopped"
	default:
		return slog.LevelWarn, "leadership lost (renewal lapse); loops stopped"
	}
}

// Run campaigns for leadership and runs onElected while leader. [#110] On a genuine renewal lapse
// (leadership lost while the root ctx is still alive) it RE-CAMPAIGNS in-process rather than
// returning — symmetric with the dynamodb coordinator's acquire loop. Previously a lapse made Run
// return ctx.Err() == nil, which fell through main's `err != nil && ctx.Err() == nil` fatal guard, so
// the process logged "stopped" and exited 0 mid-pod-life on every K8s-API flap. Now the loop only
// returns when the root ctx is cancelled (SIGTERM/rollout → ctx.Err() != nil, a clean exit) or when
// elector construction fails (a loud, non-nil error → main fatal). Each iteration builds a FRESH
// elector so a re-election reads a fresh epoch fence and re-enters onElected (Scheduler.Run resets via
// Reset(), the same re-entry the dynamodb coordinator already relies on; the drain barrier below joins
// the prior term first, so a re-election never races the old leadership's emit worker).
func (c *Coordinator) Run(ctx context.Context, onElected func(context.Context)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.campaign(ctx, onElected); err != nil {
			return err // elector construction failure — genuine, loud, non-nil (main fatals)
		}
		// campaign() returned: either the root ctx was cancelled (handled at the top of the next
		// iteration) or leadership lapsed with the ctx still live — loop back and re-campaign.
	}
}

// campaign runs a single leader-election term to completion (drain barrier included). It returns nil
// when the term ends (leadership lost or ctx cancelled) and a non-nil error only if the elector
// cannot be constructed.
func (c *Coordinator) campaign(ctx context.Context, onElected func(context.Context)) error {
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
		// ReleaseOnCancel is intentionally false: on SIGTERM the root ctx is cancelled, which aborts
		// any in-flight collect/emit and the epoch-fenced commit (no final watermark is persisted on
		// the way out) — we let the lease EXPIRE rather than release it into a standby, so the next
		// leader resumes from the last committed watermark and re-pulls the partial window (F35).
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				elected.Store(true)
				slog.Info("leader elected; starting loops", "identity", c.identity)
				defer close(leadDone)
				onElected(coordinate.WithEpoch(leaderCtx, c.epoch(leaderCtx)))
			},
			// [#74] client-go v0.36.2 defers OnStoppedLeading UNCONDITIONALLY at the top of Run (before the
			// acquire check), so it fires on EVERY Run() exit: a never-elected standby exiting on ctx cancel,
			// a cleanly SIGTERM'd leader, and a genuine renewal lapse alike. Classify the three so a rolling
			// restart of a healthy deployment doesn't emit a spurious "leadership lost" from every replica
			// (which would false-positive any alert built on that line and mask a real lapse). elected is set
			// inside OnStartedLeading; ctx is the root election ctx (cancelled ⇒ clean shutdown, not a lapse).
			OnStoppedLeading: func() {
				lvl, msg := leadershipStoppedEvent(elected.Load(), ctx.Err())
				slog.Log(context.Background(), lvl, msg, "identity", c.identity)
			},
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
	// [#110] Return nil regardless of why the term ended — Run's loop re-checks ctx.Err() at the top
	// and returns it on a real cancellation, or re-campaigns on a bare leadership lapse.
	return nil
}

// epochReadBackoff is the pause between attempts to read the Lease for the epoch fence, and
// epochReadWarnEvery rate-limits the warning while the read keeps failing. Hardcoded, like the lease
// timings in main.go; no config knob.
const (
	epochReadBackoff   = 200 * time.Millisecond
	epochReadWarnEvery = 10 * time.Second
)

// leaseEpoch maps a Lease's LeaseTransitions count to the leader epoch: the count plus one, so the
// first leader on a fresh Lease (0 transitions) runs at epoch 1. That lines the lease numbering up with
// the no-op coordinator (epoch 1) and the DynamoDB coordinator (first fence 1): a single-replica
// deployment's checkpoint, written at epoch 1, stays writable when HA is enabled over it [#375]. It is a
// monotonic upward shift from the old numbering (epoch == count), so a checkpoint written by an older
// release, or by an older replica in a mixed-version rollout, is never above the new epoch. A nil count
// (client-go always writes one, but the API allows it) is treated as 0 transitions.
func leaseEpoch(transitions *int32) int64 {
	if transitions == nil {
		return 1
	}
	return int64(*transitions) + 1
}

// epoch reads the Lease's LeaseTransitions (incremented on each leadership change) once at election
// and returns it as a COARSE monotonic fence epoch (see leaseEpoch). [AR-C4, #376] A failed Get is retried
// every epochReadBackoff until it succeeds or ctx (the leader context) is done; the scheduler is not
// started until then, because a leader that starts at a guessed epoch below the stored one is fenced on
// every write for its whole term while it keeps renewing the Lease, blocking every standby. On ctx done it
// returns 0 and the caller's onElected runs under the cancelled context and exits without collecting.
// client-go cancels the leader context when renewal fails, so the wait is bounded by the leadership
// term. While it lasts the failure is logged at warning level, rate-limited by epochReadWarnEvery.
//
// This epoch is not re-checked before each Save. The real in-flight fence is leaderCtx cancellation on
// lease loss (client-go cancels OnStartedLeading's ctx, aborting in-flight Collect/Emit/Save); the
// durable fence is the monotonic-Time + epoch checkpoint write (DESIGN §9). The value-changed-(series,ts)
// safety rests on emit-once-after-settle + monotonic Time, NOT on the epoch being a per-write fence:
// LeaseTransitions is coarser than that.
func (c *Coordinator) epoch(ctx context.Context) int64 {
	began := time.Now()
	var failures int
	var lastWarn time.Time
	for {
		l, err := c.cs.CoordinationV1().Leases(c.ns).Get(ctx, c.name, metav1.GetOptions{})
		if err == nil {
			if failures > 0 {
				slog.Info("lease: read LeaseTransitions for the epoch fence after failed attempts; starting loops",
					"failed_attempts", failures, "waited", time.Since(began))
			}
			return leaseEpoch(l.Spec.LeaseTransitions)
		}
		failures++
		if lastWarn.IsZero() || time.Since(lastWarn) >= epochReadWarnEvery {
			slog.Warn("lease: cannot read LeaseTransitions for the epoch fence; loops stay stopped until it is read or leadership ends",
				"failed_attempts", failures, "waited", time.Since(began), "err", err)
			lastWarn = time.Now()
		}
		select {
		case <-ctx.Done():
			slog.Warn("lease: leadership ended before LeaseTransitions could be read; loops not started",
				"failed_attempts", failures, "waited", time.Since(began), "err", err)
			return 0
		case <-time.After(epochReadBackoff):
		}
	}
}

var _ coordinate.Coordinator = (*Coordinator)(nil)
