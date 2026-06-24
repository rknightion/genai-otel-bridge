// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthHeartbeatReadyAndLeadership(t *testing.T) {
	now := time.Unix(1000, 0)
	h := NewHealth(3 * time.Minute)
	h.clock = func() time.Time { return now }

	if rec := do(h, "/readyz"); rec.Code != 503 {
		t.Fatalf("readyz before ready = %d want 503", rec.Code)
	}
	h.MarkReady()
	if rec := do(h, "/readyz"); rec.Code != 200 {
		t.Fatalf("readyz = %d want 200", rec.Code)
	}
	// [CP-C5] Standby (not leader) is healthy even with no beat (it doesn't run the scheduler).
	if rec := do(h, "/healthz"); rec.Code != 200 {
		t.Fatalf("standby healthz = %d want 200", rec.Code)
	}
	// As leader: healthy right after a beat.
	h.SetLeader(true)
	h.Beat()
	if rec := do(h, "/healthz"); rec.Code != 200 {
		t.Fatalf("leader healthz after beat = %d want 200", rec.Code)
	}
	// Stale past the threshold with no beat ⇒ unhealthy (a wedged scheduler is restartable).
	now = now.Add(4 * time.Minute)
	if rec := do(h, "/healthz"); rec.Code != 503 {
		t.Fatalf("leader stale healthz = %d want 503", rec.Code)
	}
	// Demoted back to standby ⇒ healthy again.
	h.SetLeader(false)
	if rec := do(h, "/healthz"); rec.Code != 200 {
		t.Fatalf("demoted standby healthz = %d want 200", rec.Code)
	}
}

func do(h *Health, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}
