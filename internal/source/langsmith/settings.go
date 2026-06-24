// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/grafana-ps/aip-oi/internal/config"
)

// sessionsDefaults holds the package-canonical defaults for the sessions loop — the single source of
// truth shared by New (initial Config construction) and ExampleSource (rendered chart values).
// Changing a default here updates both paths atomically; no silent drift.
type sessionsDefaults struct {
	StatsWindow       time.Duration
	UseApproxStats    bool
	SessionLabelValue string
	PageLimit         int
	MaxSessions       int
	EmitFeedback      bool
}

func defaultSessionsSettings() sessionsDefaults {
	return sessionsDefaults{
		StatsWindow: time.Hour, UseApproxStats: false,
		SessionLabelValue: "id",
		PageLimit:         100, MaxSessions: 1000, EmitFeedback: true,
	}
}

// applySettings overlays the decoupled per-loop `settings` map (config.LoopConfig.Settings) onto cfg,
// parsing the LangSmith-specific knobs. The carrier is vendor-neutral string values (so no LangSmith
// field name leaks into internal/config); this package owns the key names and their types. An UNKNOWN
// key is ignored (forward-compatible); a MALFORMED value for a known key is a hard error — never
// silently fall back to a default and hide operator intent (operationally honest).
func applySettings(cfg *Config, s map[string]string) error {
	for k, v := range s {
		var err error
		switch k {
		case "stats_window":
			cfg.StatsWindow, err = time.ParseDuration(v)
		case "use_approx_stats":
			cfg.UseApproxStats, err = strconv.ParseBool(v)
		case "session_filter":
			cfg.SessionFilter = v
		// NOTE: session_label_key is deliberately NOT settable — the per-session label key is fixed at
		// "session" to match the composition-root guard allow-list. A configurable key would silently be
		// dropped by the default-deny guard if it didn't match (AR-MED). Cardinality is tuned via
		// session_label_value (id|name) + session_filter + max_sessions, not by renaming the label.
		case "session_label_value":
			cfg.SessionLabelValue = v
		case "page_limit":
			if cfg.PageLimit, err = strconv.Atoi(v); err == nil && cfg.PageLimit <= 0 {
				err = fmt.Errorf("must be > 0")
			}
		case "max_sessions":
			if cfg.MaxSessions, err = strconv.Atoi(v); err == nil && cfg.MaxSessions <= 0 {
				err = fmt.Errorf("must be > 0")
			}
		case "emit_feedback":
			cfg.EmitFeedback, err = strconv.ParseBool(v)
		case "feedback_keys":
			cfg.FeedbackKeys = splitCSV(v)
		default:
			// Unknown key: don't fail (a newer chart/config may carry keys an older binary doesn't know),
			// but WARN — a silently-ignored key is usually a typo (e.g. max_session vs max_sessions), and
			// silent misconfig is exactly what "operationally honest" forbids.
			slog.Warn("langsmith: ignoring unknown setting key", "key", k, "source_instance", cfg.SourceInstance)
		}
		if err != nil {
			return fmt.Errorf("langsmith: setting %q=%q: %w", k, v, err)
		}
	}
	return nil
}

// splitCSV trims and drops empties; returns nil for an all-empty input (so the zero value means "all").
func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// humanDur formats a duration as a compact human string ("24h", "5m", "1h30m") for chart examples —
// time.Duration.String() emits "24h0m0s", ugly and inconsistent with the chart's "24h"/"10m" style.
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

