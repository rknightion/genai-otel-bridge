// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Invariant 1 — exactly one stable leader; the checkpoint advances under it; no churn/split-brain.
func TestInvariant1_SingleLeader(t *testing.T) {
	cs := client(t)
	waitSteady(t, cs)
	l0 := getLease(t, cs)
	startHolder, startTrans := holder(l0), transitions(l0)

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
}

// Invariant 2 — killing the leader: a standby takes over and resumes FORWARD from the checkpoint
// (no rewind), within the lease-expiry budget (ReleaseOnCancel=false ⇒ ~LeaseDuration, not instant).
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

	// Hard-kill the leader (grace 0 — skip the 300s drain; the standby acquires after lease expiry).
	zero := int64(0)
	if err := cs.CoreV1().Pods(ns()).Delete(context.Background(), oldHolder,
		metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("delete leader %q: %v", oldHolder, err)
	}

	// A different pod takes the Lease; transitions increments.
	eventually(t, 45*time.Second, func() bool {
		l := getLease(t, cs)
		return holder(l) != oldHolder && holder(l) != "" && transitions(l) > oldTrans
	}, "standby did not take over within lease-expiry budget")

	// The new leader resumes FORWARD past the pre-kill frontier (no rewind / no re-bootstrap).
	eventually(t, 120*time.Second, func() bool {
		wm, ok := latestWatermark(t, cs)
		return ok && wm.After(wm0)
	}, "checkpoint did not advance past the pre-kill watermark after failover")
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

	// The frontier stays monotonic (the new leader advances it; the zombie never rewinds/corrupts it).
	prev, _ := latestWatermark(t, cs)
	advanced := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		wm, ok := latestWatermark(t, cs)
		if ok {
			if wm.Before(prev) {
				t.Fatalf("checkpoint rewound under a frozen zombie: %s < %s", wm, prev)
			}
			if wm.After(prev) {
				advanced = true
			}
			prev = wm
		}
		time.Sleep(2 * time.Second)
	}
	if !advanced {
		t.Fatalf("checkpoint did not advance after the standby took over (new leader not progressing)")
	}
}
