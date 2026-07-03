// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// groupsSettings are the decoupled knobs the groups loop reads from the per-loop `settings` map (so no
// vendor field names leak into internal/config — same pattern as the langsmith source). Defaults are
// applied by newGroupsLoop before the operator overlay.
type groupsSettings struct {
	windowSpan   time.Duration // trailing query window: [now-windowSpan, now-settle]
	settle       time.Duration // exclude the still-mutating recent tail from the query upper bound
	pageSize     int           // current_page page size
	maxGroups    int           // hard per-endpoint row cap (bounded coverage + offset-ignoring-server backstop)
	metadataKeys []string      // request-metadata group keys to poll (each → metadata/<key>); empty ⇒ none
	emitCost     bool          // emit the cost gauge in USD (÷100 from Portkey cents); default ON
	emitPrompts  bool          // emit the per-prompt request dimension (groups/prompt); default ON (opt out via emit_prompts:false)
	expectedWS   string        // optional: assert the key's analytics scope is exactly this workspace (scope.go)
	apiKeyIDs    string        // optional api-key UUID filter (CSV→comma-joined); scopes groups to those keys, as the notebook does. Empty ⇒ workspace-wide
}

// applyGroupsSettings overlays operator-set knobs onto g. A malformed KNOWN key fails fast (loud
// misconfig); an UNKNOWN key is warned and ignored (forward-compat, mirrors langsmith.applySettings).
func applyGroupsSettings(g *groupsSettings, s map[string]string) error {
	for k, v := range s {
		switch k {
		case "window_span":
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("portkey groups: window_span: %w", err)
			}
			if d <= 0 {
				return fmt.Errorf("portkey groups: window_span must be positive, got %s", d)
			}
			g.windowSpan = d
		case "settle":
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("portkey groups: settle: %w", err)
			}
			if d < 0 {
				return fmt.Errorf("portkey groups: settle must be >= 0, got %s", d)
			}
			g.settle = d
		case "page_size":
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("portkey groups: page_size: %w", err)
			}
			if n <= 0 {
				return fmt.Errorf("portkey groups: page_size must be positive, got %d", n)
			}
			g.pageSize = n
		case "max_groups":
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("portkey groups: max_groups: %w", err)
			}
			if n <= 0 {
				return fmt.Errorf("portkey groups: max_groups must be positive, got %d", n)
			}
			g.maxGroups = n
		case "metadata_keys":
			g.metadataKeys = splitCSV(v)
		case "emit_cost":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("portkey groups: emit_cost: %w", err)
			}
			g.emitCost = b
		case "emit_prompts":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("portkey groups: emit_prompts: %w", err)
			}
			g.emitPrompts = b
		case "expected_workspace":
			g.expectedWS = strings.TrimSpace(v)
		case "api_key_ids":
			g.apiKeyIDs = cleanAPIKeyIDs(v)
		default:
			slog.Warn("portkey groups: ignoring unknown setting", "key", k)
		}
	}
	// [review-H1] settle is subtracted from the window UPPER bound and window_span from the LOWER bound,
	// so settle >= window_span inverts the query window (time_of_generation_min > max) — a silent-empty or
	// confusing-degraded poll. Reject it fast (loud misconfig), mirroring the analytics positive-window
	// guard. (Checked here, after the overlay, so it sees the final values incl. defaults.)
	if g.settle >= g.windowSpan {
		return fmt.Errorf("portkey groups: settle (%s) must be < window_span (%s); otherwise the query window inverts", g.settle, g.windowSpan)
	}
	return nil
}

