// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

const runsPageSizeMax = 100 // runs/query hard server `limit` ceiling (live-confirmed)

// runsSettings are the decoupled knobs the runs loop reads from its per-loop `settings` map — the SAME
// pattern as groups/logs_export/sessions, so no vendor field name leaks into internal/config. Defaults
// are applied by defaultRunsSettings before the operator overlay; applyRunsSettings then validates.
type runsSettings struct {
	sessionIDs         []string      // explicit static scope (project UUIDs); if set, wins over discovery
	sessionFilter      string        // LangSmith filter expr for auto-discovery (used when sessionIDs empty)
	maxSessions        int           // cap on discovered/used projects (loud truncation)
	window, settle     time.Duration // per-window query span; settle = exclude the still-running/late tail
	maxBackfill        time.Duration // floor: a watermark older than now-maxBackfill is skipped-loud
	pageSize           int           // runs/query `limit` (≤ runsPageSizeMax)
	maxPagesPerWindow  int           // drain cap: a window exceeding this advances-past with a counted gap
	maxResponseBytes   int64         // per-page decode cap (DoS backstop)
	sessionRefresh     time.Duration // auto-discovery in-memory cache TTL
	rootOnly           bool          // is_root=true (one log per trace)
	runType            string        // optional single run_type filter ("" = all)
	extraRecordFields  []string      // operator opt-in: extra content-free fields → strip RECORD allow-list
	extraIndexedFields []string      // operator opt-in: extra content-free fields → strip INDEXED tier (Loki stream-label candidate; auto-allow-listed in the guard; GS1-gated to be queryable)
}

// hardDeniedRunsFields are run fields that must NEVER be opt-in-able via extra_record_fields. Opting one
// in would make the composition-root guard drop the WHOLE record (a denied key fails okLog), silently
// emitting nothing — so reject it at config time, loud, honest (not a silent no-output). It is the SHARED
// content floor (source.AbsoluteNeverDenyKeys — message bodies + injected PII, the SAME set the app guard
// denies, so the fail-fast mirror cannot drift from the backstop) PLUS LangSmith-specific raw-content
// pointers: `inputs_s3_urls`/`outputs_s3_urls` are SIGNED URLs to the raw input/output blobs in S3 — a
// pointer to the message bodies plus a credential, strictly worse than the bodies; never opt-in-able.
var hardDeniedRunsFields = func() map[string]bool {
	m := set(source.AbsoluteNeverDenyKeys()...)
	m["inputs_s3_urls"] = true
	m["outputs_s3_urls"] = true
	return m
}()

func defaultRunsSettings() runsSettings {
	return runsSettings{
		maxSessions: 1000, window: time.Hour, settle: 10 * time.Minute, maxBackfill: 24 * time.Hour,
		pageSize: runsPageSizeMax, maxPagesPerWindow: 100, maxResponseBytes: defaultRunsResponseBytes,
		sessionRefresh: 5 * time.Minute,
		// Content-free operational LangSmith run fields, on by default (structured metadata, not labels):
		// run tags, child run ids (trace structure), and the app path.
		extraRecordFields: []string{"tags", "child_run_ids", "app_path"},
	}
}

func applyRunsSettings(rs *runsSettings, s map[string]string) error {
	for k, v := range s {
		var err error
		switch k {
		case "session_ids":
			rs.sessionIDs = splitCSV(v)
		case "session_filter":
			rs.sessionFilter = v
		case "max_sessions":
			rs.maxSessions, err = parseRunsPositiveInt(v)
		case "window":
			rs.window, err = parseRunsPositiveDuration(v)
		case "settle":
			rs.settle, err = parseRunsNonNegDuration(v)
		case "max_backfill":
			rs.maxBackfill, err = parseRunsPositiveDuration(v)
		case "session_refresh":
			rs.sessionRefresh, err = parseRunsNonNegDuration(v)
		case "page_size":
			rs.pageSize, err = parseRunsPositiveInt(v)
		case "max_pages_per_window":
			rs.maxPagesPerWindow, err = parseRunsPositiveInt(v)
		case "max_response_bytes":
			var n int
			if n, err = parseRunsPositiveInt(v); err == nil {
				rs.maxResponseBytes = int64(n)
			}
		case "root_only":
			rs.rootOnly, err = strconv.ParseBool(v)
		case "run_type":
			rs.runType = v
		case "extra_record_fields":
			rs.extraRecordFields = splitCSV(v)
		case "extra_indexed_fields":
			rs.extraIndexedFields = splitCSV(v)
		default:
			slog.Warn("langsmith runs: ignoring unknown setting", "key", k)
		}
		if err != nil {
			return fmt.Errorf("langsmith runs: setting %q=%q: %w", k, v, err)
		}
	}
	return validateRunsSettings(rs)
}

