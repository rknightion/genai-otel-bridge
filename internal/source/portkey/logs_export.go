// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/grafana-ps/aip-oi/internal/httpx"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// Export job phases (the logs-export lifecycle state machine, PoC §6). One Collect advances the machine
// by at most one non-blocking step; the phase + job state ride in model.Watermark.Cursor (JSON) so the
// FROZEN Loop/Collect seam is unchanged and a leader change resumes the in-flight job idempotently.
const (
	phaseIdle        = "idle"        // between windows — Time = last fully-emitted win_max
	phaseCreated     = "created"     // export job created (draft), not yet started
	phasePolling     = "polling"     // job queued/in_progress — waiting for success
	phaseDownloading = "downloading" // job success — streaming/chunking the signed-URL file
)

// exportCursor is the in-flight export job state for ONE window (one page at a time). Watermark.Time is
// the last FULLY-emitted window's win_max (monotonic, forward-only); this cursor carries the
// not-yet-complete window's progress and only clears back to idle when the whole window is emitted.
type exportCursor struct {
	Phase          string `json:"phase"`
	JobID          string `json:"job_id,omitempty"`
	WinMin         string `json:"win_min,omitempty"`          // RFC3339 — the window THIS job set covers
	WinMax         string `json:"win_max,omitempty"`          // RFC3339
	Page           int    `json:"page,omitempty"`             // current_page being processed (0-indexed)
	Pages          int    `json:"pages,omitempty"`            // ceil(total_records/page_size), learned at create
	TotalRecords   int    `json:"total_records,omitempty"`    // matched count, fixed at create
	PageOffsetDone int    `json:"page_offset_done,omitempty"` // LINES of the current page already consumed (chunk resume)
	PollDeadline   string `json:"poll_deadline,omitempty"`    // RFC3339Nano — abandon (cancel+restart) a job still running past this
}

// decodeCursor parses the Watermark.Cursor. An empty cursor (first run / after a clean window) OR an
// unparseable one resets to idle — re-pulling the window from scratch is free and idempotent (the S3
// object is stable; we never re-run a successful export, only re-download), so a corrupt cursor is a
// safe restart, not a crash.
func decodeCursor(s string) exportCursor {
	if s == "" {
		return exportCursor{Phase: phaseIdle}
	}
	var c exportCursor
	if err := json.Unmarshal([]byte(s), &c); err != nil || c.Phase == "" {
		return exportCursor{Phase: phaseIdle}
	}
	return c
}

func (c exportCursor) encode() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// logsExportLoop is the STATEFUL Portkey logs-export loop (RP2 / Cdx-C6). Unlike the analytics
// (time-bucketed) and groups (snapshot) loops, a single Portkey "export" is a multi-step job lifecycle
// (create → start → poll → download → page), so this loop encodes a state machine in Watermark.Cursor
// and advances it by at MOST one non-blocking step per Collect — keeping the FROZEN Loop/Collect seam.
//
// Watermark.Time = the last FULLY-emitted window's win_max (monotonic, forward-only, gap-free as for
// metrics). The Cursor carries the in-flight window's job state. DELIVERY IS AT-LEAST-ONCE for logs (not
// the exactly-once gap-free guarantee of the metric plane): an in-flight page resumes via the cursor
// (re-download the stable S3 object, skip to page_offset_done), but a job FAILURE or a mid-window leader
// change restarts the window from page 0 and may re-emit an already-emitted page — Loki tolerates
// duplicate operational records, and a completed window (Time advanced) is never re-pulled.
type logsExportLoop struct {
	baseURL, authHdr, authVal string
	hc                        *httpx.Client // control-plane lifecycle calls (shared limiter w/ the other loops)
	dlClient                  *httpx.Client // signed-URL object download (AllowHosts = signedURLAllowHosts)
	sourceInstance            string
	workspaceID               string
	cadence                   time.Duration
	window, settle            time.Duration
	maxBackfill               time.Duration
	pageSize                  int
	maxPagesPerWindow         int
	chunkMaxRecords           int
	downloadMaxBytes          int64 // 0 ⇒ defaultDownloadMaxBytes; a loop field so the truncation path is testable
	jobPollTimeout            time.Duration
	requestedData             []string
	signedURLAllowHosts       []string
	policy                    fieldPolicy
	// useCase is the resolved api_key_use_case slug for this fan-out instance (empty ⇒ legacy/unlabelled).
	// apiKeyIDs is the comma-separated list of API key UUIDs to scope the export filter (empty ⇒ all keys).
	useCase   string
	apiKeyIDs string
	// onGraphSkipped, if set, counts a failed/abandoned export job as an alertable self-metric
	// (→ aip_oi_source_graph_unavailable_total{loop="logs_export",graph=...}) so a flapping export is
	// visible in metrics, not just logs — distinct from window_lag (the "stuck" symptom). nil ⇒ log only.
	onGraphSkipped func(loop, graph string)
	onAuthError    func(loop, source string) // followup §9: 401/403 on a lifecycle call → own alertable signal
	now            func() time.Time
}

