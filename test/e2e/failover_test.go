// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Invariant 1 — exactly one stable leader; the checkpoint advances under it; no churn/split-brain;
// and what it emits is what the mock served, one point per bucket with none missing.
func TestInvariant1_SingleLeader(t *testing.T) {
	cs := client(t)
	waitSteady(t, cs)
	l0 := getLease(t, cs)
	startHolder, startTrans := holder(l0), transitions(l0)
	marks := sinkMarks(t, cs)

	// The single leader must make forward progress (checkpoint advances at least once).
	var wm0 time.Time
	eventually(t, 90*time.Second, func() bool {
		wm, ok := latestWatermark(t, cs)
		if ok {
			wm0 = wm
		}
		return ok
	}, "checkpoint never created (leader not progressing)")
	eventually(t, 90*time.Second, func() bool {
		wm, ok := latestWatermark(t, cs)
		return ok && wm.After(wm0)
	}, "checkpoint never advanced under the leader")

	// Hold steady for ~3x LeaseDuration: the SAME holder, no new transitions (no split-brain/churn).
	consistently(t, 45*time.Second, func() bool {
		l := getLease(t, cs)
		return holder(l) == startHolder && transitions(l) == startTrans
	}, "leader changed under steady state (churn or split-brain)")

	requireLeaderRun(t, cs, startHolder, startTrans, marks[startHolder], 2)
}

// Invariant 2 — killing the leader: a standby takes over within the lease-expiry budget
// (ReleaseOnCancel=false ⇒ ~LeaseDuration, not instant), the checkpoint moves forward, and the new
// leader's first emitted bucket is the first one the old checkpoint did not cover (no re-emit, no
// re-bootstrap, no gap), read from its OTLP sink.
func TestInvariant2_FailoverResumesNoRewind(t *testing.T) {
	cs := client(t)
	waitSteady(t, cs)
	l0 := getLease(t, cs)
	oldHolder, oldTrans := holder(l0), transitions(l0)

	// Ensure the checkpoint has advanced under the old leader before we kill it.
	var wm0 time.Time
	eventually(t, 120*time.Second, func() bool {
		wm, ok := latestWatermark(t, cs)
		if ok {
			wm0 = wm
		}
		return ok && !wm.IsZero()
	}, "checkpoint never advanced before kill")
	cp0, _ := latestRecord(t, cs)
	frontier := &failoverFrontier{oldEpoch: cp0.Epoch, old: cp0.Time}
	marks := sinkMarks(t, cs)

	// Hard-kill the leader (grace 0 — skip the 300s drain; the standby acquires after lease expiry).
	zero := int64(0)
	if err := cs.CoreV1().Pods(ns()).Delete(context.Background(), oldHolder,
		metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("delete leader %q: %v", oldHolder, err)
	}

	// A different pod takes the Lease; transitions increments.
	eventually(t, 45*time.Second, func() bool {
		frontier.observe(t, cs)
		l := getLease(t, cs)
		return holder(l) != oldHolder && holder(l) != "" && transitions(l) > oldTrans
	}, "standby did not take over within lease-expiry budget")
	newHolder := holder(getLease(t, cs))

	// The checkpoint moves forward past the pre-kill frontier. This alone does not prove a resume
	// from the checkpoint: a re-bootstrapped leader would also write a later watermark within a tick.
	eventually(t, 120*time.Second, func() bool {
		frontier.observe(t, cs)
		wm, ok := latestWatermark(t, cs)
		return ok && wm.After(wm0)
	}, "checkpoint did not advance past the pre-kill watermark after failover")

	// [#408] What does prove it: the new leader's emitted data starts exactly after the old checkpoint.
	requireResumeAtCheckpoint(t, cs, frontier, newHolder, marks[newHolder])
}

