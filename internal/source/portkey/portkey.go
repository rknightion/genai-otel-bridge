// SPDX-License-Identifier: Apache-2.0

// Package portkey is the LLM-gateway source. Three loops behind the common Source interface, each
// independently enabled via config: the time-bucketed `analytics` loop derives workspace-aggregate
// gauges from the validated graph endpoints (DESIGN §15); the window-total `groups` loop (groups.go)
// emits per-dimension (ai_model / request-metadata) snapshot gauges; and the stateful `logs_export`
// loop (logs_export.go) drives the Portkey export job lifecycle (create→start→poll→download→page) to
// emit content-free operational log records as OTLP logs. All share one httpx client (one rate
// limiter) for control-plane calls; logs_export adds a second client for the signed-URL S3 download.
package portkey

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

const granularity = time.Minute // the 1-min regime we pin via the ≤55m window clamp (H5)

const maxResponseBytes = 1 << 20 // [AR-L-body] cap the analytics decode (~55 points; cheap guard)

type portkeySource struct{ loops []source.Loop }

func (s *portkeySource) ID() string           { return "portkey" }
func (s *portkeySource) Loops() []source.Loop { return s.loops }

// Register adds the portkey constructor to a registry (called by the composition root).
func Register(reg *source.Registry) { reg.Register("portkey", New) }

