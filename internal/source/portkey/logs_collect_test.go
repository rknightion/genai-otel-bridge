// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// fakeJob is one server-side export. `page` is which current_page it covers; `polls` counts detail GETs.
type fakeJob struct {
	page   int
	polls  int
	status string
}

// fakeExport is an httptest control plane + S3 for the logs-export lifecycle. It is deliberately small
// and configurable per-test: `total` is the matched count returned at create; a job reports `success`
// after `pollsUntilSuccess` detail GETs (or `failStatus` if set); `pageBody(page)` produces the JSONL
// for current_page. `signedURLHost`, when set, rewrites the returned signed_url host (SSRF-reject test).
type fakeExport struct {
	mu                 sync.Mutex
	t                  *testing.T
	srv                *httptest.Server
	jobs               map[string]*fakeJob
	seq                int
	total              int
	pollsUntilSuccess  int
	failStatus         string
	startFailStatus    int // when non-zero, POST /start returns this HTTP status + an AB01 body (lost-ack/non-draft test)
	httpStatusOverride int // when non-zero, every control-plane call returns this HTTP status (auth-fail test)
	signedURLHost      string
	pageBody           func(page int) string // JSONL body for a page
	createBodies       []map[string]json.RawMessage
	downloadHits       int // count of S3 object GETs (re-download observability)
}

func newFakeExport(t *testing.T, total int, pageBody func(page int) string) *fakeExport {
	f := &fakeExport{t: t, jobs: map[string]*fakeJob{}, total: total, pageBody: pageBody}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeExport) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if f.httpStatusOverride != 0 && strings.HasPrefix(p, "/logs/exports") {
		w.WriteHeader(f.httpStatusOverride)
		return
	}
	switch {
	case r.Method == http.MethodPost && p == "/logs/exports":
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.createBodies = append(f.createBodies, body)
		page := 0
		if fb, ok := body["filters"]; ok {
			var filt struct {
				CurrentPage int `json:"current_page"`
			}
			_ = json.Unmarshal(fb, &filt)
			page = filt.CurrentPage
		}
		f.seq++
		id := fmt.Sprintf("job-%d", f.seq)
		f.jobs[id] = &fakeJob{page: page, status: statusDraft}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "total": f.total, "object": "export"})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/start"):
		if f.startFailStatus != 0 {
			// Portkey rejects /start on a job that is no longer in draft with 400 AB01. This models the
			// production lost-ack wedge: a prior /start's effect landed server-side while its response
			// failed, so re-issuing /start now 400s. The job's server-side status is unchanged here.
			w.WriteHeader(f.startFailStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"data":    map[string]any{"message": "Invalid request. Please check and try again.", "errorCode": "AB01"},
			})
			return
		}
		if j := f.jobs[jobIDFromPath(p, "/start")]; j != nil {
			j.status = statusQueued
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "queued", "object": "export"})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/cancel"):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/download"):
		id := jobIDFromPath(p, "/download")
		host := f.srv.Listener.Addr().String()
		if f.signedURLHost != "" {
			host = f.signedURLHost
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"signed_url": "http://" + host + "/s3/" + id + ".jsonl?X-Amz-Expires=21600", "object": "export",
		})
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/s3/"):
		f.downloadHits++
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/s3/"), ".jsonl")
		j := f.jobs[id]
		page := 0
		if j != nil {
			page = j.page
		}
		_, _ = w.Write([]byte(f.pageBody(page)))
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/logs/exports/"):
		id := strings.TrimPrefix(p, "/logs/exports/")
		j := f.jobs[id]
		if j == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		j.polls++
		status := statusSuccess
		if f.failStatus != "" {
			status = f.failStatus
		} else if j.polls <= f.pollsUntilSuccess {
			status = statusInProgress
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": status, "object": "export"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func jobIDFromPath(path, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/logs/exports/"), suffix)
}

// exportLine builds one valid content-free JSONL export record (with PII/config blobs injected, as the
// live API does, so the strip is exercised end-to-end).
func exportLine(i int, model string) string {
	rec := map[string]any{
		"id": "r" + strconv.Itoa(i), "trace_id": "t" + strconv.Itoa(i),
		"created_at": "2026-06-18T14:23:45Z", "response_time": 1485,
		"response_status_code": 200, "ai_org": "openai", "ai_model": model,
		"cost": 0.3194, "total_units": 1312, "currency": "USD",
		"metadata":       map[string]any{"owner": "a person", "data_classification": "C3"},
		"portkeyHeaders": map[string]any{"x-portkey-config": map[string]any{}},
		"prompt":         "secret",
	}
	b, _ := json.Marshal(rec)
	return string(b) + "\n"
}

