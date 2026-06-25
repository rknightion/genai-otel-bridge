// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/rknightion/genai-otel-bridge/internal/httpx"
)

func runsTestClient(t *testing.T) *httpx.Client {
	t.Helper()
	return httpx.New(httpx.Config{UserAgent: "t", Timeout: 5 * time.Second, AllowPrivate: true,
		Limiter: rate.NewLimiter(rate.Inf, 1)})
}

func TestQueryRunsRequestAndParse(t *testing.T) {
	var gotBody map[string]any
	var page int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if atomic.AddInt32(&page, 1) == 1 {
			_, _ = w.Write([]byte(`{"runs":[{"id":"r1","run_type":"llm"}],"cursors":{"next":"gt(cursor,'x')","prev":null}}`))
		} else {
			_, _ = w.Write([]byte(`{"runs":[{"id":"r2"}],"cursors":{"next":null}}`))
		}
	}))
	defer srv.Close()
	l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
		selectFields: defaultRunsFieldPolicy().selectKeys(), pageSize: 100}

	winMin := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	winMax := winMin.Add(time.Hour)
	runs, next, status, _, err := l.queryRuns(context.Background(), []string{"sess-1"}, winMin, winMax, "")
	if err != nil || status != 200 {
		t.Fatalf("err=%v status=%d", err, status)
	}
	if len(runs) != 1 || string(runs[0]["id"]) != `"r1"` || next != "gt(cursor,'x')" {
		t.Fatalf("page1 parse wrong: runs=%v next=%q", runs, next)
	}
	// request shape: session scope from the param
	if s, _ := json.Marshal(gotBody["session"]); string(s) != `["sess-1"]` {
		t.Fatalf("session scope wrong: %s", s)
	}
	if gotBody["order"] != "asc" || gotBody["start_time"] == nil || gotBody["end_time"] == nil {
		t.Fatalf("missing window/order: %v", gotBody)
	}
	if _, hasCursor := gotBody["cursor"]; hasCursor {
		t.Fatal("first page must NOT send a cursor")
	}
	// content-free select
	sel, _ := json.Marshal(gotBody["select"])
	for _, banned := range []string{"inputs", "outputs", "messages", "events", "extra", "serialized",
		"inputs_preview", "outputs_preview", "error", "s3_urls", "manifest", "name"} {
		if strings.Contains(string(sel), `"`+banned+`"`) {
			t.Fatalf("select named content field %q: %s", banned, sel)
		}
	}
	// page 2 (with cursor) terminates
	_, next2, _, _, err := l.queryRuns(context.Background(), []string{"sess-1"}, winMin, winMax, "gt(cursor,'x')")
	if err != nil {
		t.Fatal(err)
	}
	if _, hasCursor := gotBody["cursor"]; !hasCursor {
		t.Fatal("continuation page must send a cursor")
	}
	if next2 != "" {
		t.Fatalf("last page next must be empty, got %q", next2)
	}
}

func TestQueryRunsErrorStatuses(t *testing.T) {
	// Any non-2xx (not just the 422 select case) must surface the upstream error BODY so the failure
	// self-diagnoses in the loop.tick trace exception + warn log (Cycle 2 / the execution_order outage).
	for _, code := range []int{429, 500, 401, 422} {
		body := `{"detail":"UPSTREAM-ERR-MARKER select.6 invalid"}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}))
		l := &runsLoop{baseURL: srv.URL, authHdr: "x-api-key", authVal: "k", hc: runsTestClient(t),
			selectFields: defaultRunsFieldPolicy().selectKeys(), pageSize: 100}
		_, _, status, errBody, err := l.queryRuns(context.Background(), []string{"s"}, time.Unix(0, 0).UTC(), time.Unix(3600, 0).UTC(), "")
		if err != nil || status != code {
			t.Fatalf("code %d: got status=%d err=%v (want status=%d, no transport err)", code, status, err, code)
		}
		if !strings.Contains(errBody, "UPSTREAM-ERR-MARKER") {
			t.Fatalf("code %d: errBody %q must capture the upstream error body", code, errBody)
		}
		srv.Close()
	}
}
