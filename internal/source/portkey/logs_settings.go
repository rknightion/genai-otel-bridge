// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// portkeyPageSizeMax is Portkey's hard per-export page_size ceiling (PoC §5: a 1,022,784-match export
// delivered exactly 50,000 lines). Requesting more is silently capped server-side, so reject it at
// config time rather than mis-compute the page count.
const portkeyPageSizeMax = 50000

// logsSettings are the decoupled knobs the logs-export loop reads from its per-loop `settings` map — the
// SAME pattern as groups/langsmith, so no vendor field names leak into internal/config. Defaults are
// applied by newLogsExportLoop before the operator overlay; applyLogsSettings then validates the final
// set (loud fail-fast on a misconfig, the analytics-positive-window convention).
type logsSettings struct {
	window, settle      time.Duration // window = the per-export query span; settle = exclude the still-mutating tail
	maxBackfill         time.Duration // floor: a watermark older than now-maxBackfill is unstorable (Loki accept horizon) → skip-loud
	pageSize            int           // current_page page size (≤ portkeyPageSizeMax)
	maxPagesPerWindow   int           // tripwire: a window producing more pages than this is mis-sized → loud error, no silent drop
	chunkMaxRecords     int           // per-Collect emit chunk (bounds memory; the file is never buffered whole)
	jobPollTimeout      time.Duration // abandon (cancel + restart the window) a job still running past this
	downloadTimeout     time.Duration // signed-URL object GET timeout (downloads are larger than control-plane calls)
	requestedData       []string      // content-free fields ASKED of Portkey — NOT an egress filter (PoC §3); we strip regardless
	extraRecordFields   []string      // operator opt-in: extra content-free fields → strip RECORD allow-list (also added to requested_data)
	extraIndexedFields  []string      // operator opt-in: extra content-free fields → strip INDEXED tier (Loki stream-label candidate; auto-allow-listed in the guard; also added to requested_data; GS1-gated)
	signedURLAllowHosts []string      // signed-URL host allow-list (SSRF, §7) — REQUIRED non-empty when enabled
	workspaceID         string        // export filter scope — REQUIRED when enabled
	// metadataRecordFields names sub-keys to LIFT OUT of the Portkey-injected `metadata` object into the
	// strip RECORD tier (the only sanctioned path into `metadata` — operator-named, content-free sub-keys
	// only, e.g. a correlation_id). NOT added to requested_data: we never ASK for the metadata blob (a
	// minimisation violation, contentRequestKeys) — Portkey injects it regardless and we extract client-side.
	metadataRecordFields []string
	// metadataTraceIDField names the ONE metadata sub-key whose UUID value also populates the OTLP
	// LogRecord.TraceID (logs↔traces correlation). Empty = no trace mapping; auto-lifted into the record tier.
	metadataTraceIDField string
	// traceIDField names the ONE TOP-LEVEL export field (e.g. Portkey's native `trace_id`) whose UUID value
	// populates the OTLP LogRecord.TraceID — the alternative to metadataTraceIDField for deployments that
	// stamp the trace id as a first-class field rather than a metadata sub-key. Auto-added to the record
	// allow-list + requested_data. Mutually exclusive with metadataTraceIDField (one OTLP trace_id source).
	traceIDField string
}

// hardDeniedLogFields are fields that must NEVER be opt-in-able via extra_record_fields. Opting one in
// would either make the composition-root guard drop the WHOLE record (a floor key fails okLog) or — for
// the Portkey-specific bare `prompt` (NOT on the guard floor, which carries the OTel `gen_ai.prompt`) —
// LEAK content, since the strip would allow-list it. It is the SHARED content floor
// (source.AbsoluteNeverDenyKeys — the SAME set the app guard denies, so the fail-fast mirror can't drift)
// PLUS the Portkey content fields (contentRequestKeys: bare prompt + request/response/messages/…).
var hardDeniedLogFields = func() map[string]bool {
	m := set(source.AbsoluteNeverDenyKeys()...)
	for k := range contentRequestKeys {
		m[k] = true
	}
	return m
}()

// contentRequestKeys are fields that must NEVER be named in requested_data (defence-in-depth beyond the
// egress strip): message bodies + the PoC §3 injected PII/config blobs. Requesting them can't widen what
// we EMIT (the strip allow-list is authoritative), but asking for them at all is a content-minimisation
// violation (followup §1) — so a config that lists one fails fast, loudly.
var contentRequestKeys = map[string]bool{
	"prompt": true, "request": true, "response": true, "messages": true,
	"inputs": true, "outputs": true, "metadata": true, "portkeyHeaders": true,
}