// nLines concatenates n export records (same model), the JSONL page body helper.
func nLines(n int, model string) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(exportLine(i, model))
	}
	return b.String()
}

func logsCfg(srv *httptest.Server, settings map[string]string) config.SourceConfig {
	host := srv.Listener.Addr().String() // 127.0.0.1:port — strip the port for the host allow-list
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	s := map[string]string{"workspace_id": "ws-test", "signed_url_allow_hosts": host}
	for k, v := range settings {
		s[k] = v
	}
	return config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"logs_export": {
			Enabled: true, Cadence: config.Duration(time.Minute), Settings: s,
		}},
	}
}

// TestLogsExtraIndexedFieldsInRequestedData: a field promoted via extra_indexed_fields must be merged
// into requested_data so Portkey actually includes it in the export (else the strip allow-lists a field
// the export never carried).
func TestLogsExtraIndexedFieldsInRequestedData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	l := mkLogsLoop(t, logsCfg(srv, map[string]string{"window": "1h", "settle": "10m", "extra_indexed_fields": "cache_status"}), time.Now().UTC())
	found := false
	for _, f := range l.requestedData {
		if f == "cache_status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("extra_indexed_fields must be merged into requested_data; got %v", l.requestedData)
	}
}

func mkLogsLoop(t *testing.T, cfg config.SourceConfig, now time.Time) *logsExportLoop {
	t.Helper()
	src, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	for _, lp := range src.Loops() {
		if ll, ok := lp.(*logsExportLoop); ok {
			ll.now = func() time.Time { return now }
			return ll
		}
	}
	t.Fatal("no logs_export loop built")
	return nil
}

// drive runs the step machine to completion: it feeds each returned watermark back as the next `since`
// (simulating the scheduler's commit), accumulates emitted Logs, and stops when Time advances past the
// start frontier (window complete) or maxSteps is hit. Returns the collected logs + the final watermark.
func drive(t *testing.T, l *logsExportLoop, start model.Watermark, maxSteps int) ([]model.LogRecord, model.Watermark) {
	t.Helper()
	var logs []model.LogRecord
	wm := start
	for step := 0; step < maxSteps; step++ {
		b, err := l.Collect(context.Background(), wm)
		if err != nil {
			t.Fatalf("step %d: Collect error: %v", step, err)
		}
		logs = append(logs, b.Logs...)
		wm = b.Watermark
		if wm.Time.After(start.Time) || (start.Time.IsZero() && !wm.Time.IsZero()) {
			return logs, wm // window completed (frontier advanced)
		}
	}
	t.Fatalf("did not complete within %d steps (last wm=%+v)", maxSteps, wm)
	return logs, wm
}

// TestLogsCollectHappyPathSinglePage drives a single-page window create→start→poll→download→advance and
// asserts: the right number of content-free records emitted, PII/content stripped, frontier = win_max,
// cursor cleared, and the create request carried no content field.
func TestLogsCollectHappyPathSinglePage(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "gpt-5") })
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	logs, wm := drive(t, l, model.Watermark{Time: now.Add(-time.Hour)}, 10)
	if len(logs) != 3 {
		t.Fatalf("emitted %d logs, want 3", len(logs))
	}
	for _, lr := range logs {
		if lr.Body != "" {
			t.Fatalf("Body must be empty, got %q", lr.Body)
		}
		if lr.IndexedAttributes["ai_model"] != "gpt-5" || lr.IndexedAttributes["ai_org"] != "openai" {
			t.Fatalf("indexed attrs wrong: %v", lr.IndexedAttributes)
		}
		for _, banned := range []string{"metadata", "portkeyHeaders", "prompt", "owner", "data_classification"} {
			if _, ok := lr.RecordAttributes[banned]; ok {
				t.Fatalf("content leaked into record attrs: %q", banned)
			}
			if _, ok := lr.IndexedAttributes[banned]; ok {
				t.Fatalf("content leaked into indexed attrs: %q", banned)
			}
		}
	}
	// Window complete: frontier = win_max = now-settle (min(winMin+1h, now-10m)).
	wantWinMax := now.Add(-10 * time.Minute)
	if !wm.Time.Equal(wantWinMax) {
		t.Fatalf("frontier=%v want win_max=%v", wm.Time, wantWinMax)
	}
	if wm.Cursor != "" {
		t.Fatalf("cursor must clear on window completion, got %q", wm.Cursor)
	}
	// Create request must never name a content field.
	if len(f.createBodies) == 0 {
		t.Fatal("no create request captured")
	}
	rd, _ := json.Marshal(f.createBodies[0]["requested_data"])
	for _, banned := range []string{"prompt", "request", "response", "metadata", "portkeyHeaders"} {
		if strings.Contains(string(rd), `"`+banned+`"`) {
			t.Fatalf("create requested_data named content field %q: %s", banned, rd)
		}
	}
}

