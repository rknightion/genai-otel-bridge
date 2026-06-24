// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
)

// groupsResponse is the /analytics/groups/* envelope: a flat WINDOW-TOTAL list (one aggregate row per
// dimension value over [min,max]), NOT a time-bucketed series — there is no timestamp/data_points
// (groups PoC §a). `is_quota_exceeded` is OPTIONAL (ai-models omits it, users/metadata include it) —
// a plain bool field is absent-safe (absent ⇒ false), which is exactly the decoder tolerance §d wants.
// Rows decode as raw maps so the parser can extract the dimension value WITHOUT hard-coding the row's
// dimension field name (`ai_model`/`user`/`<metadata key>`) — the metadata field name is unconfirmed
// in dev (§c), so we never depend on it.
type groupsResponse struct {
	Object          string                       `json:"object"`
	Total           int                          `json:"total"`
	IsQuotaExceeded bool                         `json:"is_quota_exceeded"`
	Data            []map[string]json.RawMessage `json:"data"`
}

// groupRow is one parsed dimension row: its dimension VALUE (label value), request count, and cost
// (cost is optional — hasCost distinguishes "absent" from "a real 0", so a missing cost field never
// emits a spurious 0-cost gauge).
type groupRow struct {
	dimValue string
	requests float64
	cost     float64
	hasCost  bool
}

// parseGroupRows extracts rows from a decoded envelope. `requests` is required (numeric); `cost` is
// optional; the dimension VALUE is read from the EXPLICIT `dimField` (ai_model | metadata_value),
// confirmed by a 2026-06-20 live probe of the real Portkey API. Metadata rows also carry extra stat
// fields (avg_tokens, avg_weighted_feedback, last_seen, requests_with_feedback) which are ignored —
// we read only dimField / requests / cost. A non-string dimField value falls back to "" (the
// unattributed bucket).
func parseGroupRows(data []map[string]json.RawMessage, dimField string) ([]groupRow, error) {
	out := make([]groupRow, 0, len(data))
	for i, raw := range data {
		var row groupRow
		rq, ok := raw["requests"]
		if !ok {
			return nil, fmt.Errorf("portkey groups: row %d missing requests", i)
		}
		if err := json.Unmarshal(rq, &row.requests); err != nil {
			return nil, fmt.Errorf("portkey groups: row %d requests: %w", i, err)
		}
		if c, ok := raw["cost"]; ok {
			if err := json.Unmarshal(c, &row.cost); err != nil {
				return nil, fmt.Errorf("portkey groups: row %d cost: %w", i, err)
			}
			row.hasCost = true
		}
		// Dimension value: read from the explicit dimField; best-effort — a non-string value gives "".
		if dv, ok := raw[dimField]; ok {
			_ = json.Unmarshal(dv, &row.dimValue) // best-effort; non-string ⇒ ""
		}
		out = append(out, row)
	}
	return out, nil
}

// groupMetricName builds a distinct groups series name: <prefix>_<metric>_by_<dim>. The `_by_<dim>`
// suffix keeps groups disjoint from the analytics aggregate names (e.g. portkey_api_requests) so they
// never collide after normalisation (M7 ValidateOwnership) and never mix labelled+unlabelled series.
func groupMetricName(prefix, metric, dim string) string {
	return prefix + "_" + metric + "_by_" + dim
}

// deriveGroups maps parsed rows for ONE endpoint into window-total Gauge samples stamped at `at` (the
// window upper bound). Each row emits a requests gauge always, and a cost gauge only when emitCost is
// set AND the row carried a cost. `baseLabels` (e.g. {metadata_key:<k>}) is copied into each sample so
// the shared map is never aliased; `valueLabelKey` (ai_model / metadata_value) carries the dim value.
//
// Cost: Portkey's groups cost field is in USD CENTS (confirmed 2026-06-20 by a physical token-ceiling
// argument: text-embedding-3-small 72,045 reqs × 8,191-token max × $0.02/1M ≈ $11.80 max possible
// dollars, but the field read 136.85 → must be cents). Emitted as <prefix>_cost_usd_by_<dim> (÷100
// → dollars), ON by default.
func deriveGroups(rows []groupRow, prefix, dim string, baseLabels map[string]string, valueLabelKey string, emitCost bool, at time.Time) []model.Sample {
	out := make([]model.Sample, 0, len(rows))
	for _, r := range rows {
		labels := make(map[string]string, len(baseLabels)+1)
		maps.Copy(labels, baseLabels)
		labels[valueLabelKey] = r.dimValue
		out = append(out, model.Sample{
			Name:      groupMetricName(prefix, "requests", dim),
			Kind:      model.Gauge,
			Labels:    labels,
			Value:     r.requests,
			Timestamp: at,
		})
		if emitCost && r.hasCost {
			costLabels := maps.Clone(labels)
			out = append(out, model.Sample{
				Name:      groupMetricName(prefix, "cost_usd", dim),
				Kind:      model.Gauge,
				Labels:    costLabels,
				Value:     r.cost / 100, // Portkey cents → dollars
				Timestamp: at,
			})
		}
	}
	return out
}
