// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"fmt"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
)

// humanDur formats a duration as a compact human string ("24h", "5m", "1h30m") for chart examples.
// time.Duration.String() emits "24h0m0s" — ugly and inconsistent with the chart's "24h"/"10m" style.
// Copied verbatim from internal/source/langsmith/settings.go (each vendor package owns its copy,
// same as splitCSV).
func humanDur(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= h * time.Hour
	}
	if m := d / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		d -= m * time.Minute
	}
	if s := d / time.Second; s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

// ExampleSource returns a documented, buildable example Portkey source for the Helm chart generator
// to render. It lives here (not in internal/config) so the vendor-specific loop keys and settings
// keys stay in the vendor package, keeping core decoupled. The generator renders this value into a
// (commented-out) chart example.
//
// The analytics loop is intentionally absent: it is already the active default config: block and has
// no settings keys to document. This example surfaces only the groups + logs_export loops, which are
// opt-in and have decoupled settings knobs not visible elsewhere in the chart.
func ExampleSource() config.SourceConfig {
	g := defaultGroupsSettings()
	l := defaultLogsSettings()
	return config.SourceConfig{
		Type:           "portkey",
		Enabled:        true,
		BaseURL:        "https://api.portkey.ai/v1",
		SourceInstance: "portkey-${ENV}",
		Auth:           config.AuthConfig{Header: "x-portkey-api-key", Value: "${PORTKEY_API_KEY}"},
		RateLimit:      config.RateLimitConfig{RPS: 1, Burst: 3},
		// api_key_use_cases: maps a use-case label to the Portkey api-key UUIDs whose traffic it
		// represents. Non-empty ⇒ each enabled loop scopes per entry and stamps a normalised
		// api_key_use_case label (metrics) / record attribute (logs). Empty ⇒ today's workspace-wide
		// behaviour (optional per-loop settings.api_key_ids). Only listed keys are collected;
		// unlisted-key traffic is intentionally out of scope. Each label is normalised (lowercased,
		// spaces→underscores) to the api_key_use_case metric label / log record attribute.
		APIKeyUseCases: []config.APIKeyUseCase{
			{Label: "Data Gen", APIKeyIDs: []string{"<api-key-uuid>"}},
		},
		Loops: map[string]config.LoopConfig{
			// Window-total snapshot loop: per-dimension gauges (ai_model, request-metadata). Window==0
			// (snapshot-scheduled; the real query window is settings.window_span). Cost is emitted in
			// USD by default (÷100 from Portkey's cents, confirmed 2026-06-20).
			"groups": {
				Enabled: true,
				Cadence: config.Duration(time.Minute),
				Settings: map[string]string{
					"window_span":        humanDur(g.windowSpan),
					"settle":             humanDur(g.settle),
					"page_size":          fmt.Sprintf("%d", g.pageSize),
					"max_groups":         fmt.Sprintf("%d", g.maxGroups),
					"metadata_keys":      "use_case",
					"emit_cost":          "true",
					"emit_prompts":       "true",
					"expected_workspace": "",
					"api_key_ids":        "",
				},
			},
			// Stateful export-lifecycle loop: content-free per-request OTLP logs → Loki. Window==0
			// (snapshot-scheduled; the real export window is settings.window). workspace_id and
			// signed_url_allow_hosts are REQUIRED — construction fails without them.
			"logs_export": {
				Enabled: true,
				Cadence: config.Duration(time.Minute),
				Settings: map[string]string{
					"window":                  "1h",
					"settle":                  humanDur(l.settle),
					"max_backfill":            humanDur(l.maxBackfill),
					"page_size":               fmt.Sprintf("%d", l.pageSize),
					"max_pages_per_window":    fmt.Sprintf("%d", l.maxPagesPerWindow),
					"chunk_max_records":       fmt.Sprintf("%d", l.chunkMaxRecords),
					"job_poll_timeout":        humanDur(l.jobPollTimeout),
					"download_timeout":        humanDur(l.downloadTimeout),
					"requested_data":          strings.Join(defaultRequestedData(), ","),
					"extra_record_fields":     "cache_status",
					"extra_indexed_fields":    "",
					"metadata_record_fields":  "",
					"metadata_trace_id_field": "",
					"trace_id_field":          "",
					"workspace_id":            "<your-workspace-id>",
					"signed_url_allow_hosts":  "ai-gateway-dataservice-us-prod.s3.us-west-2.amazonaws.com",
				},
			},
		},
	}
}