// jobFailed records an export-job failure/abandonment as a loud, counted, alertable event.
func (l *logsExportLoop) jobFailed(reason string) {
	if l.onGraphSkipped != nil {
		l.onGraphSkipped("logs_export", reason)
	}
}

// traceIDUnparsed counts a record whose CONFIGURED trace-id metadata field was present but not a parseable
// UUID, so the OTLP trace_id mapping was lost (the raw value still ships as a record attr). Reuses the
// graph-skipped self-metric (→ aip_oi_source_graph_unavailable_total{loop="logs_export",graph="trace_id_unparsed"})
// so a fleet-wide broken mapping — operator configured logs↔traces correlation, upstream changed the format —
// is alertable rather than silently 0% effective.
func (l *logsExportLoop) traceIDUnparsed() {
	if l.onGraphSkipped != nil {
		l.onGraphSkipped("logs_export", "trace_id_unparsed")
	}
}

func (l *logsExportLoop) Cadence() time.Duration { return l.cadence }

// IndexedKeys returns the full set of record fields this loop promotes to LogRecord.IndexedAttributes
// (OTLP resource attrs → Loki stream labels via GS1): the base content-free allow-list ∪
// settings.extra_indexed_fields (already merged into l.policy.indexed at construction). Sorted, for a
// deterministic Loki-budget error message and Key fingerprint. Satisfies source.IndexedKeyDeclarer — the
// composition root budgets these against the Loki max_label_names_per_series limit.
func (l *logsExportLoop) IndexedKeys() []string {
	idx := make([]string, 0, len(l.policy.indexed))
	for k := range l.policy.indexed {
		idx = append(idx, k)
	}
	sort.Strings(idx)
	return idx
}

// Key fingerprints the loop's emitted-log SCHEMA (the indexed attribute key set) so a change to what we
// promote bootstraps a fresh watermark (F37). Logs declare no metric series, so the loop intentionally
// does NOT implement SeriesDeclarer (nothing to ownership-check against the metric plane).
//
// Fan-out instances (one per use-case) fold the slug into the naming component so they each own a
// distinct per-cursor watermark — ownership-safe because logs is NOT a SeriesDeclarer (no duplicate-
// series risk). The legacy unlabelled path (useCase=="") leaves naming=="logs_export" (unchanged key).
func (l *logsExportLoop) Key() model.CheckpointKey {
	naming := "logs_export"
	if l.useCase != "" {
		naming += ",api_key_use_case=" + l.useCase
	}
	return model.CheckpointKey{
		SourceInstance:    l.sourceInstance,
		Loop:              "logs_export",
		OutputFingerprint: model.Fingerprint(l.IndexedKeys(), naming),
	}
}

