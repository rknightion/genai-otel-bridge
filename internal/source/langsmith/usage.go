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

	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// usageConfig is the package-local, decoupled config for the `usage` (platform cost-driver) loop.
// Shares the transport/auth/paging shape with the sessions loop; adds the usage-specific knobs.
type usageConfig struct {
	BaseURL        string
	AuthHeader     string
	AuthValue      string
	SourceInstance string
	Prefix         string
	Cadence        time.Duration

	StatsWindow       time.Duration // stats_start_time = now - StatsWindow (rolling aggregate window)
	SessionFilter     string        // LangSmith `filter` — bounds which projects (cardinality + span-call fan-out)
	SessionLabelKey   string        // FIXED "session" (matches the guard allow-list)
	SessionLabelValue string        // "id" (default, bounded) | "name" (human, ephemeral/high-card)
	PageLimit         int           // /sessions offset/limit page size (default 100)
	MaxSessions       int           // hard cap on projects per Collect (bounds span-call fan-out; default 1000)
	EmitSpanCounts    bool          // fetch per-project span (all-runs) counts via runs/stats (+1 call/project)
}

// usageLoop pulls per-project platform cost drivers: traces (billable unit, from /sessions run_count) +
// optional spans (storage driver, from runs/stats) — each tagged with the retention tier. Aggregate-now
// snapshot, mirroring the sessions loop. See usage_derive.go for the emitted metrics.
type usageLoop struct {
	baseURL, authHdr, authVal string
	hc                        *httpx.Client
	prefix, sourceInstance    string
	cadence, statsWindow      time.Duration
	sessionFilter             string
	pageLimit, maxSessions    int
	emitSpanCounts            bool
	deriveCfg                 usageDeriveConfig
	onAuthError               func(loop, source string)
	onGraphSkipped            func(loop, graph string)
	now                       func() time.Time
}

