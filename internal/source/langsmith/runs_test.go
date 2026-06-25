// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

func TestRunsCursorRoundTrip(t *testing.T) {
	c := runsCursor{WinMin: "2026-06-19T00:00:00Z", WinMax: "2026-06-19T01:00:00Z", Next: "gt(cursor,'x')", Page: 2}
	if got := decodeRunsCursor(c.encode()); got != c {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, c)
	}
	if decodeRunsCursor("").WinMax != "" { // empty cursor = idle (no window)
		t.Fatal("empty cursor must be idle")
	}
	if decodeRunsCursor("{not json").WinMax != "" { // corrupt = idle (safe restart)
		t.Fatal("corrupt cursor must reset to idle")
	}
	a, b, ok := c.windowBounds()
	if !ok || a.UTC().Format(time.RFC3339) != "2026-06-19T00:00:00Z" || b.UTC().Format(time.RFC3339) != "2026-06-19T01:00:00Z" {
		t.Fatalf("windowBounds wrong: %v %v %v", a, b, ok)
	}
	if _, _, ok := (runsCursor{}).windowBounds(); ok {
		t.Fatal("empty window bounds must be not-ok")
	}
}

func TestNewBuildsRunsAndSessions(t *testing.T) {
	sc := config.SourceConfig{Type: "langsmith", BaseURL: "https://x",
		Auth: config.AuthConfig{Header: "x-api-key", Value: "k"}, SourceInstance: "ls",
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 3},
		Loops: map[string]config.LoopConfig{
			"sessions": {Enabled: true, Cadence: config.Duration(time.Minute)},
			"runs":     {Enabled: true, Cadence: config.Duration(time.Minute), Settings: map[string]string{"session_ids": "s1"}},
		}}
	src, err := New(sc, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	loops := map[string]bool{}
	for _, lp := range src.Loops() {
		loops[lp.Key().Loop] = true
	}
	if !loops["sessions"] || !loops["runs"] || len(src.Loops()) != 2 {
		t.Fatalf("want sessions+runs, got %v (n=%d)", loops, len(src.Loops()))
	}

	// only runs enabled → 1 loop
	scR := sc
	scR.Loops = map[string]config.LoopConfig{"runs": sc.Loops["runs"]}
	srcR, err := New(scR, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcR.Loops()) != 1 || srcR.Loops()[0].Key().Loop != "runs" {
		t.Fatalf("only-runs wrong: n=%d", len(srcR.Loops()))
	}

	// neither enabled → 0 loops
	scN := sc
	scN.Loops = map[string]config.LoopConfig{}
	srcN, err := New(scN, source.Deps{})
	if err != nil || len(srcN.Loops()) != 0 {
		t.Fatalf("neither: n=%d err=%v", len(srcN.Loops()), err)
	}
}