// TestLogsCollectMultiPage: total exceeds one page → multiple sequential export jobs, all emitted, then
// the frontier advances exactly once (when the LAST page completes).
func TestLogsCollectMultiPage(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	// total 5, page_size 2 ⇒ 3 pages (2,2,1). Each page body returns page_size-or-less lines.
	pages := map[int]int{0: 2, 1: 2, 2: 1}
	f := newFakeExport(t, 5, func(page int) string { return nLines(pages[page], "m") })
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m", "page_size": "2", "chunk_max_records": "100"}), now)

	logs, wm := drive(t, l, model.Watermark{Time: now.Add(-time.Hour)}, 40)
	if len(logs) != 5 {
		t.Fatalf("emitted %d logs across pages, want 5", len(logs))
	}
	if !wm.Time.Equal(now.Add(-10 * time.Minute)) {
		t.Fatalf("frontier=%v want win_max", wm.Time)
	}
	if f.seq != 3 {
		t.Fatalf("created %d export jobs, want 3 (one per page)", f.seq)
	}
}

// TestLogsCollectChunkedPage: a single page larger than chunk_max_records is emitted across multiple
// download chunks (bounded memory), and the frontier advances once at the end.
func TestLogsCollectChunkedPage(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 10, func(page int) string { return nLines(10, "m") })
	// page_size 50000 (one page), chunk_max_records 4 ⇒ chunks of 4,4,2 (+ a trailing empty read).
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m", "chunk_max_records": "4"}), now)

	logs, wm := drive(t, l, model.Watermark{Time: now.Add(-time.Hour)}, 40)
	if len(logs) != 10 {
		t.Fatalf("emitted %d logs in chunks, want 10", len(logs))
	}
	if !wm.Time.Equal(now.Add(-10 * time.Minute)) {
		t.Fatalf("frontier=%v want win_max", wm.Time)
	}
}

// TestLogsCollectEmptyWindow: a window with zero matched records advances the frontier cleanly with NO
// logs and NO download (create returns total 0 → skip straight to win_max).
func TestLogsCollectEmptyWindow(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 0, func(page int) string { return "" })
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	logs, wm := drive(t, l, model.Watermark{Time: now.Add(-time.Hour)}, 5)
	if len(logs) != 0 {
		t.Fatalf("empty window emitted %d logs, want 0", len(logs))
	}
	if !wm.Time.Equal(now.Add(-10 * time.Minute)) {
		t.Fatalf("frontier=%v want win_max", wm.Time)
	}
	if f.downloadHits != 0 {
		t.Fatalf("empty window must not download, got %d hits", f.downloadHits)
	}
}

