// SPDX-License-Identifier: AGPL-3.0-only

// internal/emit/coalesce.go
package emit

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
)

// CoalesceDPM enforces the hard ≤ maxDPM-data-points-per-minute cap on the PRODUCT plane, per
// (series, 60s-aligned sample timestamp), last-write-wins (newest timestamp survives). It is a pure,
// STATELESS, intra-batch stage: it runs in schedule.LoopRunner.ProcessBatch BEFORE splitByBucket, so a
// sub-minute / grouped source that produces several distinct-timestamp points inside one minute for one
// series is collapsed before those points fan into separate per-bucket emits (each of which Mimir would
// otherwise accept = >1DPM). Returns the surviving samples (input order preserved on the common no-op
// path) and the count suppressed (counted via aip_oi_samples_capped_total, never silent).
//
// Why stateless / intra-batch is sufficient (no cross-batch seen-set on the gap-free path):
//   - cross-batch same-minute re-emit is already prevented by bucket-settle + the forward-only,
//     epoch-fenced watermark (a settled minute is emitted once, never re-collected);
//   - cross-LOOP same-series collision is prevented at config time by source.ValidateOwnership;
//   - exact-duplicate-timestamp writes are the backstop: Mimir rejects them and the runner skips-with-
//     gap (aip_oi_samples_skipped_total{reason="duplicate_timestamp"}), also counted.
//
// So the only uncaught >1DPM path is intra-batch sub-minute multiplicity — exactly what this closes.
//
// LWW keeps the newest timestamp's value (the correct representative reading for a 1DPM gauge); ties on
// timestamp break deterministically by larger value so output is byte-deterministic regardless of input
// order. Survivor timestamps are NOT re-aligned to the minute boundary (that would invent timestamps and
// risk cross-batch duplicate-timestamp churn).
func CoalesceDPM(samples []model.Sample, maxDPM int) (kept []model.Sample, capped int) {
	if maxDPM < 1 {
		maxDPM = 1
	}
	if len(samples) == 0 {
		return samples, 0
	}
	type key struct {
		series string
		minute int64
	}
	groups := map[key][]int{}
	for i, s := range samples {
		k := key{seriesKey(s), s.Timestamp.Truncate(time.Minute).UnixNano()}
		groups[k] = append(groups[k], i)
	}
	keep := make([]bool, len(samples))
	for _, idxs := range groups {
		if len(idxs) <= maxDPM {
			for _, i := range idxs {
				keep[i] = true
			}
			continue
		}
		sort.SliceStable(idxs, func(a, b int) bool {
			ta, tb := samples[idxs[a]].Timestamp, samples[idxs[b]].Timestamp
			if !ta.Equal(tb) {
				return ta.After(tb) // newest first (LWW)
			}
			return samples[idxs[a]].Value > samples[idxs[b]].Value // deterministic tie-break
		})
		for j, i := range idxs {
			if j < maxDPM {
				keep[i] = true
			} else {
				capped++
			}
		}
	}
	if capped == 0 {
		return samples, 0 // common case: no allocation, bytes unchanged downstream
	}
	out := make([]model.Sample, 0, len(samples)-capped)
	for i, s := range samples {
		if keep[i] {
			out = append(out, s)
		}
	}
	return out, capped
}

// seriesKey is a collision-safe identity for a sample's series: (name, unit, sorted labels),
// LENGTH-PREFIXED so distinct tuples can never collide into one key. Mirrors otlp.labelKey's
// length-prefix discipline (ext-review-11): a naive "k=v;" join would let {"a":"b;c=d"} and
// {"a":"b","c":"d"} hash equal, coalescing two genuinely-different series and SILENTLY dropping data.
// Kept independent of otlp.labelKey on purpose — that one is the determinism-critical sort tiebreaker
// inside the FROZEN encoder; this one is a grouping key. They need only both be collision-safe, not
// byte-identical. Keep both length-prefixed if either changes.
func seriesKey(s model.Sample) string {
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
	return b.String()
}