// defaultRequestedData is the PoC §3 content-free field set (= the strip allow-list's keys). Asked of
// Portkey to reduce what it computes; egress safety is still enforced by the strip + the guard.
func defaultRequestedData() []string {
	p := defaultLogFieldPolicy()
	out := make([]string, 0, len(p.indexed)+len(p.record))
	for k := range p.indexed {
		out = append(out, k)
	}
	for k := range p.record {
		out = append(out, k)
	}
	sort.Strings(out) // deterministic (stable create-request body across leaders)
	return out
}

// defaultLogsSettings is the pre-overlay default set (the two safety-required keys — workspace_id and
// signed_url_allow_hosts — are intentionally absent so an unset config fails validation loudly).
//
// jobPollTimeout=30m / downloadTimeout=5m are sized for LOAD: a live probe (followup §9, 2026-06-21)
// measured a FULL 50,000-record export taking ~10-20 min to GENERATE server-side at Portkey — longer than
// the old 10m abandon deadline, which would have cancelled+restarted a legitimate busy-window page forever
// (window never advances). 30m gives headroom over the observed worst case; the loop stays honest while it
// waits (empty no-advance batches, growing window_lag, the export_stuck metric only fires PAST the
// deadline). downloadTimeout 5m covers the signed-URL chunk read for a large page (incl. the per-chunk
// re-download skip) on a non-in-region link; in-region (the expected EKS→S3 us-west-2 path) it is seconds.
// A deployment whose windows stay well under 50k/page can lower both; one with even heavier traffic should
// shrink `window` so pages don't approach the 50k cap rather than raise these further.
func defaultLogsSettings() logsSettings {
	return logsSettings{
		window: time.Hour, settle: 10 * time.Minute, maxBackfill: 24 * time.Hour,
		pageSize: portkeyPageSizeMax, maxPagesPerWindow: 50, chunkMaxRecords: 5000,
		jobPollTimeout: 30 * time.Minute, downloadTimeout: 5 * time.Minute,
		requestedData:     defaultRequestedData(),
		extraRecordFields: []string{"cache_status"}, // Portkey cache hit/miss — content-free operational, on by default
	}
}

func applyLogsSettings(ls *logsSettings, s map[string]string) error {
	for k, v := range s {
		var err error
		switch k {
		case "window":
			ls.window, err = parsePositiveDuration("window", v)
		case "settle":
			ls.settle, err = parseNonNegDuration("settle", v)
		case "max_backfill":
			ls.maxBackfill, err = parsePositiveDuration("max_backfill", v)
		case "job_poll_timeout":
			ls.jobPollTimeout, err = parsePositiveDuration("job_poll_timeout", v)
		case "download_timeout":
			ls.downloadTimeout, err = parsePositiveDuration("download_timeout", v)
		case "page_size":
			ls.pageSize, err = parsePositiveInt("page_size", v)
		case "max_pages_per_window":
			ls.maxPagesPerWindow, err = parsePositiveInt("max_pages_per_window", v)
		case "chunk_max_records":
			ls.chunkMaxRecords, err = parsePositiveInt("chunk_max_records", v)
		case "requested_data":
			ls.requestedData = splitCSV(v)
		case "extra_record_fields":
			ls.extraRecordFields = splitCSV(v)
		case "extra_indexed_fields":
			ls.extraIndexedFields = splitCSV(v)
		case "metadata_record_fields":
			ls.metadataRecordFields = splitCSV(v)
		case "metadata_trace_id_field":
			ls.metadataTraceIDField = v
		case "trace_id_field":
			ls.traceIDField = v
		case "signed_url_allow_hosts":
			ls.signedURLAllowHosts = splitCSV(v)
		case "workspace_id":
			ls.workspaceID = v
		default:
			slog.Warn("portkey logs_export: ignoring unknown setting", "key", k)
		}
		if err != nil {
			return err
		}
	}
	return validateLogsSettings(ls)
}

