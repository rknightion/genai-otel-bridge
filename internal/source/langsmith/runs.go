// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/grafana-ps/aip-oi/internal/httpx"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// runsCursor is the in-flight position WITHIN one window. Watermark.Time is the last FULLY-drained
// window's winMax (forward-only); this cursor carries the not-yet-complete window + the API resume
// token. An empty/corrupt cursor = idle (start a fresh window) — a safe restart (re-pull is free;
// at-least-once tolerates a re-emit).
type runsCursor struct {
	WinMin string `json:"win_min,omitempty"` // RFC3339Nano — the window being drained
	WinMax string `json:"win_max,omitempty"`
	Next   string `json:"next,omitempty"` // API cursors.next to resume mid-window; "" = first page
	Page   int    `json:"page,omitempty"` // pages drained so far (max_pages_per_window cap)
}

func decodeRunsCursor(s string) runsCursor {
	if s == "" {
		return runsCursor{}
	}
	var c runsCursor
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		// [adversarial-review L1] corrupt-but-present cursor → restart the window from the watermark (safe:
		// re-pull is free, at-least-once). Log it so a checkpoint-corrupting bug isn't invisible.
		slog.Warn("langsmith runs: corrupt cursor — restarting the window from the watermark", "err", err)
		return runsCursor{}
	}
	return c
}

func (c runsCursor) encode() string {
	b, _ := json.Marshal(c)
	return string(b)
}

func (c runsCursor) windowBounds() (winMin, winMax time.Time, ok bool) {
	a, ok1 := parseCursorTime(c.WinMin)
	b, ok2 := parseCursorTime(c.WinMax)
	return a, b, ok1 && ok2
}

func parseCursorTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// runsLoop is the forward-only windowed log pull (the LangSmith analogue of Portkey logs_export, minus
// the export-job lifecycle: runs/query is a synchronous paginated POST). One cursor page per Collect;
// window/cursor state rides Watermark.Cursor; Watermark.Time advances only on whole-window completion.
// DELIVERY IS AT-LEAST-ONCE (Loki tolerates dup operational records). The session list is resolved per
// Collect (static settings.session_ids, or cached filter-bounded auto-discovery via GET /sessions).
type runsLoop struct {
	baseURL, authHdr, authVal string
	hc                        *httpx.Client
	sourceInstance            string

	// scope
	sessionIDs    []string // explicit static scope; if set, wins over discovery
	sessionFilter string   // LangSmith filter expr for auto-discovery (used when sessionIDs is empty)
	maxSessions   int      // cap on discovered/used projects (loud truncation)
	selectFields  []string // content-free projection sent to runs/query (NOT relied on — strip is authoritative)

	// window / paging
	cadence           time.Duration
	window, settle    time.Duration
	maxBackfill       time.Duration
	pageSize          int
	maxPagesPerWindow int
	maxResponseBytes  int64
	sessionRefresh    time.Duration // in-memory discovery cache TTL

	// volume / filter knobs
	rootOnly bool
	runType  string

	policy         runsFieldPolicy
	onGraphSkipped func(loop, graph string)
	onAuthError    func(loop, source string)
	now            func() time.Time

	// in-memory discovery cache (Collect is single-flight → no lock; resets on failover → re-discover).
	cachedSessions []string
	cachedAt       time.Time
}

func (l *runsLoop) Cadence() time.Duration { return l.cadence }

// IndexedKeys returns the full set of record fields this loop promotes to LogRecord.IndexedAttributes
// (OTLP resource attrs → Loki stream labels via GS1): the base content-free allow-list ∪
// settings.extra_indexed_fields (already merged into l.policy.indexed at construction). Sorted, for a
// deterministic Loki-budget error message and Key fingerprint. Satisfies source.IndexedKeyDeclarer — the
// composition root budgets these against the Loki max_label_names_per_series limit.
func (l *runsLoop) IndexedKeys() []string {
	idx := make([]string, 0, len(l.policy.indexed))
	for k := range l.policy.indexed {
		idx = append(idx, k)
	}
	sort.Strings(idx)
	return idx
}

// Key fingerprints the emitted-log SCHEMA (the indexed-attr key set) so a change to what we promote
// bootstraps a fresh watermark (F37). Logs declare no metric series → no SeriesDeclarer.
func (l *runsLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{
		SourceInstance:    l.sourceInstance,
		Loop:              "runs",
		OutputFingerprint: model.Fingerprint(l.IndexedKeys(), "runs"),
	}
}

// windowTruncated records an oversize-window tail-drop as a loud, counted, alertable event.
func (l *runsLoop) windowTruncated() {
	if l.onGraphSkipped != nil {
		l.onGraphSkipped("runs", "window_truncated")
	}
}