// TestLogsCollectNothingSettled: when the window upper bound (now-settle) is not past the lower bound,
// Collect returns empty with NO advance and NO create (nothing has settled yet).
func TestLogsCollectNothingSettled(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 100, func(page int) string { return nLines(100, "m") })
	// since.Time = now-5m, settle 10m ⇒ win_max = now-10m < winMin ⇒ nothing settled.
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	b, err := l.Collect(context.Background(), model.Watermark{Time: now.Add(-5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Logs) != 0 || b.Watermark.Time != now.Add(-5*time.Minute) {
		t.Fatalf("nothing-settled must be empty + no advance, got %+v", b.Watermark)
	}
	if f.seq != 0 {
		t.Fatalf("must not create an export when nothing settled, created %d", f.seq)
	}
}

// TestLogsCollectInProgressNoAdvance: while the job is queued/in_progress, Collect returns empty with the
// cursor UNCHANGED (the loop re-polls next tick; window_lag reflects the in-flight work).
func TestLogsCollectInProgressNoAdvance(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	f.pollsUntilSuccess = 2 // first two detail GETs report in_progress
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	// idle→created→polling, then a polling tick that returns in_progress (no advance, same cursor).
	wm := model.Watermark{Time: now.Add(-time.Hour)}
	var pollCursor string
	for step := 0; step < 4; step++ {
		b, err := l.Collect(context.Background(), wm)
		if err != nil {
			t.Fatal(err)
		}
		cur := decodeCursor(b.Watermark.Cursor)
		if cur.Phase == phasePolling && b.Watermark.Cursor == wm.Cursor {
			pollCursor = b.Watermark.Cursor // an in-progress re-poll: cursor unchanged
			break
		}
		wm = b.Watermark
	}
	if pollCursor == "" {
		t.Fatal("expected an in_progress poll tick with an unchanged cursor")
	}
}

// TestLogsCollectFailedJobRestarts: a failed export resets the cursor to idle (persisted, non-empty) so
// the next tick re-creates rather than re-polling the dead job — never a silent stall on a dead job.
func TestLogsCollectFailedJobRestarts(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	f.failStatus = "failed"
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	wm := model.Watermark{Time: now.Add(-time.Hour)}
	// idle(create job-1)→created(start)→polling(failed→resetIdle).
	b1, _ := l.Collect(context.Background(), wm)
	b2, _ := l.Collect(context.Background(), b1.Watermark)
	b3, _ := l.Collect(context.Background(), b2.Watermark)
	cur := decodeCursor(b3.Watermark.Cursor)
	if cur.Phase != phaseIdle {
		t.Fatalf("failed job must reset to idle, got phase %q", cur.Phase)
	}
	if b3.Watermark.Cursor == "" {
		t.Fatal("reset-to-idle cursor must be NON-empty so it persists at the zero frontier and abandons the dead job")
	}
	if !b3.Watermark.Time.Equal(now.Add(-time.Hour)) {
		t.Fatalf("failed job must NOT advance Time, got %v", b3.Watermark.Time)
	}
}

// TestLogsCollectStartLostAckRecovers reproduces the production wedge (eks-test, 2026-06-21): POST /start
// returned a 400/AB01 to the loop AFTER Portkey had already queued the job server-side (a lost ack), so the
// job ran to success while the cursor stayed in `created`. The old code re-issued /start on the now-non-draft
// job every tick (400 AB01 forever), pinning the window. The loop must instead poll on a /start error, see
// the job has left draft, advance to polling, and complete the window.
func TestLogsCollectStartLostAckRecovers(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	f.startFailStatus = http.StatusBadRequest // /start always 400s (AB01), as on a job that already started

	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	logs, wm := drive(t, l, model.Watermark{Time: now.Add(-time.Hour)}, 10)
	if len(logs) != 3 {
		t.Fatalf("emitted %d logs, want 3 — the loop did not recover from the /start lost-ack", len(logs))
	}
	if !wm.Time.Equal(now.Add(-10 * time.Minute)) {
		t.Fatalf("frontier did not advance to win_max after recovery, got %v", wm.Time)
	}
}

// TestLogsCollectStartGenuineFailureNoFalseAdvance guards the recovery's other arm: a /start error where the
// job is GENUINELY still in draft (the start did not take effect) must surface the error and stay in `created`
// to retry next tick — the recovery poll must never mistake an un-started draft for a started job.
func TestLogsCollectStartGenuineFailureNoFalseAdvance(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	f.startFailStatus = http.StatusBadRequest
	f.failStatus = statusDraft // the recovery poll reports the job still in draft (start never landed)

	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	wm := model.Watermark{Time: now.Add(-time.Hour)}
	b1, err := l.Collect(context.Background(), wm) // idle→created (create job-1)
	if err != nil {
		t.Fatal(err)
	}
	if cur := decodeCursor(b1.Watermark.Cursor); cur.Phase != phaseCreated {
		t.Fatalf("want phase created after job creation, got %q", cur.Phase)
	}
	b2, err := l.Collect(context.Background(), b1.Watermark) // stepCreated: start 400 + poll says draft → error
	if err == nil {
		t.Fatalf("a genuine start failure (job still draft) must surface the error, not advance; got cursor %q", b2.Watermark.Cursor)
	}
}

// TestLogsCollectStuckJobTimeout: a job still in_progress past job_poll_timeout is cancelled + the window
// restarts from idle (loud, bounded — never blocks the window forever).
func TestLogsCollectStuckJobTimeout(t *testing.T) {
	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	f.pollsUntilSuccess = 1000 // never succeeds
	cur := base
	clock := &cur
	src, err := New(logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m", "job_poll_timeout": "2m"}), source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	var l *logsExportLoop
	for _, lp := range src.Loops() {
		l = lp.(*logsExportLoop)
	}
	l.now = func() time.Time { return *clock }

	wm := model.Watermark{Time: base.Add(-time.Hour)}
	b, _ := l.Collect(context.Background(), wm) // idle → created
	wm = b.Watermark
	b, _ = l.Collect(context.Background(), wm) // created → polling (deadline = base+2m)
	wm = b.Watermark
	*clock = base.Add(3 * time.Minute) // advance past the deadline
	b, _ = l.Collect(context.Background(), wm)
	if decodeCursor(b.Watermark.Cursor).Phase != phaseIdle {
		t.Fatalf("stuck job past deadline must reset to idle, got %q", b.Watermark.Cursor)
	}
}

// TestLogsCollectRejectsUnlistedSignedURLHost: a download URL whose host is NOT in signed_url_allow_hosts
// is refused before any fetch (SSRF gate) — Collect errors loudly, never fetches.
func TestLogsCollectRejectsUnlistedSignedURLHost(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	f.signedURLHost = "evil.example.com:80" // not in the allow-list
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)

	wm := model.Watermark{Time: now.Add(-time.Hour)}
	var lastErr error
	for step := 0; step < 5; step++ {
		b, err := l.Collect(context.Background(), wm)
		if err != nil {
			lastErr = err
			break
		}
		wm = b.Watermark
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "signed_url_allow_hosts") {
		t.Fatalf("expected a signed-url host rejection, got %v", lastErr)
	}
	if f.downloadHits != 0 {
		t.Fatalf("must not fetch an unlisted host, got %d hits", f.downloadHits)
	}
}

// TestLogsExportRejectsNonZeroLoopWindow: the loop is snapshot-scheduled (relies on LoopConfig.Window==0),
// so a stray non-zero loops.logs_export.window (e.g. copied from an analytics block) must fail fast —
// otherwise the scheduler would spuriously accelerate ticks and count backfill_unstorable for it.
func TestLogsExportRejectsNonZeroLoopWindow(t *testing.T) {
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1", SourceInstance: "pk",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 1},
		Loops: map[string]config.LoopConfig{"logs_export": {
			Enabled: true, Cadence: config.Duration(time.Minute),
			Window:   config.Duration(50 * time.Minute), // ← the footgun: must be rejected
			Settings: map[string]string{"workspace_id": "ws", "signed_url_allow_hosts": "h.example.com"},
		}},
	}
	if _, err := New(cfg, source.Deps{}); err == nil {
		t.Fatal("a non-zero loops.logs_export.window must fail fast (snapshot-scheduled loop)")
	}
}