// ExampleSettingsComments returns one explanatory one-liner per settings key across both Portkey
// loops documented in ExampleSource (groups + logs_export). The helmgen renderer attaches these as
// YAML head-comments on each settings entry in the chart example block so operators see the intent.
func ExampleSettingsComments() map[string]string {
	return map[string]string{
		// groups loop knobs
		"window_span":        "groups: trailing query window [now-window_span, now-settle]",
		"emit_cost":          "groups: emit the per-dimension cost gauge in USD (÷100 from Portkey's cents); ON by default",
		"emit_prompts":       "groups: per-prompt request dimension (adds requests_by_prompt{prompt}); default ON, set false to opt out. Prompt slug is a content-free identifier, not content.",
		"metadata_keys":      "groups: opt-in per-metadata-field group endpoints (csv); each adds a _by_metadata gauge",
		"max_groups":         "groups: per-poll cap on distinct dimension rows (cardinality guard)",
		"expected_workspace": "analytics/groups SAFETY (optional): assert the API key's analytics scope is EXACTLY this workspace (slug, e.g. ws-acme-001) before emitting — catches a too-broad/global key (which Portkey can't request-scope on analytics). Mismatch ⇒ refuse to emit + alert; empty ⇒ no check. Set the same value on analytics+groups.",
		"api_key_ids":        "analytics/groups FILTER (optional): scope every graph/group to these Portkey api-key UUIDs (csv), exactly like the notebook's api_key_ids filter. Empty ⇒ workspace-wide (all keys). Set the SAME value on analytics+groups so the per-key aggregates and the per-model/metadata breakdowns agree. NOTE the UUID is the api-key's id (from GET /api-keys), NOT the key secret — and a read-only poller key has no inference traffic of its own, so point this at the key(s) your application actually calls Portkey with.",
		// logs_export loop knobs
		"window":                  "logs_export: per-export query span; the scheduler is snapshot-gated (LoopConfig.Window==0)",
		"max_backfill":            "logs_export: watermark older than now-max_backfill skipped LOUD. Cap ≤ Loki reject_old_samples_max_age (GC default 7d). Default 24h.",
		"page_size":               "page size per fetch (groups: dimension rows; logs_export: records per export page, server ceiling 50000)",
		"max_pages_per_window":    "logs_export: tripwire — a window exceeding this pages advances-past with a counted gap (never stalls)",
		"chunk_max_records":       "logs_export: per-Collect emit chunk (bounds memory; the export file is never buffered whole)",
		"job_poll_timeout":        "logs_export: abandon a stuck job after this interval (cancel + restart the window)",
		"download_timeout":        "logs_export: signed-URL object GET timeout (S3 downloads are larger than control-plane calls)",
		"requested_data":          "logs_export: fields requested of Portkey export (NOT an egress filter — the client-side strip governs egress)",
		"extra_record_fields":     "logs_export: opt content-free fields into the structured-metadata allow-list (csv); bodies hard-denied",
		"extra_indexed_fields":    "logs_export: promote content-free fields to the INDEXED (Loki stream-label) tier (csv, default empty); auto-allow-listed in the guard; creates Loki streams (keep LOW cardinality — the per-metric budget is the backstop); needs GS1 stack-side promotion to be queryable as {label=…}; bodies hard-denied",
		"metadata_record_fields":  "logs_export: lift named sub-keys OUT of the (otherwise hard-denied) Portkey metadata blob into the structured-metadata tier (csv, default empty) — e.g. correlation_id. Only operator-named content-free sub-keys are extracted; the rest of metadata (PII) stays dropped. Bodies hard-denied.",
		"metadata_trace_id_field": "logs_export: name ONE metadata sub-key whose UUID value is also mapped to the OTLP log trace_id (logs↔traces correlation), e.g. correlation_id (default empty). Auto-lifted into the record tier too. Bodies hard-denied.",
		"trace_id_field":          "logs_export: name ONE TOP-LEVEL export field (e.g. Portkey's native trace_id) whose UUID value is mapped to the OTLP log trace_id (logs↔traces correlation), default empty. The alternative to metadata_trace_id_field for deployments that stamp the trace id as a first-class field; the two are mutually exclusive. Auto-added to the record tier + requested_data. A non-UUID value leaves trace_id unset (counted via trace_id_unparsed) but still ships as a record attr.",
		"workspace_id":            "logs_export: REQUIRED — Portkey workspace id",
		"signed_url_allow_hosts":  "logs_export: REQUIRED — allow-list of S3 hosts for the signed download URL (SSRF guard)",
		// shared keys (both loops)
		"settle": "exclude the still-mutating recent tail from the query upper bound",
	}
}
