// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

const (
	// maxResponseBytes is a DoS backstop on a single page decode, NOT the primary size bound (a bounded
	// StatsWindow + modest PageLimit keep real pages far smaller — see the PoC spec). The decode struct
	// omits the heavy `values` maps, so decode allocations stay bounded even for a large wire response.
	maxResponseBytes = 32 << 20
	// httpTimeout is generous: exact aggregate stats over a wide window can be slow on the backend.
	httpTimeout = 30 * time.Second
)

// Config is the PACKAGE-LOCAL, decoupled config for the LangSmith source. The composition root maps a
// (generic + tagged langsmith-specific) root-config block into this; nothing here leaks into
// internal/config or the Helm generator. Metric names, label keys, endpoints, cadence and window are
// all data — no vendor/customer constant is baked into core.
type Config struct {
	// Generic (mapped from config.SourceConfig / its "sessions" LoopConfig by New).
	BaseURL        string
	AuthHeader     string
	AuthValue      string
	SourceInstance string
	Prefix         string
	Cadence        time.Duration
	RPS            float64
	Burst          int
	AllowHosts     []string
	AllowPrivate   bool
	UserAgent      string

	// LangSmith-specific (defaults applied below; coordinator exposes these via root config — WIRING TODO).
	StatsWindow       time.Duration // stats_start_time = now - StatsWindow (rolling aggregate window)
	UseApproxStats    bool          // approximate (cheaper) backend stats
	SessionFilter     string        // LangSmith `filter` expression — bounds which sessions (cardinality)
	SessionLabelKey   string        // label key for the per-session dimension (default "session")
	SessionLabelValue string        // "id" (default, bounded) | "name" (human, but ephemeral/high-card)
	PageLimit         int           // offset/limit page size (default 100)
	MaxSessions       int           // hard cap on sessions pulled per Collect (bounded coverage; default 1000)
	EmitFeedback      bool          // emit numeric feedback_stats facets
	FeedbackKeys      []string      // optional allow-list of feedback keys (empty ⇒ all numeric)
}

type sessionsSource struct{ loops []source.Loop }

func (s *sessionsSource) ID() string           { return "langsmith" }
func (s *sessionsSource) Loops() []source.Loop { return s.loops }

// Register adds the langsmith constructor to a registry (called by the composition root).
func Register(reg *source.Registry) { reg.Register("langsmith", New) }

// New is the registry constructor: it builds whichever loops are enabled (`sessions`, `runs`), SHARING
// one httpx client (one rate limiter) across them so both stay within LangSmith's tenant-wide ~10 req/10s
// budget. The generic config.SourceConfig (+ each loop's LoopConfig.Settings) maps into the package-local
// configs; LangSmith-specific knobs are defaulted here (WIRING TODO: docs/superpowers/specs/langsmith-poc.md).
func New(sc config.SourceConfig, deps source.Deps) (source.Source, error) {
	sessCfg, hasS := sc.Loops["sessions"]
	runsCfg, hasR := sc.Loops["runs"]
	enS := hasS && sessCfg.Enabled
	enR := hasR && runsCfg.Enabled
	if !enS && !enR {
		return &sessionsSource{}, nil // nothing enabled
	}
	hc := newClient(sc, deps)
	var loops []source.Loop
	if enS {
		sd := defaultSessionsSettings()
		cfg := Config{
			BaseURL: sc.BaseURL, AuthHeader: sc.Auth.Header, AuthValue: sc.Auth.Value,
			SourceInstance: sc.SourceInstance, Prefix: sessCfg.MetricPrefix,
			Cadence: time.Duration(sessCfg.Cadence),
			RPS:     sc.RateLimit.RPS, Burst: sc.RateLimit.Burst,
			AllowHosts: sc.HTTP.AllowHosts, AllowPrivate: sc.HTTP.AllowPrivate, UserAgent: sc.HTTP.UserAgent,
			// LangSmith-specific defaults derived from defaultSessionsSettings() — single source of truth
			// shared with ExampleSource(). The decoupled per-loop `settings` map then overlays any
			// operator-set knobs (a malformed value fails fast).
			StatsWindow: sd.StatsWindow, UseApproxStats: sd.UseApproxStats,
			SessionLabelKey: "session", SessionLabelValue: sd.SessionLabelValue,
			PageLimit: sd.PageLimit, MaxSessions: sd.MaxSessions, EmitFeedback: sd.EmitFeedback,
		}
		if err := applySettings(&cfg, sessCfg.Settings); err != nil {
			return nil, err
		}
		lp, err := newSessionsLoop(cfg, hc)
		if err != nil {
			return nil, err
		}
		lp.onAuthError = deps.OnAuthError // followup §9: 401/403 → own alertable signal
		loops = append(loops, lp)
	}
	if enR {
		lp, err := newRunsLoop(sc, runsCfg, deps, hc)
		if err != nil {
			return nil, err
		}
		loops = append(loops, lp)
	}
	return &sessionsSource{loops: loops}, nil
}