// TestLogsCollectClampsBackfill: a watermark older than max_backfill is clamped to the floor, so the
// created export covers [now-max_backfill, …] — never an unstorable (too-old) span.
func TestLogsCollectClampsBackfill(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 1, func(page int) string { return nLines(1, "m") })
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m", "max_backfill": "1h"}), now)

	// since.Time is 10h old — far past the 1h backfill floor.
	_, err := l.Collect(context.Background(), model.Watermark{Time: now.Add(-10 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.createBodies) == 0 {
		t.Fatal("no create captured")
	}
	var filt struct {
		Min string `json:"time_of_generation_min"`
	}
	_ = json.Unmarshal(f.createBodies[0]["filters"], &filt)
	want := now.Add(-time.Hour).UTC().Format(time.RFC3339) // clamped to the floor, not now-10h
	if filt.Min != want {
		t.Fatalf("create window min=%q want clamped floor %q (backfill clamp)", filt.Min, want)
	}
}

// TestLogsCollectFailedJobFiresMetric: a failed export fires the OnGraphSkipped hook (→ a counted,
// alertable self-metric), so a flapping export is visible in metrics, not just logs.
func TestLogsCollectFailedJobFiresMetric(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	f.failStatus = "failed"
	var skips []string
	deps := source.Deps{OnGraphSkipped: func(loop, graph string) {
		if loop == "logs_export" {
			skips = append(skips, graph)
		}
	}}
	src, err := New(logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), deps)
	if err != nil {
		t.Fatal(err)
	}
	var l *logsExportLoop
	for _, lp := range src.Loops() {
		l = lp.(*logsExportLoop)
	}
	l.now = func() time.Time { return now }

	wm := model.Watermark{Time: now.Add(-time.Hour)}
	for step := 0; step < 3; step++ { // idle→created→polling(failed)
		b, _ := l.Collect(context.Background(), wm)
		wm = b.Watermark
	}
	if len(skips) != 1 || skips[0] != "export_failed" {
		t.Fatalf("failed job must fire OnGraphSkipped(export_failed), got %v", skips)
	}
}