func splitCSV(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// defaultGroupsSettings returns the pre-overlay default knobs for the groups loop (mirrors
// defaultLogsSettings / defaultRunsSettings so ExampleSource can derive drift-proof defaults).
func defaultGroupsSettings() groupsSettings {
	return groupsSettings{windowSpan: time.Hour, settle: 10 * time.Minute, pageSize: 1000, maxGroups: 10000, emitCost: true, emitPrompts: true}
}

// newGroupsLoop builds the groups snapshot loop from the generic source config + its "groups"
// LoopConfig. The httpx client is SHARED with the analytics loop (one rate limiter ⇒ both loops stay
// within Portkey's tenant-wide request budget). ucs is the resolved set of use-case passes (nil when
// api_key_use_cases is empty — a single unlabelled pass using the legacy settings.api_key_ids is used).
func newGroupsLoop(cfg config.SourceConfig, lpCfg config.LoopConfig, deps source.Deps, hc *httpx.Client, ucs []resolvedUseCase) (*groupsLoop, error) {
	prefix := lpCfg.MetricPrefix
	if prefix == "" {
		prefix = "portkey_api"
	}
	gs := defaultGroupsSettings()
	if err := applyGroupsSettings(&gs, lpCfg.Settings); err != nil {
		return nil, err
	}
	cadence := time.Duration(lpCfg.Cadence)
	if cadence <= 0 {
		cadence = 5 * time.Minute // defensive; the config validator already enforces the cadence floor
	}
	// Build passes: if use-cases are configured, one pass per use-case; otherwise a single unlabelled
	// pass using the legacy settings.api_key_ids filter (backward compatible — Key() is unchanged).
	passes := ucs
	if len(passes) == 0 {
		passes = []resolvedUseCase{{slug: "", apiKeyIDsCSV: gs.apiKeyIDs}}
	}
	return &groupsLoop{
		baseURL: cfg.BaseURL, authHdr: cfg.Auth.Header, authVal: cfg.Auth.Value,
		hc: hc, prefix: prefix, sourceInstance: cfg.SourceInstance,
		cadence: cadence, windowSpan: gs.windowSpan, settle: gs.settle,
		pageSize: gs.pageSize, maxGroups: gs.maxGroups,
		metadataKeys: gs.metadataKeys, emitCost: gs.emitCost, emitPrompts: gs.emitPrompts,
		expectedWorkspace: gs.expectedWS,
		passes:            passes,
		onGraphSkipped:    deps.OnGraphSkipped,
		onAuthError:       deps.OnAuthError,
		now:               func() time.Time { return time.Now().UTC() },
	}, nil
}

// groupsLoop is the WINDOW-TOTAL (aggregate-now / snapshot) Portkey loop: each tick fetches a flat
// per-dimension total over a fixed trailing window and emits fresh gauges. It deliberately shares NONE
// of the analytics loop's bucket/settle/watermark/granularity/revision machinery — there are no buckets
// to settle, no per-bucket frontier to advance, and (because every poll stamps a fresh advancing
// timestamp) no Mimir duplicate-timestamp hazard. (groups PoC §a + the watermarking decision.)
type groupsLoop struct {
	baseURL, authHdr, authVal string
	hc                        *httpx.Client
	prefix, sourceInstance    string
	cadence, windowSpan       time.Duration
	settle                    time.Duration
	pageSize, maxGroups       int
	metadataKeys              []string
	emitCost                  bool
	emitPrompts               bool
	expectedWorkspace         string // optional key-scope guardrail (settings.expected_workspace); see scope.go
	// passes is the set of filtered passes to run each Collect. Empty use-cases ⇒ a single pass with
	// slug "" and the legacy settings.api_key_ids filter (backward compatible). Each pass stamps its slug.
	// Key() is NOT folded with the slug — one instance, one watermark (M7 ownership invariant).
	passes         []resolvedUseCase
	scopeVerified  bool // caches a passing scope check (Collect is single-flight, no lock)
	onGraphSkipped func(loop, graph string)
	onAuthError    func(loop, source string)
	now            func() time.Time
}

func (l *groupsLoop) Cadence() time.Duration { return l.cadence }

// SeriesNames declares the loop's output series for startup ownership validation (M7). The distinct
// `_by_model`/`_by_metadata` names never collide with the analytics aggregate names. Cost names appear
// only when emit_cost is set; metadata names only when at least one metadata key is configured.
func (l *groupsLoop) SeriesNames() []string {
	out := []string{groupMetricName(l.prefix, "requests", "model")}
	if l.emitCost {
		out = append(out, groupMetricName(l.prefix, "cost_usd", "model"))
	}
	if len(l.metadataKeys) > 0 {
		out = append(out, groupMetricName(l.prefix, "requests", "metadata"))
		if l.emitCost {
			out = append(out, groupMetricName(l.prefix, "cost_usd", "metadata"))
		}
	}
	if l.emitPrompts {
		out = append(out, groupMetricName(l.prefix, "requests", "prompt"))
	}
	return out
}

func (l *groupsLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{
		SourceInstance:    l.sourceInstance,
		Loop:              "groups",
		OutputFingerprint: model.Fingerprint(l.SeriesNames(), "prefix="+l.prefix),
	}
}

// groupsEndpoint is one dimension to poll: its URL path, a clean label for logs/skip-metric, the
// metric `_by_<dim>` suffix, the JSON field name that carries the dimension value (dimField), the
// row-value label key, and any fixed base labels (e.g. metadata_key).
type groupsEndpoint struct {
	urlPath       string
	label         string
	dim           string
	dimField      string // JSON field name for the dimension value (ai_model | metadata_value)
	valueLabelKey string
	baseLabels    map[string]string
	// emitCost gates the per-row cost gauge for THIS endpoint. It MUST stay in sync with SeriesNames():
	// deriveGroups emits <prefix>_cost_usd_by_<dim> whenever emitCost && a row carries a cost field, so a
	// dim whose cost name is NOT declared (e.g. prompt) must set emitCost=false — otherwise, if Portkey
	// ever adds cost to that dimension's rows, the loop would emit an UNDECLARED series that bypassed the
	// M7 ownership check (#104). Keeps emitted ⊆ declared, structurally.
	emitCost bool
}

func (l *groupsLoop) endpoints() []groupsEndpoint {
	eps := []groupsEndpoint{{urlPath: "ai-models", label: "ai-models", dim: "model", dimField: "ai_model", valueLabelKey: "ai_model", emitCost: l.emitCost}}
	for _, k := range l.metadataKeys {
		eps = append(eps, groupsEndpoint{
			urlPath:       "metadata/" + url.PathEscape(k),
			label:         "metadata/" + k,
			dim:           "metadata",
			dimField:      "metadata_value",
			valueLabelKey: "metadata_value",
			baseLabels:    map[string]string{"metadata_key": k},
			emitCost:      l.emitCost,
		})
	}
	if l.emitPrompts {
		eps = append(eps, groupsEndpoint{
			urlPath: "prompt", label: "prompt", dim: "prompt",
			dimField: "prompt", valueLabelKey: "prompt",
			// emitCost deliberately false: the prompt dimension carries no cost (live-probed 2026-06-22)
			// and SeriesNames() declares no cost_usd_by_prompt. Never emit an undeclared cost series (#104).
			emitCost: false,
		})
	}
	return eps
}

// Collect fetches each configured dimension INDEPENDENTLY over the fixed trailing window and emits
// fresh gauges stamped at the window upper bound (now-settle). A failed endpoint emits nothing for
// itself, is counted via OnGraphSkipped, and does NOT block the others (snapshot ⇒ no shared watermark
// to corrupt). Only when EVERY endpoint fails does Collect error (loud, no advance) — like the
// analytics all-404 case. The returned watermark is a forward-only liveness heartbeat (Time=now);
// `since` is intentionally unused (no replay frontier for a snapshot loop).
func (l *groupsLoop) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	now := l.now()
	if l.expectedWorkspace != "" && !l.scopeVerified {
		ok, err := verifyScopeForCollect(ctx, l.hc, l.baseURL, l.authHdr, l.authVal, l.expectedWorkspace, l.Key().Loop, l.sourceInstance, now, l.onGraphSkipped, l.onAuthError)
		if err != nil {
			return model.Batch{}, err // refuse to emit (mismatch) or retry (transient) — never silently advance
		}
		l.scopeVerified = ok
	}
	until := now.Add(-l.settle)
	from := now.Add(-l.windowSpan)
	// [review-M3] Stamp samples at MINUTE resolution. A snapshot loop emits a fresh point each poll; if
	// two polls land in the same wall-clock minute (sub-minute cadence, or the jitter edge at ~60s), two
	// DISTINCT sub-minute timestamps would push >1 point/series/minute past Mimir (CoalesceDPM is
	// per-batch and can't dedup across polls), violating the 1DPM cap. Truncating to the minute makes
	// same-minute re-polls share a timestamp (LWW/duplicate-timestamp dedup ⇒ exactly 1DPM) while the
	// FETCH window still uses the precise now-settle bound. (followup §0 1DPM; groups PoC watermark note.)
	stamp := until.Truncate(time.Minute)
	eps := l.endpoints()
	var samples []model.Sample
	skipped, total := 0, 0
	for _, p := range l.passes {
		for _, ep := range eps {
			total++
			rows, ok := l.collectEndpoint(ctx, ep, from, until, p.apiKeyIDsCSV)
			if !ok {
				skipped++
				if l.onGraphSkipped != nil {
					l.onGraphSkipped(l.Key().Loop, ep.label)
				}
				continue
			}
			s := deriveGroups(rows, l.prefix, ep.dim, ep.baseLabels, ep.valueLabelKey, ep.emitCost, stamp)
			stampUseCase(s, p.slug)
			samples = append(samples, s...)
		}
	}
	if skipped == total {
		return model.Batch{}, fmt.Errorf("portkey groups: all %d endpoint fetch(es) failed this poll — capability/permission/transient error, not empty data", total)
	}
	wm := since
	wm.Time = now
	return model.Batch{Key: l.Key(), Samples: samples, Watermark: wm}, nil
}

