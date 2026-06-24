// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// dataPoint covers BOTH response shapes (OP5a, DESIGN §15): the count graphs use {timestamp,total};
// the latency graph uses {timestamp,avg,p50,p90,p99} (no total). JSON decode leaves absent fields 0.
type dataPoint struct {
	Timestamp          string  `json:"timestamp"`
	Total              float64 `json:"total"`
	TotalRequestUnits  float64 `json:"total_request_units"`  // tokens graph: prompt/input units
	TotalResponseUnits float64 `json:"total_response_units"` // tokens graph: completion/output units
	Avg                float64 `json:"avg"`
	P50                float64 `json:"p50"`
	P90                float64 `json:"p90"`
	P99                float64 `json:"p99"`
}

// timedPoint pairs a parsed bucket time with its datapoint so they sort together (round3-#1).
type timedPoint struct {
	t  time.Time
	dp dataPoint
}

// graphResponse is the analytics graph envelope. Summary is intentionally not modelled (its shape
// differs by graph — {total} vs {avg,p50,p90,p99} — and derive uses data_points only).
type graphResponse struct {
	DataPoints      []dataPoint `json:"data_points"`
	IsQuotaExceeded bool        `json:"is_quota_exceeded"`
	Object          string      `json:"object"`
}

// metricSuffix maps a graph name to its metric suffix. Units are baked into the NAME (so the
// gateway does not double-suffix, F42); Sample.Unit stays empty.
var metricSuffix = map[string]string{
	"requests": "requests",
	"cost":     "cost_usd",
	"tokens":   "tokens",
	"latency":  "latency_seconds",
	"errors":   "errors",
	"users":    "users",
}

// latencyStats are the latency statistics emitted as {quantile}-labelled gauges for the latency graph
// (OP5a, DESIGN §15): the p50/p90/p99 percentiles plus `avg` (the arithmetic mean). `avg` is NOT a
// percentile and is NOT derivable from p50/p90/p99, so it is emitted for notebook parity rather than
// computed downstream; it rides the existing `quantile` label as value "avg" to keep all latency stats
// under one metric (dashboards select the stat) — no new label key.
var latencyStats = []string{"p50", "p90", "p99", "avg"}

// tokenTypes splits the tokens graph into {token_type}-labelled gauges so the prompt/completion breakdown
// is queryable (cost attribution: output units are priced higher than input). Portkey reports `total`
// alongside `total_request_units` (input/prompt) and `total_response_units` (output/completion); total ==
// input + output (live-probed 2026-06-23). All three are emitted under ONE metric name distinguished by
// the label — DO NOT bare-sum `portkey_api_tokens` across token_type (that double-counts `total`); always
// select a token_type, or sum only `{token_type=~"input|output"}`.
var tokenTypes = []struct {
	label string
	pick  func(dataPoint) float64
}{
	{"total", func(dp dataPoint) float64 { return dp.Total }},
	{"input", func(dp dataPoint) float64 { return dp.TotalRequestUnits }},
	{"output", func(dp dataPoint) float64 { return dp.TotalResponseUnits }},
}

// msPerSecond converts Portkey's millisecond latency values to the seconds expected by the
// `_seconds`-suffixed metric (Prometheus/OTel convention). Confirmed ms against live data 2026-06-19
// (resolves the DESIGN §15 / followup §8 s-vs-ms unit caveat).
const msPerSecond = 1000.0

func (dp dataPoint) quantile(q string) float64 {
	switch q {
	case "p50":
		return dp.P50
	case "p90":
		return dp.P90
	case "p99":
		return dp.P99
	case "avg":
		return dp.Avg
	}
	return 0
}

func nameFor(prefix, graph string) string { return prefix + "_" + metricSuffix[graph] }