// newClient builds the hardened outbound client SHARED by all langsmith loops (one rate limiter — the
// LangSmith ~10 req/10s budget is tenant-wide). Mirrors portkey.newClient.
func newClient(sc config.SourceConfig, deps source.Deps) *httpx.Client {
	ua := sc.HTTP.UserAgent
	if ua == "" {
		ua = "genai-otel-bridge/0.1"
	}
	return httpx.New(httpx.Config{
		UserAgent: ua, Timeout: httpTimeout,
		AllowHosts: sc.HTTP.AllowHosts, AllowPrivate: sc.HTTP.AllowPrivate,
		Limiter:  rate.NewLimiter(rate.Limit(sc.RateLimit.RPS), sc.RateLimit.Burst),
		Observer: deps.UpstreamObserver,
	})
}

// newSource builds a sessions-only source from the package-local Config (the path tests use directly).
// It builds its OWN client; New shares one client across loops via newClient + newSessionsLoop.
func newSource(cfg Config, deps source.Deps) (source.Source, error) {
	ua := cfg.UserAgent
	if ua == "" {
		ua = "genai-otel-bridge/0.1"
	}
	hc := httpx.New(httpx.Config{
		UserAgent: ua, Timeout: httpTimeout,
		AllowHosts: cfg.AllowHosts, AllowPrivate: cfg.AllowPrivate,
		Limiter:  rate.NewLimiter(rate.Limit(cfg.RPS), cfg.Burst),
		Observer: deps.UpstreamObserver,
	})
	lp, err := newSessionsLoop(cfg, hc)
	if err != nil {
		return nil, err
	}
	lp.onAuthError = deps.OnAuthError // followup §9: 401/403 → own alertable signal
	return &sessionsSource{loops: []source.Loop{lp}}, nil
}