// collectEndpoint paginates one dimension ALL-OR-NOTHING: it accumulates rows across pages and returns
// (rows, true) on full success, or (nil, false) on ANY page error / non-200 / quota / parse failure —
// discarding the partial set (a snapshot is free + idempotent to re-fetch next tick). Pagination stops
// on an empty page (past the end), a short page (the last), the max_groups cap, or a page that adds no
// NEW dimension value (a backstop against a server that ignores current_page, so we never loop forever).
// apiKeyIDs is the per-pass UUID filter CSV (empty ⇒ workspace-wide, mirroring the legacy behaviour).
func (l *groupsLoop) collectEndpoint(ctx context.Context, ep groupsEndpoint, from, until time.Time, apiKeyIDs string) (rows []groupRow, ok bool) {
	seen := map[string]bool{}
	// [#140] Count rows dropped by the cross-page dedup. In normal operation (confirmed-string dim values,
	// an offset-respecting server) this stays 0. It becomes >0 when either the server ignores current_page
	// (returns overlapping pages) OR distinct dimension values COLLAPSE to the same string — most acutely
	// when a non-string dim decode falls back to "" (groups_derive.go), which would otherwise silently
	// discard every row after the first and report one arbitrary row's total as the whole endpoint. Report
	// it (Warn + OnGraphSkipped) on success so the collapse is alertable, never silent — we keep dedup-by-
	// first (NOT sum) so a genuinely repeated page can't double-count.
	dupDropped := 0
	defer func() {
		if ok && dupDropped > 0 {
			slog.Warn("portkey groups: dropped duplicate-dimension rows in cross-page dedup (possible dim-value collapse to \"\", or an offset-ignoring server)",
				"endpoint", ep.label, "dropped", dupDropped, "source", l.sourceInstance)
			if l.onGraphSkipped != nil {
				l.onGraphSkipped(l.Key().Loop, ep.label+"_dup_dim")
			}
		}
	}()
	// [review-M1] Hard page cap = ceil(maxGroups/pageSize): the infinite-loop backstop for a server that
	// IGNORES current_page (returns the same page forever). We do NOT terminate on "page added no new
	// dimension value" — that heuristic would SILENTLY early-truncate if the (unconfirmed) metadata row
	// shape ever collapses distinct values to one (e.g. an empty/mis-extracted dim field). Normal
	// termination is an empty or short page; the cap only bites a misbehaving server.
	maxPages := max((l.maxGroups+l.pageSize-1)/l.pageSize, 1)
	for page := 0; ; page++ {
		resp, code, err := l.fetchPage(ctx, ep.urlPath, from, until, page, apiKeyIDs)
		if err != nil {
			slog.Warn("portkey groups endpoint fetch error", "endpoint", ep.label, "page", page, "source", l.sourceInstance, "err", err)
			return nil, false
		}
		switch code {
		case http.StatusOK:
			// proceed
		case http.StatusNotFound:
			slog.Warn("portkey groups endpoint unavailable (capability)", "endpoint", ep.label, "source", l.sourceInstance)
			return nil, false
		default:
			// 401/403/429/5xx: discard this endpoint's batch this poll (no shared watermark to advance).
			slog.Warn("portkey groups endpoint status", "endpoint", ep.label, "status", code, "source", l.sourceInstance)
			if source.IsAuthStatus(code) && l.onAuthError != nil {
				l.onAuthError(l.Key().Loop, l.sourceInstance) // followup §9: credential failure = own signal
			}
			return nil, false
		}
		if resp.IsQuotaExceeded {
			slog.Warn("portkey groups endpoint quota exceeded", "endpoint", ep.label, "source", l.sourceInstance)
			return nil, false // mirror the analytics F34 discard, but per-endpoint
		}
		pageRows, err := parseGroupRows(resp.Data, ep.dimField)
		if err != nil {
			slog.Warn("portkey groups parse error", "endpoint", ep.label, "source", l.sourceInstance, "err", err)
			return nil, false
		}
		for _, r := range pageRows {
			if seen[r.dimValue] {
				dupDropped++ // #140: observable, not silent (see the defer)
				continue
			}
			seen[r.dimValue] = true
			rows = append(rows, r)
			if len(rows) >= l.maxGroups {
				slog.Warn("portkey groups truncated at max_groups cap", "endpoint", ep.label, "max_groups", l.maxGroups, "source", l.sourceInstance)
				return rows, true // bounded coverage — a success, just capped (logged, never silent)
			}
		}
		if len(pageRows) == 0 || len(pageRows) < l.pageSize {
			break // past the end (empty) or the last (short) page
		}
		if page+1 >= maxPages {
			slog.Warn("portkey groups hit page cap (server may be ignoring current_page)", "endpoint", ep.label, "max_pages", maxPages, "source", l.sourceInstance)
			break
		}
	}
	return rows, true
}