// cleanAPIKeyIDs normalises the operator's `settings.api_key_ids` (a CSV of Portkey api-key UUIDs) into
// the comma-joined value Portkey's `api_key_ids` query param expects: trims whitespace, drops empties.
// Empty/blank input ⇒ "" (no filter ⇒ workspace-wide, the backward-compatible default). This scopes the
// analytics/groups data to specific keys exactly as the prometheus notebook does (filter to its own key).
func cleanAPIKeyIDs(s string) string {
	var ids []string
	for p := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			ids = append(ids, t)
		}
	}
	return strings.Join(ids, ",")
}

// derive is PURE: (responses, window params) → settled, forward-only Gauge samples. No state,
// no I/O. bucketEnd = timestamp (+granularity when the API stamps bucket-START — OP5e set this true).
// The latency graph emits one gauge per percentile with a {quantile} label; all other graphs emit a
// single gauge from `total`.
func derive(resp map[string]graphResponse, prefix string, since, now time.Time, settle, gran time.Duration, startSemantics bool) ([]model.Sample, error) {
	cutoff := now.Add(-settle)
	graphs := make([]string, 0, len(resp))
	for g := range resp {
		graphs = append(graphs, g)
	}
	sort.Strings(graphs)

	var out []model.Sample
	for _, g := range graphs {
		r := resp[g]
		// [round3-#1] Parse into (time, datapoint) pairs and SORT ascending before the granularity
		// guard + emit loop. The guard's `step <= 0 || step%gran` test assumes ascending order; the
		// API is observed ascending (OP5) but relying on response ordering is fragile — a reordered
		// (or out-of-order) healthy response must not trip a false ErrGranularityUnexpected stall.
		pts := make([]timedPoint, 0, len(r.DataPoints))
		for _, dp := range r.DataPoints {
			ts, err := time.Parse(time.RFC3339, dp.Timestamp)
			if err != nil {
				return nil, fmt.Errorf("portkey: bad timestamp %q: %w", dp.Timestamp, err)
			}
			pts = append(pts, timedPoint{t: ts.UTC(), dp: dp})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
		// [AR-H-gran] Granularity guard + omitted-zero tolerance (F27/F43/OP5f). After sorting, a step
		// that is an integer multiple k×gran means the API OMITTED (k−1) empty buckets — normal, must
		// NOT stall (a hard `!= gran` check turns any quiet minute into a permanent stall). Only a
		// NON-multiple step (e.g. 90s vs 60s) or a 0 step (duplicate timestamp) is anomalous → reject.
		// (Portkey returns explicit zeros, OP5f, so omitted buckets are rare; the tolerance is defence.)
		for i := 1; i < len(pts); i++ {
			if step := pts[i].t.Sub(pts[i-1].t); step <= 0 || step%gran != 0 {
				return nil, fmt.Errorf("%w: %s step %s not a positive multiple of %s", source.ErrGranularityUnexpected, g, step, gran)
			}
		}
		for _, p := range pts {
			dp := p.dp
			bend := p.t
			if startSemantics {
				bend = bend.Add(gran)
			}
			if !bend.After(since) { // forward-only
				continue
			}
			if bend.After(cutoff) { // not yet settled
				continue
			}
			if g == "latency" {
				for _, q := range latencyStats {
					out = append(out, model.Sample{
						Name:   nameFor(prefix, g),
						Kind:   model.Gauge,
						Labels: map[string]string{"quantile": q},
						// Portkey reports latency in MILLISECONDS; convert to seconds for the
						// `_seconds` metric (see msPerSecond).
						Value:     dp.quantile(q) / msPerSecond,
						Timestamp: bend,
					})
				}
				continue
			}
			if g == "tokens" {
				for _, tt := range tokenTypes {
					out = append(out, model.Sample{
						Name:      nameFor(prefix, g),
						Kind:      model.Gauge,
						Labels:    map[string]string{"token_type": tt.label},
						Value:     tt.pick(dp),
						Timestamp: bend,
					})
				}
				continue
			}
			out = append(out, model.Sample{
				Name:      nameFor(prefix, g),
				Kind:      model.Gauge,
				Value:     dp.Total,
				Timestamp: bend,
			})
		}
	}
	return out, nil
}