// validateRunsSettings fails fast (loud, never a silent no-op). The SCOPE gate is hard: runs/query 400s
// without a scope and pulling all 100+ projects is a firehose — so REQUIRE session_ids OR session_filter.
func validateRunsSettings(rs *runsSettings) error {
	if len(rs.sessionIDs) == 0 && rs.sessionFilter == "" {
		return fmt.Errorf("langsmith runs: session_ids or session_filter is required (runs/query must be scoped to projects; refusing to pull all projects)")
	}
	if rs.window <= 0 {
		return fmt.Errorf("langsmith runs: window must be positive")
	}
	if rs.pageSize > runsPageSizeMax {
		return fmt.Errorf("langsmith runs: page_size %d exceeds server max %d", rs.pageSize, runsPageSizeMax)
	}
	if rs.settle >= rs.window {
		return fmt.Errorf("langsmith runs: settle (%s) must be < window (%s)", rs.settle, rs.window)
	}
	for _, f := range rs.extraRecordFields {
		if hardDeniedRunsFields[f] {
			return fmt.Errorf("langsmith runs: extra_record_fields cannot include %q — it is a hard-denied message-body field (the content guard would drop the whole record)", f)
		}
	}
	for _, f := range rs.extraIndexedFields {
		if hardDeniedRunsFields[f] {
			return fmt.Errorf("langsmith runs: extra_indexed_fields cannot include %q — it is a hard-denied message-body field (a body must never become a Loki stream label)", f)
		}
	}
	// [#65] Validate opt-ins against the known select enum. Both extra_record_fields and
	// extra_indexed_fields are mirrored into the runs/query `select` projection (selectKeys), which the
	// server enum-validates: ANY value outside the accepted set 422s the WHOLE query — the runs loop then
	// emits zero logs and window_lag grows until a human decodes the 422 (a whole-loop outage from a
	// one-character typo like "app-path"). A table lookup at config-load turns that silent runtime firehose
	// into a loud fail-fast, consistent with the package's other fail-fast validations. The enum is the
	// live 0.13.5 snapshot; a newer server may accept more — refresh validLangsmithSelectEnum from a fresh
	// 422 probe if the server changes.
	for _, f := range rs.extraRecordFields {
		if !validLangsmithSelectEnum[f] {
			return fmt.Errorf("langsmith runs: extra_record_fields value %q is not a known LangSmith 0.13.5 select field — it would 422 the entire runs/query at runtime (check for a typo, or refresh validLangsmithSelectEnum for a newer server)", f)
		}
	}
	for _, f := range rs.extraIndexedFields {
		if !validLangsmithSelectEnum[f] {
			return fmt.Errorf("langsmith runs: extra_indexed_fields value %q is not a known LangSmith 0.13.5 select field — it would 422 the entire runs/query at runtime (check for a typo, or refresh validLangsmithSelectEnum for a newer server)", f)
		}
	}
	if len(rs.extraIndexedFields) > 0 {
		// Promoting fields to the INDEXED tier creates Loki streams; an un-bounded field explodes
		// cardinality. The per-loop guard budget is the runtime backstop (over-budget indexed signatures
		// are dropped + alerted), but warn loudly so the operator sizes the field before it bites.
		slog.Warn("langsmith runs: extra_indexed_fields promotes fields to the indexed (Loki stream-label) tier — ensure they are LOW cardinality; the per-loop cardinality budget will drop over-budget series",
			"extra_indexed_fields", rs.extraIndexedFields)
	}
	return nil
}

func parseRunsPositiveDuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", d)
	}
	return d, nil
}

func parseRunsNonNegDuration(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must be >= 0, got %s", d)
	}
	return d, nil
}

func parseRunsPositiveInt(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be positive, got %d", n)
	}
	return n, nil
}

// newRunsLoop builds the runs loop. It SHARES the control-plane httpx client (one rate limiter across
// all langsmith loops — the ~10 req/10s budget is tenant-wide). LoopConfig.Window MUST be 0 (the loop is
// snapshot-scheduled; the real per-window span is settings.window), mirroring the logs_export guard.
func newRunsLoop(cfg config.SourceConfig, lpCfg config.LoopConfig, deps source.Deps, hc *httpx.Client) (*runsLoop, error) {
	if w := time.Duration(lpCfg.Window); w != 0 {
		return nil, fmt.Errorf("langsmith runs: loops.runs.window must be unset/0 (snapshot-scheduled; the per-window span is settings.window); got %s", w)
	}
	rs := defaultRunsSettings()
	if err := applyRunsSettings(&rs, lpCfg.Settings); err != nil {
		return nil, err
	}
	cadence := time.Duration(lpCfg.Cadence)
	if cadence <= 0 {
		cadence = time.Minute // defensive; the config validator already enforces the cadence floor
	}
	// The content-free default policy + any operator-opted-in record fields and indexed-tier promotions
	// (both validated above to exclude hard-denied bodies). selectFields mirrors the ACTIVE allow-list
	// (indexed∪record) so opted-in fields are requested from runs/query.
	policy := defaultRunsFieldPolicy().withExtraRecordFields(rs.extraRecordFields).withExtraIndexedFields(rs.extraIndexedFields)
	return &runsLoop{
		baseURL: cfg.BaseURL, authHdr: cfg.Auth.Header, authVal: cfg.Auth.Value, hc: hc,
		sourceInstance: cfg.SourceInstance,
		sessionIDs:     rs.sessionIDs, sessionFilter: rs.sessionFilter, maxSessions: rs.maxSessions,
		selectFields: policy.selectKeys(),
		cadence:      cadence, window: rs.window, settle: rs.settle, maxBackfill: rs.maxBackfill,
		pageSize: rs.pageSize, maxPagesPerWindow: rs.maxPagesPerWindow, maxResponseBytes: rs.maxResponseBytes,
		sessionRefresh: rs.sessionRefresh, rootOnly: rs.rootOnly, runType: rs.runType,
		policy: policy, onGraphSkipped: deps.OnGraphSkipped, onAuthError: deps.OnAuthError,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}
