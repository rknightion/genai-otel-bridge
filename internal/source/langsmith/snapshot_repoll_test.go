// SPDX-License-Identifier: Apache-2.0

package langsmith

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// The backend rejects a changed value at an existing (series, ts) instead of last-write-wins, so a
// snapshot loop must emit at most once per minute-truncated stamp (the runner would otherwise count the
// whole batch as samples_skipped{reason=duplicate_timestamp}). A same-stamp poll makes no vendor call
// and still advances the liveness watermark.

func TestSnapshotSameMinuteRepollSessionsSkipsVendor(t *testing.T) {
	var hits atomic.Int64
	inner := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer inner.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	lp := mkLoop(t, testConfig(srv), now)

	b1, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b1.Samples) == 0 {
		t.Fatalf("first poll: err=%v samples=%d", err, len(b1.Samples))
	}
	first := hits.Load()
	now = now.Add(55 * time.Second) // 12:00:57, same minute
	lp.now = func() time.Time { return now }
	b2, err := lp.Collect(context.Background(), b1.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Samples) != 0 {
		t.Fatalf("same-minute re-poll emitted %d samples, want 0", len(b2.Samples))
	}
	if hits.Load() != first {
		t.Fatalf("same-minute re-poll issued %d vendor request(s)", hits.Load()-first)
	}
	if !b2.Watermark.Time.Equal(now) {
		t.Fatalf("watermark = %s, want %s (liveness must still advance)", b2.Watermark.Time, now)
	}
	if b2.Key != lp.Key() {
		t.Fatalf("empty batch key = %+v", b2.Key)
	}
	// Next minute emits again.
	now = now.Add(5 * time.Second)
	lp.now = func() time.Time { return now }
	b3, err := lp.Collect(context.Background(), b2.Watermark)
	if err != nil || len(b3.Samples) == 0 {
		t.Fatalf("next-minute poll: err=%v samples=%d", err, len(b3.Samples))
	}
}

// Collect succeeds but the emit then fails: the runner leaves the checkpoint unchanged, so the next
// poll in the same minute receives the OLD watermark and must emit again (nothing was delivered).
func TestSnapshotSameMinuteRetryAfterFailedEmitSessions(t *testing.T) {
	srv := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer srv.Close()
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	lp := mkLoop(t, testConfig(srv), now)
	since := model.Watermark{} // never advanced: the emit failed, nothing committed
	if b, err := lp.Collect(context.Background(), since); err != nil || len(b.Samples) == 0 {
		t.Fatalf("first poll: err=%v samples=%d", err, len(b.Samples))
	}
	now = now.Add(55 * time.Second)
	lp.now = func() time.Time { return now }
	b, err := lp.Collect(context.Background(), since)
	if err != nil || len(b.Samples) == 0 {
		t.Fatalf("same-minute retry after a failed emit must emit again: err=%v samples=%d", err, len(b.Samples))
	}
}

func TestSnapshotSameMinuteRetryAfterFailedEmitUsage(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	lp := mkUsageLoop(t, f, now, nil)
	since := model.Watermark{}
	if b, err := lp.Collect(context.Background(), since); err != nil || len(b.Samples) == 0 {
		t.Fatalf("first poll: err=%v samples=%d", err, len(b.Samples))
	}
	now = now.Add(55 * time.Second)
	lp.now = func() time.Time { return now }
	b, err := lp.Collect(context.Background(), since)
	if err != nil || len(b.Samples) == 0 {
		t.Fatalf("same-minute retry after a failed emit must emit again: err=%v samples=%d", err, len(b.Samples))
	}
}

func TestSnapshotSameMinuteRepollSessionsFailureDoesNotConsumeStamp(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	inner := fakeLangSmith(t, loadFixture(t, "sessions_with_stats.json"))
	defer inner.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	lp := mkLoop(t, testConfig(srv), now)
	if _, err := lp.Collect(context.Background(), model.Watermark{}); err == nil {
		t.Fatal("expected the 500 poll to fail")
	}
	fail.Store(false)
	now = now.Add(10 * time.Second)
	lp.now = func() time.Time { return now }
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b.Samples) == 0 {
		t.Fatalf("retry in the same minute must emit: err=%v samples=%d", err, len(b.Samples))
	}
}

func TestSnapshotSameMinuteRepollUsageSkipsVendor(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	lp := mkUsageLoop(t, f, now, nil)
	b1, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b1.Samples) == 0 {
		t.Fatalf("first poll: err=%v samples=%d", err, len(b1.Samples))
	}
	sessions, stats := len(f.sessionsQuery), len(f.statsBodies)
	now = now.Add(55 * time.Second)
	lp.now = func() time.Time { return now }
	b2, err := lp.Collect(context.Background(), b1.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Samples) != 0 || len(f.sessionsQuery) != sessions || len(f.statsBodies) != stats {
		t.Fatalf("same-minute re-poll: samples=%d extra sessions calls=%d extra stats calls=%d",
			len(b2.Samples), len(f.sessionsQuery)-sessions, len(f.statsBodies)-stats)
	}
	if !b2.Watermark.Time.Equal(now) {
		t.Fatalf("watermark = %s, want %s", b2.Watermark.Time, now)
	}
}

func TestSnapshotSameMinuteRepollUsageFailureDoesNotConsumeStamp(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	f := newFakeUsageServer(t, usageFixture())
	f.sessionsCode = http.StatusInternalServerError
	lp := mkUsageLoop(t, f, now, nil)
	if _, err := lp.Collect(context.Background(), model.Watermark{}); err == nil {
		t.Fatal("expected the 500 poll to fail")
	}
	f.sessionsCode = 0
	now = now.Add(10 * time.Second)
	lp.now = func() time.Time { return now }
	b, err := lp.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b.Samples) == 0 {
		t.Fatalf("retry in the same minute must emit: err=%v samples=%d", err, len(b.Samples))
	}
}
