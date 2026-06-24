// SPDX-License-Identifier: AGPL-3.0-only

package schedule

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// errLoop fails Collect with a fixed generic upstream error (e.g. a 5xx).
type errLoop struct{ key model.CheckpointKey }

func (e *errLoop) Key() model.CheckpointKey { return e.key }
func (e *errLoop) Cadence() time.Duration   { return time.Minute }
func (e *errLoop) Collect(context.Context, model.Watermark) (model.Batch, error) {
	return model.Batch{}, errors.New("boom: upstream 500")
}

// captureLogs redirects the process-global slog default to a buffer for the duration of the test and
// restores it after. Tests using it must NOT call t.Parallel() (the default is process-global).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestCollectFailureLogsRateLimitedWarn: a failed collect must produce a WARN on stdout (so operators
// running `kubectl logs` see it), and a repeat within the window must be throttled to one line.
func TestCollectFailureLogsRateLimitedWarn(t *testing.T) {
	buf := captureLogs(t)

	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	cp := newMemCP()
	cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(1000, 0).UTC(), Epoch: 1})
	loop := &errLoop{key: key}
	m := &capMetrics{}
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
	sch := NewScheduler(nil, m)
	now := time.Unix(1100, 0).UTC()
	spec := LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour}
	sch.runOnce(leaderCtx(), spec, now)
	sch.runOnce(leaderCtx(), spec, now) // back-to-back: throttled by the 1-min limiter

	out := buf.String()
	if n := strings.Count(out, `msg="collect failed"`); n != 1 {
		t.Fatalf("expected exactly one rate-limited 'collect failed' warn, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("collect failure should log at WARN, got:\n%s", out)
	}
	if !strings.Contains(out, "loop=analytics") {
		t.Fatalf("collect failure warn should carry loop=analytics, got:\n%s", out)
	}
}

// TestFirstSuccessLogsOncePerLeadership: the first successful watermark commit logs one INFO liveness
// line; subsequent commits are quiet; a Reset (re-election) re-arms it.
func TestFirstSuccessLogsOncePerLeadership(t *testing.T) {
	buf := captureLogs(t)

	em := &fakeEmitter{byTS: map[int64]error{}}
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})

	r.ProcessBatch(leaderCtx(), batchAt(key, 60))
	r.ProcessBatch(leaderCtx(), batchAt(key, 120))
	const msg = `msg="loop committed first watermark advance (leader healthy)"`
	if n := strings.Count(buf.String(), msg); n != 1 {
		t.Fatalf("expected exactly one first-success info per leadership, got %d:\n%s", n, buf.String())
	}

	r.Reset() // re-election
	r.ProcessBatch(leaderCtx(), batchAt(key, 180))
	if n := strings.Count(buf.String(), msg); n != 2 {
		t.Fatalf("a new leadership's first success should log again, got %d total", n)
	}
}
