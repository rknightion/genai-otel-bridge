// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"sync"
	"testing"
	"time"
)

func TestLimiterAllow(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(time.Minute)
	l.SetClockForTest(func() time.Time { return now })

	if !l.Allow("k") {
		t.Fatal("first sight of a key must be allowed")
	}
	if l.Allow("k") {
		t.Fatal("immediate repeat within the window must be suppressed")
	}
	// A different key is independent — allowed on first sight even within the window.
	if !l.Allow("k2") {
		t.Fatal("a distinct key must be allowed on first sight")
	}
	// Advance just under the window: still suppressed.
	now = now.Add(59 * time.Second)
	if l.Allow("k") {
		t.Fatal("still within the window must be suppressed")
	}
	// Cross the window: allowed again.
	now = now.Add(2 * time.Second)
	if !l.Allow("k") {
		t.Fatal("after the window elapses the key must be allowed again")
	}
}

func TestLimiterConcurrentSingleWinner(t *testing.T) {
	// All goroutines hit the same key within one window (frozen clock) — exactly one may win.
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(time.Minute)
	l.SetClockForTest(func() time.Time { return now })

	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if l.Allow("hot") {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one goroutine should win within the window, got %d", wins)
	}
}