// New builds the portkey source from config + composition-root deps. It constructs whichever loops are
// enabled (`analytics`, `groups`), sharing ONE httpx client across them so both stay within Portkey's
// tenant-wide request budget (one rate limiter). Analytics is appended first so callers indexing
// Loops()[0] for the analytics loop keep working.
//
// With a scope (cfg.Scope, set only by the plan package for a tenant-shape target) New returns a
// *scopedSource, which also implements source.ScopeAsserter, and every analytics and groups request
// carries the scope name as workspace_slug. It then refuses settings.expected_workspace on any loop (the
// scope replaces it; this is the only place that rule is enforced, since core config never names a vendor
// setting), an analytics max_backfill or a groups window_span longer than scopeMaxLookback, and a scope
// with no name. With no scope it returns a *portkeySource and every request is the one it always sent.
func New(cfg config.SourceConfig, deps source.Deps) (source.Source, error) {
	scoped := cfg.Scope != (config.ScopeConfig{})
	if scoped {
		if err := refuseScopeConfig(cfg); err != nil {
			return nil, err
		}
	}
	src, hc, err := newLoops(cfg, deps)
	if err != nil {
		return nil, err
	}
	if !scoped {
		return src, nil
	}
	if err := refuseScopedLookback(src); err != nil {
		return nil, err
	}
	if hc == nil { // no loop is enabled; the assertion still needs the client
		hc = newClient(cfg, deps)
	}
	return &scopedSource{
		portkeySource: src, hc: hc,
		baseURL: cfg.BaseURL, authHdr: cfg.Auth.Header, authVal: cfg.Auth.Value,
		name: cfg.Scope.Name, instance: cfg.SourceInstance,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// refuseScopeConfig is New's check of a scoped config before any loop is built: a scope needs a name (it
// is the slug sent and compared), and no loop may set the legacy settings.expected_workspace, enabled or
// not and whatever its value, because the scope replaces it (design §2, §5.1).
func refuseScopeConfig(cfg config.SourceConfig) error {
	if cfg.Scope.Name == "" {
		return fmt.Errorf("portkey: source %q has a scope with no name; the scope name is the workspace slug it reads", cfg.SourceInstance)
	}
	names := make([]string, 0, len(cfg.Loops))
	for name := range cfg.Loops {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := cfg.Loops[name].Settings["expected_workspace"]; ok {
			return fmt.Errorf("portkey: source %q has a scope, so loop %q may not set settings.expected_workspace (the scope replaces it)", cfg.SourceInstance, name)
		}
	}
	return nil
}

// refuseScopedLookback is New's check of a scoped source's built loops: none may read further back than
// scopeMaxLookback, a day inside the window the scope assertion reads, so a loop never collects traffic
// older than the traffic the assertion has seen.
func refuseScopedLookback(src *portkeySource) error {
	for _, lp := range src.loops {
		switch l := lp.(type) {
		case *analyticsLoop:
			if l.maxBackfill > scopeMaxLookback {
				return fmt.Errorf("portkey analytics: max_backfill (%s) is longer than %s, the longest lookback allowed with a scope (the scope assertion reads %s)", l.maxBackfill, scopeMaxLookback, scopeProbeWindow)
			}
		case *groupsLoop:
			if l.windowSpan > scopeMaxLookback {
				return fmt.Errorf("portkey groups: window_span (%s) is longer than %s, the longest lookback allowed with a scope (the scope assertion reads %s)", l.windowSpan, scopeMaxLookback, scopeProbeWindow)
			}
		}
	}
	return nil
}

// newLoops builds the enabled loops. The client is nil when no loop is enabled.
func newLoops(cfg config.SourceConfig, deps source.Deps) (*portkeySource, *httpx.Client, error) {
	analyticsCfg, hasA := cfg.Loops["analytics"]
	groupsConf, hasG := cfg.Loops["groups"]
	logsConf, hasL := cfg.Loops["logs_export"]
	enA := hasA && analyticsCfg.Enabled
	enG := hasG && groupsConf.Enabled
	enL := hasL && logsConf.Enabled
	if !enA && !enG && !enL {
		return &portkeySource{}, nil, nil // nothing enabled
	}
	// Resolve use-cases once; all loops share the same resolved set.
	ucs, err := resolveUseCases(cfg)
	if err != nil {
		return nil, nil, err
	}
	hc := newClient(cfg, deps)
	var loops []source.Loop
	if enA {
		lp, err := newAnalyticsLoop(cfg, analyticsCfg, deps, hc, ucs)
		if err != nil {
			return nil, nil, err
		}
		loops = append(loops, lp)
	}
	if enG {
		lp, err := newGroupsLoop(cfg, groupsConf, deps, hc, ucs)
		if err != nil {
			return nil, nil, err
		}
		loops = append(loops, lp)
	}
	if enL {
		// Fan-out: one logsExportLoop instance per use-case (ownership-safe — the loop declares an
		// EMPTY series set, so distinct cursors carry no duplicate-series risk). The legacy unlabelled
		// path (no api_key_use_cases configured) runs a single instance with an empty resolvedUseCase.
		logsPasses := ucs
		if len(logsPasses) == 0 {
			logsPasses = []resolvedUseCase{{}}
		}
		for _, uc := range logsPasses {
			lp, err := newLogsExportLoop(cfg, logsConf, deps, hc, uc)
			if err != nil {
				return nil, nil, err
			}
			loops = append(loops, lp)
		}
	}
	return &portkeySource{loops: loops}, hc, nil
}

// newClient builds the hardened outbound client shared by all portkey loops (one rate limiter).
func newClient(cfg config.SourceConfig, deps source.Deps) *httpx.Client {
	ua := cfg.HTTP.UserAgent
	if ua == "" {
		ua = "genai-otel-bridge/0.1"
	}
	return httpx.New(httpx.Config{
		UserAgent: ua, Timeout: 10 * time.Second,
		AllowHosts: cfg.HTTP.AllowHosts, AllowPrivate: cfg.HTTP.AllowPrivate,
		Limiter:  rate.NewLimiter(rate.Limit(cfg.RateLimit.RPS), cfg.RateLimit.Burst),
		Observer: deps.UpstreamObserver, // self-obs upstream-request histogram (nil ⇒ off)
	})
}

func newAnalyticsLoop(cfg config.SourceConfig, lpCfg config.LoopConfig, deps source.Deps, hc *httpx.Client, ucs []resolvedUseCase) (*analyticsLoop, error) {
	prefix := lpCfg.MetricPrefix
	if prefix == "" {
		prefix = "portkey_api"
	}
	graphs := append([]string(nil), lpCfg.Graphs...)
	sort.Strings(graphs)
	if len(graphs) == 0 {
		return nil, fmt.Errorf("portkey: analytics loop enabled but no graphs configured")
	}
	for _, g := range graphs { // [CP-H4] unknown graph names are part of the output schema — fail fast on a typo
		if _, ok := metricSuffix[g]; !ok {
			return nil, fmt.Errorf("portkey: unknown graph %q (supported: cost,errors,latency,requests,tokens,users)", g)
		}
	}
	// [AR-HIGH] A time-bucketed loop REQUIRES a positive window: with window==0, Collect computes
	// until=min(start, now-settle) ≤ start every tick → empty batch, watermark never advances → a
	// SILENT permanent no-op. The config validator deliberately skips the bucket-math checks for a
	// window-less (snapshot) loop, so fail fast HERE rather than let an omitted window stall silently.
	window := time.Duration(lpCfg.Window)
	if window <= 0 {
		return nil, fmt.Errorf("portkey: analytics loop requires a positive window (time-bucketed source); got %s", window)
	}
	settle := time.Duration(lpCfg.BucketSettle)
	// [F27] Collect floors the window end to the granularity, so a window shorter than one bucket would
	// truncate back onto the watermark on every poll and never progress: reject it here, naming the key.
	if window < granularity {
		return nil, fmt.Errorf("portkey: analytics loop window (%s) must be at least the %s bucket granularity (windows are minute-aligned; a shorter window can never advance)", window, granularity)
	}

	// [#57] Mirror the AR-HIGH window guard for the OTHER two silent-no-op triggers on a raw-YAML config
	// (config.Load applies no defaults for these; the chart renders 50m/90m). Collect computes
	// start=now-bootstrap clamped to the now-max_backfill floor, and until=now-settle:
	//   - max_backfill omitted/0 (or ≤ settle) ⇒ floor=now-max_backfill ≥ until ⇒ start ≥ until every tick;
	//   - bootstrap ≤ settle ⇒ until ≤ start=now-bootstrap every tick.
	// Either way until ≤ start ⇒ empty batch, watermark stays zero, window_lag is skipped (zero watermark),
	// no samples/errors — a permanent FULLY SILENT data gap. Require both > settle so the loop can advance.
	//
	// [F27] Because until is floored to the granularity, > settle is not enough. When start is clamped to
	// now-X (X = bootstrap_lookback on a zero watermark, max_backfill on a stale one), a poll advances only
	// if floor(now-settle) > now-X. floor(t) > t-granularity always holds and is tight, so the loop is
	// guaranteed to progress at every poll iff X >= settle + granularity; for any smaller X there are
	// instants where floor(now-settle) <= now-X and the window is empty (e.g. X = settle+1s advances only
	// in the first second of each minute). Require both to be at least settle + granularity.
	minSpan := settle + granularity
	if bootstrap := time.Duration(lpCfg.BootstrapLookback); bootstrap < minSpan {
		return nil, fmt.Errorf("portkey: analytics loop requires bootstrap_lookback (%s) >= bucket_settle (%s) + %s bucket granularity = %s (windows are minute-aligned; a shorter lookback leaves no forward window at most polls — a silent stall)", bootstrap, settle, granularity, minSpan)
	}
	if mb := time.Duration(lpCfg.MaxBackfill); mb < minSpan {
		return nil, fmt.Errorf("portkey: analytics loop requires max_backfill (%s) >= bucket_settle (%s) + %s bucket granularity = %s (windows are minute-aligned; a shorter backfill floor leaves no forward window at most polls — a silent stall)", mb, settle, granularity, minSpan)
	}
	band := detectionBand(settle)
	// Build passes: if use-cases are configured, one pass per use-case; otherwise a single unlabelled
	// pass using the legacy settings.api_key_ids filter (backward compatible — Key() is unchanged).
	passes := ucs
	if len(passes) == 0 {
		passes = []resolvedUseCase{{slug: "", apiKeyIDsCSV: cleanAPIKeyIDs(lpCfg.Settings["api_key_ids"])}}
	}
	histories := make(map[string]*revisionHistory, len(passes))
	for _, p := range passes {
		histories[p.slug] = newRevisionHistory(band)
	}
	return &analyticsLoop{
		baseURL: cfg.BaseURL, authHdr: cfg.Auth.Header, authVal: cfg.Auth.Value,
		hc: hc, prefix: prefix, graphs: graphs, sourceInstance: cfg.SourceInstance,
		window: window, settle: settle,
		bootstrap: time.Duration(lpCfg.BootstrapLookback), maxBackfill: time.Duration(lpCfg.MaxBackfill),
		cadence:        time.Duration(lpCfg.Cadence),
		startSemantics: true, // OP5e (DESIGN §15): Portkey timestamps are bucket-START
		now:            func() time.Time { return time.Now().UTC() },
		// Settle-exceedance detection (DESIGN §3.3/F6): the detection window looks back over a trailing
		// band past the forward emit lower bound to RE-OBSERVE recently-settled buckets, so a value
		// change after settle is caught. Band = settle + margin (margin = settle ⇒ band = 2×settle):
		// settle covers the chosen late-arrival horizon, the equal margin gives a couple of polls'
		// slack to actually see (and stop re-counting) the change before it ages out. Detection only —
		// never re-emits. nil hook ⇒ history still tracks but no signal is raised (cheap, harmless).
		onBucketRevised: deps.OnBucketRevised,
		onCapability:    deps.OnCapability,
		onAuthError:     deps.OnAuthError,
		// Optional key-scope guardrail (analytics/groups are bound to the key's workspace, not request-
		// targetable — followup §4). Empty ⇒ no check (backward compatible).
		expectedWorkspace: strings.TrimSpace(lpCfg.Settings["expected_workspace"]),
		workspaceSlug:     cfg.Scope.Name,
		// passes / histories / band replace the old single apiKeyIDs+history fields: one pass per
		// use-case (or the single legacy pass when api_key_use_cases is empty). Key() is unchanged so
		// the checkpoint watermark is never reset on migration.
		passes:    passes,
		histories: histories,
		band:      band,
	}, nil
}

// detectionBand is the trailing window over which we re-observe settled buckets to catch a
// post-settle value change. settle + margin (margin == settle), with a small floor so a zero/tiny
// settle still gives a usable band.
func detectionBand(settle time.Duration) time.Duration {
	band := 2 * settle
	if band < time.Minute {
		band = time.Minute
	}
	return band
}

type analyticsLoop struct {
	baseURL, authHdr, authVal string
	hc                        *httpx.Client
	prefix, sourceInstance    string
	graphs                    []string
	window, settle, bootstrap time.Duration
	maxBackfill, cadence      time.Duration
	startSemantics            bool // set per OP5e
	now                       func() time.Time
	// Settle-exceedance detection state (per-loop; Collect is single-flight so no lock needed).
	// [failover] histories are in-memory only: on a leadership change (or restart) they reset, so the
	// first poll(s) after failover re-learn the trailing band and cannot flag a revision that
	// happened across the handoff. Accepted blind spot (the metric is a drift indicator, not a
	// correctness gate).
	onBucketRevised func(loop, instance string, age time.Duration)
	onCapability    func(loop, instance, graph string, state source.CapabilityState)
	onAuthError     func(loop, source string)
	// expectedWorkspace, if set (settings.expected_workspace), asserts the key's analytics scope is exactly
	// that workspace before emitting (a too-broad key would emit cross-workspace aggregates). scopeVerified
	// caches a passing check (Collect is single-flight, no lock). See scope.go.
	expectedWorkspace string
	scopeVerified     bool
	// workspaceSlug is the source's scope name, sent as workspace_slug on every graph request; empty (no
	// scope, the legacy shape) sends none.
	workspaceSlug string
	// passes is the set of filtered passes to run each Collect. Empty use-cases ⇒ a single pass with
	// slug "" and the legacy settings.api_key_ids filter (backward compatible). Each pass stamps its slug.
	passes []resolvedUseCase
	// histories is the per-slug settle-exceedance detection state (one key's late arrivals must not be
	// read as another key's bucket revision). Keyed by pass slug ("" for the legacy unlabelled pass).
	histories map[string]*revisionHistory
	// band is the detection window width (≈2×settle); kept on the struct so tests can override it.
	band time.Duration
}

func (l *analyticsLoop) SeriesNames() []string {
	out := make([]string, 0, len(l.graphs))
	for _, g := range l.graphs {
		out = append(out, nameFor(l.prefix, g))
	}
	return out
}

func (l *analyticsLoop) Cadence() time.Duration { return l.cadence }

func (l *analyticsLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{
		SourceInstance:    l.sourceInstance,
		Loop:              "analytics",
		OutputFingerprint: model.Fingerprint(l.SeriesNames(), "prefix="+l.prefix),
	}
}

// Collect pulls one forward window, runs N internal filtered passes (one per use-case, or one
// unlabelled pass for the legacy single-key path), and returns the accumulated batch + watermark.
func (l *analyticsLoop) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	now := l.now()
	if l.expectedWorkspace != "" && !l.scopeVerified {
		ok, err := verifyScopeForCollect(ctx, l.hc, l.baseURL, l.authHdr, l.authVal, l.expectedWorkspace, l.Key().Loop, l.sourceInstance, now, l.onCapability, l.onAuthError)
		if err != nil {
			return model.Batch{}, err // refuse to emit (mismatch) or retry (transient) — never silently advance
		}
		l.scopeVerified = ok
	}
	start := since.Time
	if start.IsZero() {
		start = now.Add(-l.bootstrap)
	}
	if floor := now.Add(-l.maxBackfill); start.Before(floor) {
		start = floor // older is unstorable; the scheduler counts the abandoned span (F25)
	}
	until := now.Add(-l.settle)
	if until.Sub(start) > l.window {
		until = start.Add(l.window)
	}
	if d := until.Sub(start); d > 55*time.Minute { // defensive H5 clamp
		until = start.Add(55 * time.Minute)
	}
	// [F27] Pin the window end to a whole granularity bucket. A mid-minute end would emit the bucket
	// containing it from a request that stopped part-way through, and the next window (starting at
	// that end) would emit the same (series, bucket end) again.
	until = until.Truncate(granularity)
	if !until.After(start) {
		return model.Batch{Key: l.Key(), Watermark: since}, nil // nothing settled yet
	}

	// [settle-exceedance] Compute the widened fetchStart once (shared across passes; the window math
	// is the same for all passes — only the api_key_ids filter differs). See collectPass.
	fetchStart := start.Truncate(granularity)
	if l.band > 0 {
		fetchStart = start.Add(-l.band)
		if floor := now.Add(-l.maxBackfill); fetchStart.Before(floor) {
			fetchStart = floor
		}
		// [granularity-safety] The widened detection fetch MUST stay inside Portkey's 1-minute-bucket
		// regime: a request window ≤55m yields 1-min buckets; >59m flips to 10-min (DESIGN §15/H5).
		if minFetch := until.Add(-55 * time.Minute); fetchStart.Before(minFetch) {
			fetchStart = minFetch
		}
		fetchStart = fetchStart.Truncate(granularity) // whole first bucket: no false revision from a cut
	}

	var all []model.Sample
	for _, p := range l.passes {
		s, err := l.collectPass(ctx, p, start, until, fetchStart, now)
		if err != nil {
			return model.Batch{}, err
		}
		all = append(all, s...)
	}
	// [CP-C2] Watermark advances to `until` (the whole window confirmed, even if empty — OP5f).
	wm := since
	wm.Time = until
	return model.Batch{Key: l.Key(), Samples: all, Watermark: wm}, nil
}

// collectPass fetches all graphs for one filtered pass (one use-case), derives forward-only settled
// samples, runs the per-slug settle-exceedance detection, and stamps the use-case slug. The window
// math (start/until/fetchStart) is computed once by the caller and shared across passes.
func (l *analyticsLoop) collectPass(ctx context.Context, p resolvedUseCase, start, until, fetchStart, now time.Time) ([]model.Sample, error) {
	resp := map[string]graphResponse{}
	for _, g := range l.graphs {
		r, code, err := l.fetch(ctx, g, fetchStart, until, p.apiKeyIDsCSV)
		if err != nil {
			return nil, fmt.Errorf("portkey: fetch %s: %w", g, err)
		}
		if code == http.StatusNotFound {
			slog.Warn("portkey graph unavailable (capability)", "graph", g, "source", l.sourceInstance)
			if l.onCapability != nil {
				// round3-#4: make the (otherwise silent) per-graph skip observable. Fires per skipped
				// graph — including when ALL graphs 404 (which ALSO errors loudly below).
				l.onCapability(l.Key().Loop, l.sourceInstance, g, source.CapabilityTransient404)
			}
			continue // capability detection (F5): derive from the rest
		}
		if code != http.StatusOK {
			// 401/403/429/5xx: surface as a retryable Collect error (no silent empty-data drop,
			// no false advance). The scheduler counts it and re-pulls next tick (F1/F2). A
			// persistent 401/403 keeps the loop erroring + window_lag rising (loud), never a crash.
			if source.IsAuthStatus(code) && l.onAuthError != nil {
				l.onAuthError(l.Key().Loop, l.sourceInstance) // followup §9: credential failure = own signal
			}
			return nil, source.WrapAuth(code, fmt.Errorf("portkey: %s status %d", g, code))
		}
		if r.IsQuotaExceeded {
			return nil, source.ErrQuotaExceeded // discard whole batch (F34)
		}
		resp[g] = r
	}

	// [CP-R3] All configured graphs 404'd: capability/permission/config error — not empty data.
	if len(resp) == 0 {
		return nil, fmt.Errorf("portkey: all %d configured graphs returned 404 — capability/config error, not empty data", len(l.graphs))
	}

	// [AR-H-lb] Forward-only lower bound is `start` — abandoned span counted by scheduler (F25).
	samples, err := derive(resp, l.prefix, start, until, granularity, l.startSemantics)
	if err != nil {
		return nil, err // ErrGranularityUnexpected or a parse error
	}

	// [settle-exceedance] Detection pass — does NOT affect emit/watermark (DESIGN §3.3/F6).
	if h := l.histories[p.slug]; h != nil {
		if settled, derr := derive(resp, l.prefix, fetchStart, until, granularity, l.startSemantics); derr == nil {
			if l.onBucketRevised != nil {
				for _, bucketEnd := range h.observe(settled, until) {
					// age = how late the revision is (now − bucketEnd); always ≥ settle by construction.
					l.onBucketRevised("analytics", l.sourceInstance, now.Sub(bucketEnd))
				}
			} else {
				h.observe(settled, until) // keep history current even with no hook wired
			}
		}
	}

	stampUseCase(samples, p.slug)
	return samples, nil
}

func (l *analyticsLoop) fetch(ctx context.Context, graph string, start, until time.Time, apiKeyIDs string) (graphResponse, int, error) {
	url := fmt.Sprintf("%s/analytics/graphs/%s?time_of_generation_min=%s&time_of_generation_max=%s",
		l.baseURL, graph, start.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	if apiKeyIDs != "" {
		url += "&api_key_ids=" + neturl.QueryEscape(apiKeyIDs)
	}
	// Appended by hand, after api_key_ids: url.Values.Encode would sort the keys and escape the timestamps'
	// colons, changing every legacy URL. With no scope the URL is exactly the one sent before scopes.
	if l.workspaceSlug != "" {
		url += "&workspace_slug=" + neturl.QueryEscape(l.workspaceSlug)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return graphResponse{}, 0, err
	}
	req.Header.Set(l.authHdr, l.authVal)
	resp, err := l.hc.Do(req)
	if err != nil {
		return graphResponse{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return graphResponse{}, resp.StatusCode, nil
	}
	var gr graphResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&gr); err != nil {
		return graphResponse{}, resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return gr, resp.StatusCode, nil
}

var _ source.Loop = (*analyticsLoop)(nil)
var _ source.SeriesDeclarer = (*analyticsLoop)(nil)
