// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

// authStub counts AuthError calls; embeds NoopMetrics for the rest of the schedule.Metrics interface.
type authStub struct {
	schedule.NoopMetrics
	calls int
}

func (a *authStub) AuthError(string, string) { a.calls++ }

// TestAuthErrorHookLogsRateLimitedWarn: the wired auth hook counts the metric on every occurrence but
// emits at most one WARN per window (so a persistent bad key doesn't spam stdout). Not parallel — it
// swaps the process-global slog default.
func TestAuthErrorHookLogsRateLimitedWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	m := &authStub{}
	h := authErrorHook(m, logging.NewLimiter(time.Minute))
	h("analytics", "portkey-prod")
	h("analytics", "portkey-prod") // back-to-back: throttled

	if m.calls != 2 {
		t.Fatalf("metric must record every occurrence, got %d", m.calls)
	}
	out := buf.String()
	if n := strings.Count(out, `msg="upstream auth failure (401/403); check source credentials"`); n != 1 {
		t.Fatalf("expected exactly one rate-limited auth warn, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "loop=analytics") || !strings.Contains(out, "source=portkey-prod") {
		t.Fatalf("auth warn should carry loop + source, got:\n%s", out)
	}
}