// newSessionsLoop builds the sessions loop from the package-local Config + a (possibly shared) client.
// Applies safe defaults for any unset field so a directly-constructed Config can't divide-by-zero or
// page unbounded.
func newSessionsLoop(cfg Config, hc *httpx.Client) (*sessionsLoop, error) {
	if cfg.AuthHeader == "" || cfg.AuthValue == "" {
		return nil, fmt.Errorf("langsmith: auth header and value are required")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("langsmith: base_url is required")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "langsmith"
	}
	if cfg.SessionLabelKey == "" {
		cfg.SessionLabelKey = "session"
	}
	if cfg.StatsWindow <= 0 {
		cfg.StatsWindow = time.Hour
	}
	if cfg.PageLimit <= 0 {
		cfg.PageLimit = 100
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 1000
	}
	if cfg.Cadence <= 0 {
		cfg.Cadence = time.Minute
	}
	var allow map[string]bool
	if len(cfg.FeedbackKeys) > 0 {
		allow = make(map[string]bool, len(cfg.FeedbackKeys))
		for _, k := range cfg.FeedbackKeys {
			allow[k] = true
		}
	}
	return &sessionsLoop{
		baseURL: cfg.BaseURL, authHdr: cfg.AuthHeader, authVal: cfg.AuthValue,
		hc: hc, prefix: cfg.Prefix, sourceInstance: cfg.SourceInstance,
		cadence: cfg.Cadence, statsWindow: cfg.StatsWindow, useApproxStats: cfg.UseApproxStats,
		sessionFilter: cfg.SessionFilter, pageLimit: cfg.PageLimit, maxSessions: cfg.MaxSessions,
		deriveCfg: deriveConfig{
			prefix:          cfg.Prefix,
			sessionLabelKey: cfg.SessionLabelKey,
			useName:         cfg.SessionLabelValue == "name",
			emitFeedback:    cfg.EmitFeedback,
			feedbackAllow:   allow,
		},
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

type sessionsLoop struct {
	baseURL, authHdr, authVal string
	hc                        *httpx.Client
	prefix, sourceInstance    string
	cadence, statsWindow      time.Duration
	useApproxStats            bool
	sessionFilter             string
	pageLimit, maxSessions    int
	deriveCfg                 deriveConfig
	onAuthError               func(loop, source string)
	now                       func() time.Time
}

func (l *sessionsLoop) Cadence() time.Duration { return l.cadence }

// SeriesNames declares the fixed facet series this loop emits (quantile/feedback_key/session are
// LABELS, not separate series) — for startup ownership validation (F22/F42).
func (l *sessionsLoop) SeriesNames() []string {
	suffixes := []string{
		"runs", "latency_seconds", "first_token_seconds", "tokens", "prompt_tokens",
		"completion_tokens", "cost_usd", "prompt_cost_usd", "completion_cost_usd",
		"error_rate", "streaming_rate", "feedback_score", "feedback_count",
	}
	out := make([]string, 0, len(suffixes))
	for _, s := range suffixes {
		out = append(out, l.prefix+"_"+s)
	}
	return out
}

func (l *sessionsLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{
		SourceInstance:    l.sourceInstance,
		Loop:              "sessions",
		OutputFingerprint: model.Fingerprint(l.SeriesNames(), "prefix="+l.prefix),
	}
}

// Collect pulls the current per-session aggregate snapshot over [now-StatsWindow, now] and derives
// gauges stamped at `now`. AGGREGATE-NOW (rolling snapshot), not a gap-free historical backfill:
// `since.Time` is intentionally unused for the window; the watermark advances to `now` as a
// forward-only liveness cursor (Cursor/Epoch carried through).
func (l *sessionsLoop) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	now := l.now()
	statsStart := now.Add(-l.statsWindow)

	var sessions []tracerSession
	seen := map[string]bool{}
	capped := false
	for offset := 0; ; offset += l.pageLimit {
		page, code, err := l.fetchPage(ctx, statsStart, offset)
		if err != nil {
			return model.Batch{}, fmt.Errorf("langsmith: fetch sessions (offset %d): %w", offset, err)
		}
		switch code {
		case http.StatusOK:
			// proceed
		case http.StatusTooManyRequests:
			return model.Batch{}, source.ErrQuotaExceeded // discard batch, no advance, back off (F3/F34)
		default:
			// 401/403/5xx/other: retryable error, no advance — loud (window_lag grows), never a silent
			// empty-data advance (CP-R3). A 404 on the sole endpoint is a real config/permission error.
			if source.IsAuthStatus(code) && l.onAuthError != nil {
				l.onAuthError("sessions", l.sourceInstance) // followup §9: credential failure = own signal
			}
			return model.Batch{}, fmt.Errorf("langsmith: sessions status %d", code)
		}
		added := 0
		for _, s := range page {
			if seen[s.ID] { // dedup across offset pages (live, mutating, start_time-desc list)
				continue
			}
			seen[s.ID] = true
			sessions = append(sessions, s)
			added++
			if len(sessions) >= l.maxSessions {
				capped = true
				break
			}
		}
		// Terminate on: cap reached, a short (final) page, OR no progress — the last guards against a
		// misbehaving server that ignores `offset` and would otherwise loop forever (never silently hang).
		if capped || len(page) < l.pageLimit || added == 0 {
			break
		}
	}
	if capped {
		// Never silent: a bounded-coverage truncation is logged so a too-small MaxSessions vs a growing
		// session population is observable (operationally honest).
		slog.Warn("langsmith sessions truncated at MaxSessions cap",
			"source", l.sourceInstance, "max_sessions", l.maxSessions)
	}

	samples := derive(sessions, l.deriveCfg, now)
	wm := since
	wm.Time = now
	return model.Batch{Key: l.Key(), Samples: samples, Watermark: wm}, nil
}

// fetchPage GETs one offset/limit page of /sessions with include_stats. Returns the decoded sessions,
// the HTTP status (the caller maps the taxonomy), and a transport error if any.
func (l *sessionsLoop) fetchPage(ctx context.Context, statsStart time.Time, offset int) ([]tracerSession, int, error) {
	u, err := url.Parse(l.baseURL + "/sessions")
	if err != nil {
		return nil, 0, err
	}
	q := url.Values{}
	q.Set("include_stats", "true")
	if l.useApproxStats {
		q.Set("use_approx_stats", "true")
	}
	q.Set("stats_start_time", statsStart.UTC().Format(time.RFC3339))
	q.Set("sort_by", "start_time")
	q.Set("sort_by_desc", "true")
	q.Set("limit", strconv.Itoa(l.pageLimit))
	q.Set("offset", strconv.Itoa(offset))
	if l.sessionFilter != "" {
		q.Set("filter", l.sessionFilter)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set(l.authHdr, l.authVal)
	resp, err := l.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return nil, resp.StatusCode, nil
	}
	var page []tracerSession
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&page); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return page, resp.StatusCode, nil
}

var _ source.Loop = (*sessionsLoop)(nil)
var _ source.SeriesDeclarer = (*sessionsLoop)(nil)