// TestLogsCollectTruncatedDownloadErrors: a signed-URL object exceeding the byte cap is a LOUD error
// (retryable), never a silent eof that would advance the window over the dropped tail.
func TestLogsCollectTruncatedDownloadErrors(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(100, "m") }) // ~100 lines body
	l := mkLogsLoop(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), now)
	l.downloadMaxBytes = 50 // far below the body size ⇒ truncation

	wm := model.Watermark{Time: now.Add(-time.Hour)}
	var lastErr error
	for step := 0; step < 5; step++ {
		b, err := l.Collect(context.Background(), wm)
		if err != nil {
			lastErr = err
			break
		}
		wm = b.Watermark
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "truncated") {
		t.Fatalf("a truncated download must error loudly, got %v", lastErr)
	}
}

// TestLogsCollectResumesInFlightJobAcrossLeaderChange: a NEW loop instance (simulating a new leader)
// handed the persisted cursor RESUMES the in-flight job (polls/downloads the SAME job id) rather than
// creating a new one — idempotent failover.
func TestLogsCollectResumesInFlightJobAcrossLeaderChange(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "m") })
	cfg := logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"})
	l1 := mkLogsLoop(t, cfg, now)

	// Leader 1: idle→created→polling, then crash. Capture the polling cursor.
	wm := model.Watermark{Time: now.Add(-time.Hour)}
	b, _ := l1.Collect(context.Background(), wm) // create job-1
	wm = b.Watermark
	b, _ = l1.Collect(context.Background(), wm) // start
	wm = b.Watermark
	cur := decodeCursor(wm.Cursor)
	if cur.Phase != phasePolling || cur.JobID != "job-1" {
		t.Fatalf("expected polling job-1, got %+v", cur)
	}
	createdBefore := f.seq

	// Leader 2: a fresh loop resumes from the SAME cursor and drives to completion.
	l2 := mkLogsLoop(t, cfg, now)
	logs, _ := drive(t, l2, wm, 10)
	if len(logs) != 3 {
		t.Fatalf("resumed leader emitted %d logs, want 3", len(logs))
	}
	if f.seq != createdBefore {
		t.Fatalf("resume must NOT create a new job (created %d→%d) — it resumes job-1", createdBefore, f.seq)
	}
}

// mkLogsLoopDeps mirrors mkLogsLoop but injects source.Deps (for hook assertions).
func mkLogsLoopDeps(t *testing.T, cfg config.SourceConfig, deps source.Deps, now time.Time) *logsExportLoop {
	t.Helper()
	src, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	for _, lp := range src.Loops() {
		if ll, ok := lp.(*logsExportLoop); ok {
			ll.now = func() time.Time { return now }
			return ll
		}
	}
	t.Fatal("no logs_export loop built")
	return nil
}

// TestLogsExportAuthErrorFiresHook: a 401 on the create lifecycle call fires Deps.OnAuthError with
// (logs_export, <instance>) — the credential-failure signal the logs lifecycle previously lacked.
func TestLogsExportAuthErrorFiresHook(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 0, func(int) string { return "" })
	f.httpStatusOverride = 401 // every control-plane call returns 401
	var auth [][2]string
	deps := source.Deps{OnAuthError: func(loop, s string) { auth = append(auth, [2]string{loop, s}) }}
	l := mkLogsLoopDeps(t, logsCfg(f.srv, map[string]string{"window": "1h", "settle": "10m"}), deps, now)
	// One Collect step attempts createExport → 401 → lifecycle fires the hook then returns the error.
	_, _ = l.Collect(context.Background(), model.Watermark{})
	if len(auth) != 1 || auth[0] != [2]string{"logs_export", "pk-test"} {
		t.Fatalf("OnAuthError want one (logs_export,pk-test), got %v", auth)
	}
}
