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