// validateLogsSettings fails fast on a misconfig (loud, never a silent no-op). The two SAFETY gates —
// a non-empty signed-URL host allow-list and a content-free requested_data — are hard requirements: a
// server-controlled download URL with no host allow-list is an SSRF hole, and naming a content field is
// a minimisation violation. workspace_id is required (an export with no scope is meaningless).
func validateLogsSettings(ls *logsSettings) error {
	if ls.window <= 0 {
		return fmt.Errorf("portkey logs_export: window must be positive, got %s", ls.window)
	}
	if ls.pageSize > portkeyPageSizeMax {
		return fmt.Errorf("portkey logs_export: page_size %d exceeds Portkey max %d", ls.pageSize, portkeyPageSizeMax)
	}
	if ls.chunkMaxRecords <= 0 {
		return fmt.Errorf("portkey logs_export: chunk_max_records must be positive, got %d", ls.chunkMaxRecords)
	}
	if ls.workspaceID == "" {
		return fmt.Errorf("portkey logs_export: workspace_id is required")
	}
	if len(ls.signedURLAllowHosts) == 0 {
		return fmt.Errorf("portkey logs_export: signed_url_allow_hosts is required (the download URL is a server-controlled input — refusing to fetch from an unvalidated host)")
	}
	for _, f := range ls.requestedData {
		if contentRequestKeys[f] {
			return fmt.Errorf("portkey logs_export: requested_data must not name content/PII field %q (content minimisation)", f)
		}
	}
	for _, f := range ls.extraRecordFields {
		if hardDeniedLogFields[f] {
			return fmt.Errorf("portkey logs_export: extra_record_fields cannot include %q — it is a hard-denied content/PII field (the guard would drop the whole record or it would leak)", f)
		}
	}
	for _, f := range ls.extraIndexedFields {
		if hardDeniedLogFields[f] {
			return fmt.Errorf("portkey logs_export: extra_indexed_fields cannot include %q — it is a hard-denied content/PII field (a body must never become a Loki stream label)", f)
		}
	}
	for _, f := range ls.metadataRecordFields {
		if hardDeniedLogFields[f] {
			return fmt.Errorf("portkey logs_export: metadata_record_fields cannot include %q — it is a hard-denied content/PII field (only content-free metadata sub-keys may be lifted out of the metadata blob)", f)
		}
	}
	if f := ls.metadataTraceIDField; f != "" {
		if hardDeniedLogFields[f] {
			return fmt.Errorf("portkey logs_export: metadata_trace_id_field cannot be %q — it is a hard-denied content/PII field", f)
		}
		if strings.Contains(f, ",") {
			return fmt.Errorf("portkey logs_export: metadata_trace_id_field must name a SINGLE metadata sub-key, got %q (it is not csv — use metadata_record_fields for multiple)", f)
		}
	}
	if f := ls.traceIDField; f != "" {
		if hardDeniedLogFields[f] {
			return fmt.Errorf("portkey logs_export: trace_id_field cannot be %q — it is a hard-denied content/PII field", f)
		}
		if strings.Contains(f, ",") {
			return fmt.Errorf("portkey logs_export: trace_id_field must name a SINGLE top-level export field, got %q (it is not csv)", f)
		}
		if ls.metadataTraceIDField != "" {
			return fmt.Errorf("portkey logs_export: trace_id_field and metadata_trace_id_field are mutually exclusive — both map to the OTLP trace_id; set exactly one (top-level field vs metadata sub-key)")
		}
	}
	if len(ls.extraIndexedFields) > 0 {
		// Promoting fields to the INDEXED tier creates Loki streams; an un-bounded field explodes
		// cardinality. The per-loop guard budget is the runtime backstop (over-budget indexed signatures
		// are dropped + alerted), but warn loudly so the operator sizes the field before it bites.
		slog.Warn("portkey logs_export: extra_indexed_fields promotes fields to the indexed (Loki stream-label) tier — ensure they are LOW cardinality; the per-loop cardinality budget will drop over-budget series",
			"extra_indexed_fields", ls.extraIndexedFields)
	}
	return nil
}

func parsePositiveDuration(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("portkey logs_export: %s: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("portkey logs_export: %s must be positive, got %s", name, d)
	}
	return d, nil
}

func parseNonNegDuration(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("portkey logs_export: %s: %w", name, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("portkey logs_export: %s must be >= 0, got %s", name, d)
	}
	return d, nil
}

func parsePositiveInt(name, v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("portkey logs_export: %s: %w", name, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("portkey logs_export: %s must be positive, got %d", name, n)
	}
	return n, nil
}

