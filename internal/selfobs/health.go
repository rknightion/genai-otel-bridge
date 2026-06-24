// SPDX-License-Identifier: AGPL-3.0-only

// Package selfobs is OTLP-native self-observability: self-metrics (schedule.Metrics), a
// distinct resource identity (H4), and health endpoints. /healthz tracks scheduler progress
// (a leader blocked on intended backpressure still beats; only a wedged loop goes unhealthy, L1).
package selfobs

import (
	"net/http"
	"sync"
	"time"
)

type Health struct {
	staleThreshold time.Duration // [CP-C5] generous: > emit-retry budget + max cadence, so intended backpressure never kills a leader
	mu             sync.Mutex
	lastBeat       time.Time
	ready          bool
	leader         bool
	clock          func() time.Time
}

// NewHealth takes an explicit staleness threshold (NOT a hard-coded cadence). main computes it from
// max loop cadence + the emit-retry budget + margin. [CP-C5]
func NewHealth(staleThreshold time.Duration) *Health {
	return &Health{staleThreshold: staleThreshold, clock: time.Now}
}

// Beat records that the scheduler attempted a loop iteration (progress, not necessarily success).
func (h *Health) Beat() { h.mu.Lock(); h.lastBeat = h.clock(); h.mu.Unlock() }

// MarkReady flips /readyz to 200 (config loaded + coordinator started).
func (h *Health) MarkReady() { h.mu.Lock(); h.ready = true; h.mu.Unlock() }

// SetLeader toggles leadership (wired from the coordinator). [CP-C5] A standby (not leader) is
// healthy by definition — it does not run the scheduler, so it must not be judged on heartbeat.
func (h *Health) SetLeader(v bool) { h.mu.Lock(); h.leader = v; h.mu.Unlock() }

func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		ok := h.ready
		h.mu.Unlock()
		code(w, ok)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		// [CP-C5] standby (not leader) is always healthy; only a LEADER is judged on heartbeat
		// freshness (a wedged scheduler loop), with a threshold generous enough that a leader
		// blocked in an intended emit-retry backpressure window stays alive.
		fresh := !h.leader || (!h.lastBeat.IsZero() && h.clock().Sub(h.lastBeat) <= h.staleThreshold)
		h.mu.Unlock()
		code(w, fresh)
	})
	return mux
}

func code(w http.ResponseWriter, ok bool) {
	if ok {
		w.WriteHeader(200)
		return
	}
	w.WriteHeader(503)
}
