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
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// resolveSessions returns the project-UUID scope for runs/query. Static settings.session_ids wins; else
// it auto-discovers via GET /sessions (filter-bounded), cached in-memory with a TTL. Collect is
// single-flight so no lock is needed; the cache resets on failover (new process ⇒ re-discover). A filter
// matching NO projects errors loudly (never silently advance over an empty scope, which would 400).
func (l *runsLoop) resolveSessions(ctx context.Context, now time.Time, draining bool) ([]string, error) {
	if len(l.sessionIDs) > 0 {
		return l.sessionIDs, nil
	}
	// [adversarial-review H1] Freeze the resolved scope for a window's pages: while DRAINING reuse the
	// cache unconditionally (a mid-window TTL refresh would query later pages against a different project
	// set than page 1 — the opaque cursor is bound to page 1's scope — silently dropping/dup'ing runs).
	// Only an idle tick past the TTL re-discovers. On failover the cache is empty, so a resumed window
	// re-discovers (best-effort; the project set is ~stable, and at-least-once tolerates the rare delta).
	if len(l.cachedSessions) > 0 && (draining || now.Sub(l.cachedAt) < l.sessionRefresh) {
		return l.cachedSessions, nil
	}
	ids, err := l.listSessionIDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("langsmith runs: session_filter matched no projects (scope is empty)")
	}
	l.cachedSessions, l.cachedAt = ids, now
	return ids, nil
}

// listSessionIDs pages GET /sessions (filter-bounded) and collects project UUIDs up to maxSessions. Only
// the `id` is decoded — project names/descriptions are never materialised (content-safe by struct
// omission, like the sessions loop). Over-cap is a LOUD truncation (never silent).
func (l *runsLoop) listSessionIDs(ctx context.Context) ([]string, error) {
	const pageLimit = 100
	seen := map[string]bool{}
	var ids []string
	for offset := 0; ; offset += pageLimit {
		u, err := url.Parse(l.baseURL + "/sessions")
		if err != nil {
			return nil, err
		}
		q := url.Values{}
		// Share the sibling loops' /sessions query shape (incl. the REQUIRED sort_by=start_time — a bare
		// offset 403s on the real Cloudflare-fronted instance, live-probed). Centralised so the three call
		// sites can't drift (#54).
		setSessionsPageQuery(q, pageLimit, offset, l.sessionFilter)
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set(l.authHdr, l.authVal)
		resp, err := l.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			// Same shared ~10req/10s tenant budget as queryRuns/sessions/usage — map 429 to the quota
			// sentinel so the scheduler counts samples_skipped{reason=quota_exceeded}, not a generic
			// collect error (#101). The taxonomy must match the sibling /sessions call sites.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			return nil, source.ErrQuotaExceeded
		}
		if resp.StatusCode != http.StatusOK {
			snip := httpx.ErrSnippet(resp)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("langsmith runs: discover sessions status %d%s", resp.StatusCode, errBodySuffix(snip))
		}
		var page []struct {
			ID string `json:"id"`
		}
		derr := json.NewDecoder(io.LimitReader(resp.Body, l.respCap())).Decode(&page)
		_ = resp.Body.Close()
		if derr != nil {
			return nil, fmt.Errorf("langsmith runs: decode sessions: %w", derr)
		}
		added := 0
		for _, s := range page {
			if s.ID == "" || seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			ids = append(ids, s.ID)
			added++
			if len(ids) >= l.maxSessions {
				slog.Warn("langsmith runs: session discovery truncated at max_sessions cap",
					"max_sessions", l.maxSessions, "source", l.sourceInstance)
				l.sessionsTruncated() // [adversarial-review M1] counted + alertable, not just logged
				return ids, nil
			}
		}
		// Terminate on a short/empty page or a no-progress page (offset-ignoring server → never hang).
		if len(page) < pageLimit || added == 0 {
			break
		}
	}
	return ids, nil
}