// newLogsExportLoop builds the stateful logs-export loop. It SHARES the control-plane httpx client (one
// rate limiter across all portkey loops) for the lifecycle calls, and builds a SEPARATE download client
// whose host allow-list is signed_url_allow_hosts — so the S3 (or in-VPC) object host is policed by the
// httpx egress guard too (defence-in-depth beyond the explicit per-URL check). The download client is to
// a DIFFERENT host (AWS S3, not Portkey's API), so it carries its own limiter and a longer timeout.
//
// uc is the resolved use-case for this fan-out instance. Zero value (uc == resolvedUseCase{}) ⇒ the
// legacy unlabelled path: no api_key_use_case record attr, no api_key_ids filter, unchanged Key().
func newLogsExportLoop(cfg config.SourceConfig, lpCfg config.LoopConfig, deps source.Deps, hc *httpx.Client, uc resolvedUseCase) (*logsExportLoop, error) {
	// [final-review MEDIUM] The logs loop is SNAPSHOT-scheduled: it relies on LoopConfig.Window==0 so the
	// scheduler does not accelerate its ticks or count backfill_unstorable (the real per-export window is
	// settings.window). A stray non-zero LoopConfig.Window (e.g. copied from an analytics block) would
	// silently mis-signal the scheduler — so reject it fast, the symmetric guard to the analytics loop's
	// "require a positive window".
	if w := time.Duration(lpCfg.Window); w != 0 {
		return nil, fmt.Errorf("portkey logs_export: loops.logs_export.window must be unset/0 (snapshot-scheduled; the per-export window is settings.window); got %s", w)
	}
	ls := defaultLogsSettings()
	if err := applyLogsSettings(&ls, lpCfg.Settings); err != nil {
		return nil, err
	}
	cadence := time.Duration(lpCfg.Cadence)
	if cadence <= 0 {
		cadence = time.Minute // defensive; the config validator already enforces the cadence floor
	}
	ua := cfg.HTTP.UserAgent
	if ua == "" {
		ua = "genai-otel-bridge/0.1"
	}
	dl := httpx.New(httpx.Config{
		UserAgent: ua, Timeout: ls.downloadTimeout,
		AllowHosts: ls.signedURLAllowHosts, AllowPrivate: cfg.HTTP.AllowPrivate,
		Limiter:  rate.NewLimiter(rate.Limit(cfg.RateLimit.RPS), cfg.RateLimit.Burst),
		Observer: deps.UpstreamObserver,
	})
	// The content-free default policy + any operator-opted-in record fields (validated above to exclude
	// hard-denied content). The opted-in fields are also added to requested_data so Portkey actually
	// includes them in the export (else the strip allow-lists a field the export never carried); both
	// sets are content-free so the requested_data minimisation invariant holds.
	//
	// CRITICAL: set policy.useCase on a SEPARATE line AFTER the with* builder chain — the builders each
	// return a fresh fieldPolicy{...} literal that does NOT copy useCase, so threading it through the
	// chain would silently drop it. Zero value (uc.slug=="") ⇒ no-op in stampUseCaseRecord (legacy path).
	policy := defaultLogFieldPolicy().withExtraRecordFields(ls.extraRecordFields).withExtraIndexedFields(ls.extraIndexedFields).
		withMetadataFields(ls.metadataRecordFields, ls.metadataTraceIDField).withTraceIDField(ls.traceIDField)
	policy.useCase = uc.slug
	// trace_id_field names a top-level export field (unlike a metadata sub-key, which Portkey injects), so
	// ask Portkey to include it; content-free (validated) so the requested_data minimisation invariant holds.
	requested := mergeSortedUnique(ls.requestedData, ls.extraRecordFields, ls.extraIndexedFields, []string{ls.traceIDField})
	return &logsExportLoop{
		baseURL: cfg.BaseURL, authHdr: cfg.Auth.Header, authVal: cfg.Auth.Value,
		hc: hc, dlClient: dl, sourceInstance: cfg.SourceInstance, workspaceID: ls.workspaceID,
		cadence: cadence, window: ls.window, settle: ls.settle, maxBackfill: ls.maxBackfill,
		pageSize: ls.pageSize, maxPagesPerWindow: ls.maxPagesPerWindow, chunkMaxRecords: ls.chunkMaxRecords,
		jobPollTimeout: ls.jobPollTimeout, requestedData: requested, signedURLAllowHosts: ls.signedURLAllowHosts,
		policy:         policy,
		useCase:        uc.slug,
		apiKeyIDs:      uc.apiKeyIDsCSV,
		onGraphSkipped: deps.OnGraphSkipped,
		onAuthError:    deps.OnAuthError,
		now:            func() time.Time { return time.Now().UTC() },
	}, nil
}

// mergeSortedUnique unions string slices into a sorted, de-duplicated slice (deterministic create-request
// body across leaders).
func mergeSortedUnique(lists ...[]string) []string {
	seen := map[string]bool{}
	for _, l := range lists {
		for _, s := range l {
			if s != "" {
				seen[s] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