// newUsageLoop builds the usage loop from the package-local config + a (possibly shared) client,
// applying safe defaults so a directly-constructed config can't divide-by-zero or page unbounded.
func newUsageLoop(cfg usageConfig, hc *httpx.Client) (*usageLoop, error) {
	if cfg.AuthHeader == "" || cfg.AuthValue == "" {
		return nil, fmt.Errorf("langsmith usage: auth header and value are required")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("langsmith usage: base_url is required")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "langsmith"
	}
	if cfg.SessionLabelKey == "" {
		cfg.SessionLabelKey = "session"
	}
	if cfg.StatsWindow <= 0 {
		cfg.StatsWindow = 10 * time.Minute
	}
	if cfg.PageLimit <= 0 {
		cfg.PageLimit = 100
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 1000
	}
	if cfg.Cadence <= 0 {
		cfg.Cadence = 10 * time.Minute
	}
	// Span-call fan-out is one runs/stats call PER project; with no filter it fans out across the whole
	// (potentially 100s of) project population every poll. Loud at startup — silent unbounded load is
	// exactly what "operationally honest" forbids (the max_sessions cap is the hard backstop).
	if cfg.EmitSpanCounts && cfg.SessionFilter == "" {
		slog.Warn("langsmith usage: emit_span_counts is on with no session_filter — one runs/stats call per project "+
			"per poll across up to max_sessions projects; set session_filter to bound the fan-out",
			"source", cfg.SourceInstance, "max_sessions", cfg.MaxSessions)
	}
	return &usageLoop{
		baseURL: cfg.BaseURL, authHdr: cfg.AuthHeader, authVal: cfg.AuthValue,
		hc: hc, prefix: cfg.Prefix, sourceInstance: cfg.SourceInstance,
		cadence: cfg.Cadence, statsWindow: cfg.StatsWindow,
		sessionFilter: cfg.SessionFilter, pageLimit: cfg.PageLimit, maxSessions: cfg.MaxSessions,
		emitSpanCounts: cfg.EmitSpanCounts,
		deriveCfg: usageDeriveConfig{
			prefix:          cfg.Prefix,
			sessionLabelKey: cfg.SessionLabelKey,
			useName:         cfg.SessionLabelValue == "name",
		},
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (l *usageLoop) Cadence() time.Duration { return l.cadence }

// SeriesNames declares the fixed series this loop emits (session/retention_tier are LABELS) — for
// startup ownership validation. `usage_spans` is declared even when emit_span_counts is off (the loop
// CAN emit it); ownership is about potential output, not this poll's subset.
func (l *usageLoop) SeriesNames() []string {
	return []string{l.prefix + "_usage_traces", l.prefix + "_usage_spans"}
}

func (l *usageLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{
		SourceInstance:    l.sourceInstance,
		Loop:              "usage",
		OutputFingerprint: model.Fingerprint(l.SeriesNames(), "prefix="+l.prefix),
	}
}

// Collect pulls the current per-project trace counts + retention tier over [now-StatsWindow, now], and
// (when enabled) a per-project span count via runs/stats, then derives gauges stamped at `now`.
// AGGREGATE-NOW snapshot (like the sessions loop): `since.Time` is unused; the watermark advances to
// `now` as a forward-only liveness cursor. A span-call failure for one project is a LOUD counted skip
// (traces still emitted); only a 429 (quota) discards the whole batch to back off.
func (l *usageLoop) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	now := l.now()
	statsStart := now.Add(-l.statsWindow)

	var sessions []usageSession
	seen := map[string]bool{}
	capped := false
	for offset := 0; ; offset += l.pageLimit {
		page, code, err := l.fetchPage(ctx, statsStart, offset)
		if err != nil {
			return model.Batch{}, fmt.Errorf("langsmith usage: fetch sessions (offset %d): %w", offset, err)
		}
		switch code {
		case http.StatusOK:
			// proceed
		case http.StatusTooManyRequests:
			return model.Batch{}, source.ErrQuotaExceeded // discard, no advance, back off (F3/F34)
		default:
			if source.IsAuthStatus(code) && l.onAuthError != nil {
				l.onAuthError("usage", l.sourceInstance)
			}
			return model.Batch{}, fmt.Errorf("langsmith usage: sessions status %d", code)
		}
		added := 0
		for _, s := range page {
			if seen[s.ID] {
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
		if capped || len(page) < l.pageLimit || added == 0 {
			break
		}
	}
	if capped {
		slog.Warn("langsmith usage sessions truncated at MaxSessions cap",
			"source", l.sourceInstance, "max_sessions", l.maxSessions)
	}

	var spans map[string]int
	if l.emitSpanCounts {
		spans = make(map[string]int, len(sessions))
		for _, s := range sessions {
			n, code, err := l.fetchSpanCount(ctx, s.ID, statsStart)
			if code == http.StatusTooManyRequests {
				return model.Batch{}, source.ErrQuotaExceeded // quota → back off the whole batch
			}
			if err != nil || code != http.StatusOK || n == nil {
				// Never silent: a per-project span-call failure is COUNTED + logged and that project's span
				// metric is skipped; traces still flow (the loop always progresses). A 401/403 also fires the
				// credential signal.
				if source.IsAuthStatus(code) && l.onAuthError != nil {
					l.onAuthError("usage", l.sourceInstance)
				}
				if l.onGraphSkipped != nil {
					l.onGraphSkipped("usage", "span_stats")
				}
				slog.Warn("langsmith usage: span count skipped for project",
					"source", l.sourceInstance, "status", code, "err", err)
				continue
			}
			spans[s.ID] = *n
		}
	}

	samples := usageDerive(sessions, spans, l.deriveCfg, now)
	wm := since
	wm.Time = now
	return model.Batch{Key: l.Key(), Samples: samples, Watermark: wm}, nil
}

// fetchPage GETs one offset/limit page of /sessions with include_stats, decoding only the usage fields.
// Mirrors the sessions loop's fetchPage query shape (sort_by=start_time is required — a bare offset 403s
// on the real Cloudflare-fronted instance).
func (l *usageLoop) fetchPage(ctx context.Context, statsStart time.Time, offset int) ([]usageSession, int, error) {
	u, err := url.Parse(l.baseURL + "/sessions")
	if err != nil {
		return nil, 0, err
	}
	q := url.Values{}
	q.Set("include_stats", "true")
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
	var page []usageSession
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&page); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return page, resp.StatusCode, nil
}

var _ source.Loop = (*usageLoop)(nil)
var _ source.SeriesDeclarer = (*usageLoop)(nil)