// Collect advances the export state machine by one step. It returns either an EMPTY batch with the
// watermark unchanged (a job is mid-flight — the loop does not stall and window_lag reflects in-flight
// work), a cursor-only batch (a non-emitting lifecycle step — persisted so the next tick resumes), or a
// populated batch (a download chunk's Logs, with Time advanced only when the WHOLE window completes).
// Any control-plane error returns it verbatim: the scheduler does not advance and re-pulls next tick
// (loud, idempotent — the cursor is unchanged so the step is simply re-done).
func (l *logsExportLoop) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	now := l.now()
	cur := decodeCursor(since.Cursor)
	switch cur.Phase {
	case phaseCreated:
		return l.stepCreated(ctx, since, cur)
	case phasePolling:
		return l.stepPolling(ctx, since, cur, now)
	case phaseDownloading:
		return l.stepDownloading(ctx, since, cur)
	default: // phaseIdle (and any unrecognised phase, which decodeCursor already normalises to idle)
		return l.stepIdle(ctx, since, now)
	}
}

// stepIdle starts a new window. It computes [winMin, winMax] (winMax clamped to the settled cutoff
// now-settle), and if nothing has settled yet returns an empty batch with no advance. Otherwise it
// CREATES the page-0 export (learning total_records ⇒ pages) and moves to `created`. An empty window
// (total 0) advances the frontier cleanly to winMax. A window whose page count exceeds
// max_pages_per_window is a mis-sized window: error loudly (no silent tail-drop) so the operator shrinks
// `window`. Time is unchanged on the created transition (the window isn't done yet).
func (l *logsExportLoop) stepIdle(ctx context.Context, since model.Watermark, now time.Time) (model.Batch, error) {
	winMin := since.Time
	if winMin.IsZero() {
		winMin = now.Add(-l.window) // bootstrap: the first window is the most recent one window back
	}
	// [backfill floor] A watermark older than now-maxBackfill is unstorable — its logs would be rejected
	// by the Loki accept horizon (too-old), so creating an export for it is wasted work. Skip the
	// unstorable span (loud) and resume at the floor, mirroring the analytics loop's max_backfill clamp
	// (F25). The abandoned span is honestly logged; downstream too-old rejects would otherwise count it.
	if floor := now.Add(-l.maxBackfill); winMin.Before(floor) {
		slog.Warn("portkey logs_export: watermark older than max_backfill — skipping the unstorable span",
			"from", winMin.UTC().Format(time.RFC3339), "floor", floor.UTC().Format(time.RFC3339), "source", l.sourceInstance)
		winMin = floor
	}
	winMax := winMin.Add(l.window)
	if cutoff := now.Add(-l.settle); winMax.After(cutoff) {
		winMax = cutoff
	}
	if !winMax.After(winMin) {
		return model.Batch{Key: l.Key(), Watermark: since}, nil // nothing settled yet — empty, no advance
	}
	jobID, total, err := l.createExport(ctx, winMin, winMax, 0)
	if err != nil {
		return model.Batch{}, err
	}
	pages := (total + l.pageSize - 1) / l.pageSize
	if total <= 0 || pages == 0 {
		// Empty settled window: nothing to emit — advance the frontier to winMax, back to idle.
		return model.Batch{Key: l.Key(), Watermark: model.Watermark{Time: winMax}}, nil
	}
	if l.maxPagesPerWindow > 0 && pages > l.maxPagesPerWindow {
		return model.Batch{}, fmt.Errorf("portkey logs_export: window [%s..%s] needs %d pages > max_pages_per_window %d — shrink `window`",
			winMin.UTC().Format(time.RFC3339), winMax.UTC().Format(time.RFC3339), pages, l.maxPagesPerWindow)
	}
	cur := exportCursor{
		Phase: phaseCreated, JobID: jobID,
		WinMin: winMin.UTC().Format(time.RFC3339Nano), WinMax: winMax.UTC().Format(time.RFC3339Nano),
		Page: 0, Pages: pages, TotalRecords: total,
	}
	return model.Batch{Key: l.Key(), Watermark: l.advanceCursor(since.Time, cur)}, nil
}