// Invariant 3 — a LIVE but demoted leader ("zombie"): freezing the leader (SIGSTOP) makes it stop
// renewing; a standby takes over, and the frozen zombie neither advances nor rewinds the shared
// checkpoint. Assertions complete before the ~60s liveness restart of the frozen container.
func TestInvariant3_ZombieFrozenStandbyTakesOver(t *testing.T) {
	cs := client(t)
	waitSteady(t, cs)
	l0 := getLease(t, cs)
	zombie, oldTrans := holder(l0), transitions(l0)

	freezeLeader(t, zombie)

	// Despite the (still-alive) frozen leader, a standby acquires the Lease.
	eventually(t, 45*time.Second, func() bool {
		l := getLease(t, cs)
		return holder(l) != zombie && holder(l) != "" && transitions(l) > oldTrans
	}, "standby never took over from the frozen zombie leader")

	// [#138] Resume (SIGCONT) the frozen ex-leader while the new leader is active, so the stale zombie
	// wakes with its pre-freeze in-flight batch and attempts a FORWARD commit against the newer epoch —
	// the real-cluster stale-writer path that a SIGSTOP-for-life zombie never exercises (a frozen process
	// cannot attempt any write, so "neither advances nor rewinds" was trivially true). Resume promptly:
	// the zombie must be signalled before its ~60s liveness restart (a restarted pod returns fresh as a
	// standby and never fences).
	resumeLeader(t, zombie)

	// Over ~2 lease durations after the resume, the shared frontier must stay MONOTONIC and keep making
	// forward progress: the woken zombie neither rewinds nor double-advances it, and the new leader keeps
	// advancing it. Those are the load-bearing safety invariants and are asserted hard below.
	//
	// [#138] The `checkpoint forward-write fenced` warn is checked only as BEST-EFFORT evidence, not a hard
	// gate. Positive fence evidence from a resumed zombie is not reliably reproducible here, by design:
	//   - the runner's commit() re-checks leadership (`ctx.Err()`, [ext-review-2]) immediately before the
	//     Save, so once the zombie's coordinate loop observes the lost lease and cancels leaderCtx it never
	//     reaches Save — no ErrStaleWrite, no fence warn; and
	//   - even in the narrow window where the Save does run, after a ~45s+ takeover the new leader's durable
	//     watermark is already AHEAD of the zombie's stale in-flight target, so the write lands in the
	//     benign already-advanced branch (durable ≥ attempted), which is silent by contract.
	// Either outcome is correct, safe behaviour. So we log the warn if it happens to appear but do NOT fail
	// its absence — the monotonic/no-rewind/advanced assertions are what prove the zombie caused no harm.
	prev, _ := latestWatermark(t, cs)
	advanced, fenced := false, false
	// The analytics window end is floored to the 1m granularity (#357), so its watermark only moves on
	// whole-minute boundaries even though the e2e cadence is 10s: a 30s window can legitimately see no
	// advance. Budget one granularity step plus cadence and slack. The zombie was resumed above, before
	// its ~60s liveness restart, and a later restart only returns it as a standby (log reads tolerate
	// it), so the longer window does not change what is asserted.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		wm, ok := latestWatermark(t, cs)
		if ok {
			if wm.Before(prev) {
				t.Fatalf("checkpoint rewound after resuming the stale zombie: %s < %s", wm, prev)
			}
			if wm.After(prev) {
				advanced = true
			}
			prev = wm
		}
		if !fenced && podLogsContains(t, cs, zombie, "checkpoint forward-write fenced") {
			fenced = true
		}
		time.Sleep(2 * time.Second)
	}
	if !advanced {
		t.Fatalf("checkpoint did not advance after the standby took over (new leader not progressing)")
	}
	if fenced {
		t.Logf("[#138] observed a 'checkpoint forward-write fenced' warn from resumed zombie %s (best-effort evidence)", zombie)
	} else {
		t.Logf("[#138] no fence warn from resumed zombie %s — expected: leaderCtx cancelled before Save, or benign already-advanced (see comment); monotonic/no-rewind invariants held", zombie)
	}
}