// fetchPage issues a single paginated request for the given groups endpoint path. apiKeyIDs is the
// per-pass UUID filter CSV (passed down from collectEndpoint; empty ⇒ workspace-wide).
func (l *groupsLoop) fetchPage(ctx context.Context, path string, from, until time.Time, page int, apiKeyIDs string) (groupsResponse, int, error) {
	u, err := url.Parse(l.baseURL + "/analytics/groups/" + path)
	if err != nil {
		return groupsResponse{}, 0, err
	}
	q := url.Values{}
	// time_of_generation_min/max are REQUIRED (omit ⇒ 400 AB01). We request ONLY aggregate + paging
	// params — never any content/PII field (groups carry no message bodies; this stays content-clean).
	q.Set("time_of_generation_min", from.UTC().Format(time.RFC3339))
	q.Set("time_of_generation_max", until.UTC().Format(time.RFC3339))
	q.Set("page_size", strconv.Itoa(l.pageSize))
	q.Set("current_page", strconv.Itoa(page))
	if apiKeyIDs != "" { // scope to specific api keys, as the notebook does (empty ⇒ workspace-wide)
		q.Set("api_key_ids", apiKeyIDs)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return groupsResponse{}, 0, err
	}
	req.Header.Set(l.authHdr, l.authVal)
	resp, err := l.hc.Do(req)
	if err != nil {
		return groupsResponse{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return groupsResponse{}, resp.StatusCode, nil
	}
	var gr groupsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&gr); err != nil {
		return groupsResponse{}, resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return gr, resp.StatusCode, nil
}

var (
	_ source.Loop           = (*groupsLoop)(nil)
	_ source.SeriesDeclarer = (*groupsLoop)(nil)
)