// stepCreated starts the in-flight job. JobID=="" means a page-rollover left a new page to CREATE first
// (deferred from stepDownloading so a failed chunk-emit never orphans a job): create it, stay `created`.
// Otherwise start the existing draft and move to `polling`, stamping the abandon deadline.
func (l *logsExportLoop) stepCreated(ctx context.Context, since model.Watermark, cur exportCursor) (model.Batch, error) {
	if cur.JobID == "" {
		winMin, winMax, ok := cur.windowBounds()
		if !ok {
			return model.Batch{Key: l.Key(), Watermark: l.resetIdle(since.Time)}, nil
		}
		jobID, _, err := l.createExport(ctx, winMin, winMax, cur.Page)
		if err != nil {
			return model.Batch{}, err
		}
		cur.JobID = jobID
		return model.Batch{Key: l.Key(), Watermark: l.advanceCursor(since.Time, cur)}, nil
	}
	if err := l.startExport(ctx, cur.JobID); err != nil {
		// A /start error may be a LOST ACK rather than a no-op: Portkey can queue the job server-side while
		// the response fails (timeout / transient 5xx). Blindly re-issuing /start then hits 400 AB01 forever
		// — /start is invalid on a non-draft job — and the window never advances (the eks-test logs_export wedge).
		// Recover idempotently: poll the job. If it has already LEFT draft, the start took effect, so advance
		// to polling (stepPolling adjudicates success/failure/in_progress from there). If it is still draft
		// (the start genuinely did not land) or the poll itself errors, surface the original start error and
		// retry next tick — never a false advance off an un-started draft.
		status, perr := l.pollExport(ctx, cur.JobID)
		if perr != nil || status == statusDraft {
			return model.Batch{}, err
		}
	}
	cur.Phase = phasePolling
	cur.PollDeadline = l.now().Add(l.jobPollTimeout).UTC().Format(time.RFC3339Nano)
	return model.Batch{Key: l.Key(), Watermark: l.advanceCursor(since.Time, cur)}, nil
}

// stepPolling polls the in-flight job. success ⇒ download; draft ⇒ re-issue start; queued/in_progress ⇒
// empty no-advance (still running) UNLESS the abandon deadline has passed (cancel + restart the window);
// failed/stopped/anything-else ⇒ restart the window from idle (loud — at-least-once, may re-emit pages).
func (l *logsExportLoop) stepPolling(ctx context.Context, since model.Watermark, cur exportCursor, now time.Time) (model.Batch, error) {
	status, err := l.pollExport(ctx, cur.JobID)
	if err != nil {
		return model.Batch{}, err
	}
	switch status {
	case statusSuccess:
		cur.Phase = phaseDownloading
		cur.PageOffsetDone = 0
		return model.Batch{Key: l.Key(), Watermark: l.advanceCursor(since.Time, cur)}, nil
	case statusDraft:
		cur.Phase = phaseCreated // start was lost — re-issue it next tick
		return model.Batch{Key: l.Key(), Watermark: l.advanceCursor(since.Time, cur)}, nil
	case statusQueued, statusInProgress:
		// PollDeadline is the WRITING leader's wall clock; a new leader compares it against its own clock,
		// so NTP skew can abandon a hair early/late — but the restart is loud + idempotent (re-create is
		// free), so the blast radius is wasted work, never loss. job_poll_timeout (30m) >> realistic skew.
		if deadline, ok := parseCursorTime(cur.PollDeadline); ok && now.After(deadline) {
			slog.Error("portkey logs_export: job exceeded job_poll_timeout — cancelling + restarting window",
				"job", cur.JobID, "source", l.sourceInstance)
			l.jobFailed("export_stuck")
			_ = l.cancelExport(ctx, cur.JobID) // best-effort
			return model.Batch{Key: l.Key(), Watermark: l.resetIdle(since.Time)}, nil
		}
		return model.Batch{Key: l.Key(), Watermark: since}, nil // still running — no advance, cursor unchanged
	default: // failed, stopped, or unexpected
		slog.Error("portkey logs_export: export job failed/stopped — restarting window",
			"job", cur.JobID, "status", status, "source", l.sourceInstance)
		l.jobFailed("export_failed")
		return model.Batch{Key: l.Key(), Watermark: l.resetIdle(since.Time)}, nil
	}
}

