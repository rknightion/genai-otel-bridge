// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// The backend rejects a changed value at an existing (series, ts), so the groups snapshot loop emits at
// most once per minute-truncated stamp; a same-stamp poll makes no vendor call and advances liveness only.
func TestGroupsSameMinuteRepollSkipsVendor(t *testing.T) {
	var queries []string
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a", "b")}}, nil, &queries)
	defer srv.Close()
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"emit_prompts": "false"}), source.Deps{}, now)

	b1, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b1.Samples) == 0 {
		t.Fatalf("first poll: err=%v samples=%d", err, len(b1.Samples))
	}
	calls := len(queries)
	now = now.Add(55 * time.Second)
	gl.now = func() time.Time { return now }
	b2, err := gl.Collect(context.Background(), b1.Watermark)
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Samples) != 0 || len(queries) != calls {
		t.Fatalf("same-minute re-poll: samples=%d extra vendor calls=%d", len(b2.Samples), len(queries)-calls)
	}
	if !b2.Watermark.Time.Equal(now) {
		t.Fatalf("watermark = %s, want %s", b2.Watermark.Time, now)
	}
	now = now.Add(5 * time.Second) // next minute
	gl.now = func() time.Time { return now }
	if b3, err := gl.Collect(context.Background(), b2.Watermark); err != nil || len(b3.Samples) == 0 {
		t.Fatalf("next-minute poll: err=%v samples=%d", err, len(b3.Samples))
	}
}

func TestGroupsSameMinuteRepollFailureDoesNotConsumeStamp(t *testing.T) {
	status := map[string]map[int]int{"ai-models": {0: http.StatusInternalServerError}}
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, status, nil)
	defer srv.Close()
	now := time.Date(2026, 11, 16, 12, 0, 2, 0, time.UTC)
	gl := mkGroups(t, groupsCfg(srv, map[string]string{"emit_prompts": "false"}), source.Deps{}, now)
	if _, err := gl.Collect(context.Background(), model.Watermark{}); err == nil {
		t.Fatal("expected the all-endpoints-failed poll to error")
	}
	delete(status, "ai-models") // vendor recovers
	now = now.Add(10 * time.Second)
	gl.now = func() time.Time { return now }
	b, err := gl.Collect(context.Background(), model.Watermark{})
	if err != nil || len(b.Samples) == 0 {
		t.Fatalf("retry in the same minute must emit: err=%v samples=%d", err, len(b.Samples))
	}
}