// ExampleSource returns a documented, buildable example LangSmith source for the Helm chart generator
// to render. It lives HERE (not in internal/config) so the vendor-specific shape — endpoint, auth
// header, the "sessions" loop key, and the settings keys — stays in the vendor package, keeping core
// decoupled. The generator renders this value into a (commented-out) chart example; the chart default
// itself stays portkey-only.
func ExampleSource() config.SourceConfig {
	d := defaultRunsSettings()
	sd := defaultSessionsSettings()
	return config.SourceConfig{
		Type:           "langsmith",
		Enabled:        true, // shown enabled as a copy-ready example; the chart default source stays portkey
		BaseURL:        "https://api.smith.langchain.com",
		SourceInstance: "langsmith-${ENV}",
		Auth:           config.AuthConfig{Header: "x-api-key", Value: "${LANGSMITH_API_KEY}"},
		// rps 10 / burst 20: validated against the real instance — the published "~10 req/10s" limit is
		// not what the API enforces in practice. A lower-tier tenant should reduce this if it sees 429s.
		RateLimit: config.RateLimitConfig{RPS: 10, Burst: 20},
		Loops: map[string]config.LoopConfig{
			// Aggregate-now (snapshot) loop: no window/settle/backfill — those are time-bucket concepts.
			"sessions": {
				Enabled:      true,
				Cadence:      config.Duration(time.Minute),
				MetricPrefix: "langsmith",
				Settings: map[string]string{
					"stats_window":        humanDur(sd.StatsWindow),
					"use_approx_stats":    fmt.Sprintf("%v", sd.UseApproxStats),
					"session_filter":      `eq(name, "my-project")`,
					"session_label_value": sd.SessionLabelValue,
					"page_limit":          fmt.Sprintf("%d", sd.PageLimit),
					"max_sessions":        fmt.Sprintf("%d", sd.MaxSessions),
					"emit_feedback":       fmt.Sprintf("%v", sd.EmitFeedback),
					"feedback_keys":       "correctness,helpfulness",
				},
			},
			// Forward-only windowed log pull: per-run content-free OTLP logs (→ Loki). Window==0 (snapshot-
			// scheduled; the real span is settings.window). Scope is REQUIRED — session_ids (static) OR
			// session_filter (auto-discovery); never pulls all projects. Indexed attrs (run_type/status)
			// need GS1 stream-label promotion to be queryable as {label=…}.
			"runs": {
				Enabled: true,
				Cadence: config.Duration(time.Minute),
				Settings: map[string]string{
					"session_ids":          "<project-uuid-1>,<project-uuid-2>",
					"session_filter":       `eq(name, "my-project")`,
					"max_sessions":         fmt.Sprintf("%d", d.maxSessions),
					"window":               humanDur(d.window),
					"settle":               humanDur(d.settle),
					"max_backfill":         humanDur(d.maxBackfill),
					"session_refresh":      humanDur(d.sessionRefresh),
					"page_size":            fmt.Sprintf("%d", d.pageSize),
					"max_pages_per_window": fmt.Sprintf("%d", d.maxPagesPerWindow),
					"max_response_bytes":   fmt.Sprintf("%d", d.maxResponseBytes),
					"root_only":            "false",
					"run_type":             "chain",
					// Content governance is content-free by DEFAULT. To emit extra content-free fields as Loki
					// structured metadata, opt them into the RECORD allow-list here (csv). Operational fields and
					// scalar arrays (rendered as csv) are safe; free-text (error/name/previews) is an explicit
					// content decision; message bodies (inputs/outputs/messages) are hard-denied and rejected.
					"extra_record_fields":  "tags,child_run_ids,app_path",
					"extra_indexed_fields": "",
				},
			},
		},
	}
}

// ExampleSettingsComments returns one explanatory one-liner per settings key across both LangSmith
// loops (sessions + runs). Shared keys (max_sessions, session_filter, extra_record_fields) carry a
// single comment that covers both loops. The helmgen renderer attaches these as YAML head-comments on
// each settings entry in the chart example block so operators see the intent inline.
func ExampleSettingsComments() map[string]string {
	return map[string]string{
		// sessions loop knobs
		"stats_window":        "sessions: rolling aggregate window [now-stats_window, now] (snapshot; not time-bucketed)",
		"use_approx_stats":    "sessions: use approximate (cheaper) backend stats computation",
		"session_label_value": "sessions: per-session label value — \"id\" (default, bounded) or \"name\" (human-readable but potentially high-cardinality)",
		"page_limit":          "sessions: page size for the sessions list endpoint (offset/limit pagination)",
		"emit_feedback":       "sessions: emit numeric feedback_stats gauges (score averages by feedback key)",
		"feedback_keys":       "sessions: optional allow-list of feedback keys to emit (csv, content-free operational names); empty = all numeric keys",
		// runs loop knobs
		"session_ids":          "runs: static scope (csv of project UUIDs). EITHER this OR session_filter is REQUIRED — never firehose all projects.",
		"window":               "runs: trailing query span pulled per poll (Window==0 ⇒ snapshot-scheduled)",
		"settle":               "runs: exclude the still-running/late tail — runs within settle of now may still be mutating",
		"max_backfill":         "runs: watermark older than now-max_backfill is skipped LOUD (counted gap). Cap ≤ Loki reject_old_samples_max_age (Grafana Cloud default 7d). Default 24h.",
		"session_refresh":      "runs: auto-discovery session cache TTL (re-queries /sessions after this interval)",
		"page_size":            "runs: page size for runs/query (server ceiling 100)",
		"max_pages_per_window": "runs: drain cap per window; a window exceeding this advances-past with a counted gap (never stalls)",
		"max_response_bytes":   "runs: per-page decode cap (default 32MiB); an oversize page is a loud counted truncation.",
		"root_only":            "runs: when true, only emit root runs (one log per trace); reduces volume for trace-level correlation",
		"run_type":             "runs: optional single run_type filter (e.g. chain, llm, tool); empty = all types",
		// shared keys (both loops)
		"session_filter":       "filter-bounded session auto-discovery via GET /sessions (alternative to session_ids for the runs loop; also scopes the sessions loop).",
		"max_sessions":         "cap on discovered/used sessions (projects) per poll (loud truncation when hit).",
		"extra_record_fields":  "opt content-free fields into the structured-metadata allow-list (csv); message bodies are hard-denied.",
		"extra_indexed_fields": "runs: promote content-free fields to the INDEXED (Loki stream-label) tier (csv, default empty); auto-allow-listed in the guard; creates Loki streams (keep LOW cardinality — the per-metric budget is the backstop); needs GS1 stack-side promotion to be queryable as {label=…}; message bodies are hard-denied.",
	}
}
