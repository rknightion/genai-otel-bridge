// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"sync"
	"time"
)

// Limiter throttles repeated log lines to at most one per `every` per key, so a persistently failing
// loop logs on first occurrence then at most once a minute — the metric counter carries the true rate.
// Keys are caller-chosen (e.g. "collect:<loop>"); a distinct key has its own independent window. Safe
// for concurrent use.
type Limiter struct {
	mu    sync.Mutex
	last  map[string]time.Time
	every time.Duration
	now   func() time.Time
}

// NewLimiter returns a Limiter that allows a given key at most once per `every`.
func NewLimiter(every time.Duration) *Limiter {
	return &Limiter{last: make(map[string]time.Time), every: every, now: time.Now}
}

// Allow reports whether a line for key may be logged now: true on first sight of the key, then true
// only once `every` has elapsed since the last allowed line. On a true result it records the time.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if prev, ok := l.last[key]; ok && now.Sub(prev) < l.every {
		return false
	}
	l.last[key] = now
	return true
}

// SetClockForTest overrides the clock for determinism; mirrors the schedule package's test-clock seam.
func (l *Limiter) SetClockForTest(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}