// stepDownloading mints a fresh signed URL, validates its host (SSRF gate), and streams ONE chunk of the
// current page (skipping page_offset_done already-consumed lines). It emits the chunk's Logs and:
//   - more of this page remains ⇒ stay `downloading`, advance page_offset_done, Time unchanged;
//   - page done, more pages remain ⇒ next page: clear JobID + go to `created` (the new page's job is
//     CREATED next tick, so a failed chunk-emit here never orphans a job), Time unchanged;
//   - last page done ⇒ advance Watermark.Time = winMax, clear the cursor (back to idle next tick).
func (l *logsExportLoop) stepDownloading(ctx context.Context, since model.Watermark, cur exportCursor) (model.Batch, error) {
	_, winMax, ok := cur.windowBounds()
	if !ok {
		return model.Batch{Key: l.Key(), Watermark: l.resetIdle(since.Time)}, nil
	}
	signedURL, err := l.getDownloadURL(ctx, cur.JobID)
	if err != nil {
		return model.Batch{}, err
	}
	if verr := l.validateSignedURLHost(signedURL); verr != nil {
		// Server-controlled host not allow-listed — refuse to fetch (SSRF gate, §7). Loud, no advance:
		// the host won't change without reconfig, so this is a deliberate loud stall (window_lag grows),
		// never a silent skip or a fetch from an unvalidated host.
		return model.Batch{}, fmt.Errorf("portkey logs_export: refusing download: %w", verr)
	}
	recs, lines, eof, err := l.downloadChunk(ctx, signedURL, cur.PageOffsetDone, l.chunkMaxRecords, winMax)
	if err != nil {
		return model.Batch{}, err
	}
	cur.PageOffsetDone += lines
	if !eof {
		return model.Batch{Key: l.Key(), Logs: recs, Watermark: l.advanceCursor(since.Time, cur)}, nil
	}
	if cur.Page+1 < cur.Pages {
		cur.Page++
		cur.PageOffsetDone = 0
		cur.JobID = "" // defer the next page's create to stepCreated (no orphan on a failed chunk-emit)
		cur.Phase = phaseCreated
		return model.Batch{Key: l.Key(), Logs: recs, Watermark: l.advanceCursor(since.Time, cur)}, nil
	}
	// Last page complete ⇒ the whole window is emitted: advance the frontier, clear the cursor.
	return model.Batch{Key: l.Key(), Logs: recs, Watermark: model.Watermark{Time: winMax}}, nil
}

// advanceCursor builds a watermark that keeps Time fixed (the window isn't done) and carries the new
// cursor. The same-Time/cursor-change relaxation in checkpoint.CheckMonotonic makes it persist.
func (l *logsExportLoop) advanceCursor(t time.Time, c exportCursor) model.Watermark {
	return model.Watermark{Time: t, Cursor: c.encode()}
}

// resetIdle returns to the idle phase at the unchanged Time (re-pull the window from scratch). The
// cursor is the ENCODED idle phase (non-empty) so it PERSISTS even at the zero Time of the first window
// — abandoning a dead job so the next tick re-creates rather than re-polling the failed job forever.
func (l *logsExportLoop) resetIdle(t time.Time) model.Watermark {
	return model.Watermark{Time: t, Cursor: exportCursor{Phase: phaseIdle}.encode()}
}

// windowBounds parses the cursor's RFC3339Nano window edges; ok=false on a corrupt cursor (→ resetIdle).
func (c exportCursor) windowBounds() (winMin, winMax time.Time, ok bool) {
	winMin, ok1 := parseCursorTime(c.WinMin)
	winMax, ok2 := parseCursorTime(c.WinMax)
	return winMin, winMax, ok1 && ok2
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

var _ source.Loop = (*logsExportLoop)(nil)
