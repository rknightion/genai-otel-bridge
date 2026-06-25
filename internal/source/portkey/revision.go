// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// revisionHistory is the BOUNDED in-memory record of what we emitted, used to detect
// "settle exceedance": an already-emitted (settled) bucket whose value CHANGES on a later poll —
// a late arrival that landed after bucket_settle. We compare the re-fetched value of each bucket we
// remember against the remembered value; a change is a revision (counted via
// bucket_revised_after_settle_total). Detection ONLY — the caller never re-emits a settled bucket
// (re-emit would break the gap-free / byte-identical guarantee; Mimir rejects a value-changed
// (series, ts) anyway, DESIGN §3.3/F6).
//
// Memory is bounded by a trailing band: entries whose bucketEnd is older than (newest seen − band)
// are evicted. The band is ~bucket_settle + margin — wide enough to span the window in which a late
// arrival can plausibly still rewrite a settled bucket, narrow enough that memory stays O(series ×
// band/granularity). Beyond the band we accept a blind spot: a bucket that aged out and then changes
// is silently re-learned, not counted (the trade for bounded memory).
//
// This lives in the STATEFUL portkey loop layer, NOT in derive (which stays PURE). It is not
// goroutine-safe: a loop's Collect is single-flight (schedule.LoopRunner.Busy), so one loop's
// observe() calls never overlap.
type revisionHistory struct {
	band time.Duration
	// key = length-prefixed (series identity, bucketEnd unix-nanos) → last value we recorded for it.
	seen map[string]float64
	// bucketEnd per key, kept alongside so eviction can compare ages without re-parsing the key.
	when map[string]time.Time
}

func newRevisionHistory(band time.Duration) *revisionHistory {
	return &revisionHistory{
		band: band,
		seen: map[string]float64{},
		when: map[string]time.Time{},
	}
}

// observe records the settled samples for this poll and returns the bucketEnd timestamp of each one
// that is a REVISION — a bucket we have a remembered value for whose value changed. len(result) is
// the revision count; the caller turns each bucketEnd into a lateness age (now − bucketEnd) for the
// bucket_revised_after_settle_age_seconds histogram, so we measure HOW LATE revisions are, not just
// how many. `now` is the poll's reference time, used to evict entries older than the trailing band.
// Brand-new buckets and unchanged re-fetches are not revisions. observe updates the remembered value
// to the latest (so a revision is reported once, not every subsequent poll).
func (h *revisionHistory) observe(samples []model.Sample, now time.Time) []time.Time {
	var revised []time.Time
	for _, s := range samples {
		k := revisionKey(s)
		if prev, known := h.seen[k]; known && prev != s.Value {
			revised = append(revised, s.Timestamp)
		}
		h.seen[k] = s.Value
		h.when[k] = s.Timestamp
	}
	h.evict(now)
	return revised
}

// evict drops entries whose bucketEnd is older than (now − band) so memory stays bounded.
func (h *revisionHistory) evict(now time.Time) {
	cutoff := now.Add(-h.band)
	for k, t := range h.when {
		if t.Before(cutoff) {
			delete(h.when, k)
			delete(h.seen, k)
		}
	}
}

func (h *revisionHistory) len() int { return len(h.seen) }

// revisionKey is a collision-safe identity for a sample's bucket: (series name, unit, sorted
// labels, bucketEnd unix-nanos), LENGTH-PREFIXED so distinct tuples can never collide — same
// discipline as emit.seriesKey / otlp.labelKey (ext-review-11). A naive "k=v;ts" join would let
// {"a":"b;c=d"} and {"a":"b","c":"d"} alias and mis-detect (or miss) a revision. The timestamp is
// part of the key because each (series, bucketEnd) is a distinct 1DPM point.
func revisionKey(s model.Sample) string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d:%s%d:%s", len(s.Name), s.Name, len(s.Unit), s.Unit)
	for _, k := range keys {
		fmt.Fprintf(&b, "%d:%s%d:%s", len(k), k, len(s.Labels[k]), s.Labels[k])
	}
	fmt.Fprintf(&b, "@%d", s.Timestamp.UnixNano())
	return b.String()
}