// sessionsTruncated records a session-discovery max_sessions cap truncation as a loud, counted,
// alertable event ([adversarial-review M1] — counted, not merely logged, so a project population
// growing past the cap is observable, not a silent drop).
func (l *runsLoop) sessionsTruncated() {
	if l.onGraphSkipped != nil {
		l.onGraphSkipped("runs", "sessions_truncated")
	}
}

// Collect drains ONE cursor page of the current window. Idle (empty/corrupt cursor) starts a new window
// [winMin,winMax] and fetches page 0; a draining cursor resumes via the API token. Watermark.Time
// advances to winMax ONLY when cursors.next is exhausted (or the page cap truncates the window,
// loud+counted). AT-LEAST-ONCE: a mid-window leader change resumes at cur.Next (no re-emit of completed
// pages); an emit-then-checkpoint failure may re-emit a page (Loki tolerates dups). The session scope is
// resolved at the top (static settings.session_ids, or cached filter-bounded discovery).
func (l *runsLoop) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	now := l.now()
	cur := decodeRunsCursor(since.Cursor)
	var winMin, winMax time.Time
	var resume string
	var page int
	draining := false
	if cur.WinMax != "" {
		if a, b, ok := cur.windowBounds(); ok {
			winMin, winMax, resume, page, draining = a, b, cur.Next, cur.Page, true
		}
		// else: corrupt window edges → fall through to idle (start a fresh window from since.Time)
	}
	if !draining {
		winMin = since.Time
		if winMin.IsZero() {
			winMin = now.Add(-l.window) // bootstrap: the most recent one window back
		}
		if floor := now.Add(-l.maxBackfill); winMin.Before(floor) {
			slog.Warn("langsmith runs: watermark older than max_backfill — skipping the unstorable span",
				"from", winMin.UTC().Format(time.RFC3339), "floor", floor.UTC().Format(time.RFC3339), "source", l.sourceInstance)
			winMin = floor
		}
		winMax = winMin.Add(l.window)
		if cutoff := now.Add(-l.settle); winMax.After(cutoff) {
			winMax = cutoff
		}
		if !winMax.After(winMin) {
			return model.Batch{Key: l.Key(), Watermark: since}, nil // nothing settled yet — empty, no advance (no query/discovery)
		}
	}

	// Resolve the project scope only once we're about to query (avoids a wasted discovery on a
	// nothing-settled tick). `draining` freezes the scope for the window's pages — see resolveSessions (H1).
	sessions, err := l.resolveSessions(ctx, now, draining)
	if err != nil {
		return model.Batch{}, err
	}
	runs, next, status, errBody, err := l.queryRuns(ctx, sessions, winMin, winMax, resume)
	if err != nil {
		return model.Batch{}, fmt.Errorf("langsmith runs: query: %w", err)
	}
	switch status {
	case http.StatusOK:
		// proceed
	case http.StatusTooManyRequests:
		return model.Batch{}, source.ErrQuotaExceeded // discard, no advance, back off (F34)
	default:
		// 401/403/5xx/other: retryable error, no advance — loud (window_lag grows), never a silent advance.
		if source.IsAuthStatus(status) && l.onAuthError != nil {
			l.onAuthError("runs", l.sourceInstance) // followup §9: credential failure = own signal
		}
		return model.Batch{}, fmt.Errorf("langsmith runs: status %d%s", status, errBodySuffix(errBody))
	}

	logs := make([]model.LogRecord, 0, len(runs))
	for _, raw := range runs {
		logs = append(logs, l.policy.strip(raw, winMax)) // fallback ts = winMax (start_time present in practice)
	}
	page++

	// More pages remain AND under the cap → keep draining (Time unchanged; the cursor persists via the
	// runner's Cursor!="" commit arm even at the first window's Time==zero).
	if next != "" && page < l.maxPagesPerWindow {
		nc := runsCursor{
			WinMin: winMin.UTC().Format(time.RFC3339Nano), WinMax: winMax.UTC().Format(time.RFC3339Nano),
			Next: next, Page: page,
		}
		return model.Batch{Key: l.Key(), Logs: logs, Watermark: model.Watermark{Time: since.Time, Cursor: nc.encode()}}, nil
	}
	if next != "" {
		// Cap hit with more pages pending → emit what we drained, advance PAST with a counted gap (loud).
		// Never stall (every re-pull would re-hit the cap) and never silent — the operator shrinks `window`
		// or scopes `session_ids` to avoid truncation.
		slog.Error("langsmith runs: window exceeded max_pages_per_window — advancing past with a counted gap (shrink window / tighten session scope)",
			"win_min", winMin.UTC().Format(time.RFC3339), "win_max", winMax.UTC().Format(time.RFC3339),
			"max_pages", l.maxPagesPerWindow, "source", l.sourceInstance)
		l.windowTruncated()
	}
	// Window complete (cursor exhausted or truncated): advance the frontier, clear the cursor.
	return model.Batch{Key: l.Key(), Logs: logs, Watermark: model.Watermark{Time: winMax}}, nil
}

var _ source.Loop = (*runsLoop)(nil)
