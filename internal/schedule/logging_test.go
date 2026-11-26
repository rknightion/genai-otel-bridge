// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
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

// TestTerminalHaltLogsDegradeOnce guards [#93] criterion 3: the 'loop degraded' ERROR must fire ONCE
// per degrade event, not once per tick. Before the fix, the interior-bucket commit cleared the degrade
// set on the same batch, so the next tick (which re-collects from the committed frontier and re-hits the
// terminal reject as its FIRST bucket) re-degraded and logged a second time. Now the degrade sticks
// (enterDegraded only logs on a false→true transition), so the modelled re-tick emits no second line.
func TestTerminalHaltLogsDegradeOnce(t *testing.T) {
	buf := captureLogs(t)

	em := &fakeEmitter{byTS: map[int64]error{120: &emit.RejectError{Reason: emit.ReasonUnknown, Status: 400, Msg: "bad"}}}
	cp := newMemCP()
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	r := NewLoopRunner(fakeLoop{key: key}, em, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	// Tick 1: batch [60,120,180]; bucket 60 commits (interior advance), bucket 120 terminal → degrade sticks.
	r.ProcessBatch(leaderCtx(), batchAt(key, 60, 120, 180))
	// Tick 2 models the next collect from the committed frontier (60): the batch now STARTS at the terminal
	// bucket 120 with no interior advance. Since tick 1 did not clear the degrade, this must NOT re-log.
	r.ProcessBatch(leaderCtx(), batchAt(key, 120, 180))
	const degradeMsg = `msg="loop degraded (loud halt, scheduler will back off)"`
	if n := strings.Count(buf.String(), degradeMsg); n != 1 {
		t.Fatalf("[#93] 'loop degraded' must log once per event, not once per tick; got %d:\n%s", n, buf.String())
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

// TestSchedulerLimiterKeyedByPipeline [#521]: two specs with one loop name and different pipelines both
// log their collect failure in the same minute, each with its tenant and pipeline. Keyed by the loop name
// alone, one tenant's failing loop would suppress another tenant's warning.
func TestSchedulerLimiterKeyedByPipeline(t *testing.T) {
	buf := captureLogs(t)

	m := &capMetrics{}
	sch := NewScheduler(nil, m)
	now := time.Unix(1100, 0).UTC()
	var specs []LoopSpec
	for _, p := range []Pipeline{{Tenant: "t1", ID: "t1.a"}, {Tenant: "t2", ID: "t2.a"}} {
		key := model.CheckpointKey{SourceInstance: "mt." + p.ID, Loop: "analytics", OutputFingerprint: "fp"}
		loop := &errLoop{key: key}
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, m, WithPipeline(p))
		specs = append(specs, LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, MaxBackfill: time.Hour, Pipeline: p})
	}
	for range 2 { // the second round is inside the minute: throttled per pipeline
		for _, sp := range specs {
			sch.runOnce(leaderCtx(), sp, now)
		}
	}

	lines := linesWith(buf.String(), `msg="collect failed"`)
	if len(lines) != 2 {
		t.Fatalf("want one 'collect failed' warn per pipeline (2), got %d:\n%s", len(lines), buf.String())
	}
	for i, want := range []string{"tenant=t1 pipeline=t1.a", "tenant=t2 pipeline=t2.a"} {
		if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], "loop=analytics") {
			t.Errorf("warn %d = %q, want loop=analytics and %q", i, lines[i], want)
		}
	}
}

// TestSchedulerCheckpointLoadLimiterKeyedByPipeline [#521]: the checkpoint-load warning is keyed and
// labelled by pipeline the same way.
func TestSchedulerCheckpointLoadLimiterKeyedByPipeline(t *testing.T) {
	buf := captureLogs(t)

	m := &capMetrics{}
	sch := NewScheduler(nil, m)
	now := time.Unix(1100, 0).UTC()
	for _, p := range []Pipeline{{Tenant: "t1", ID: "t1.a"}, {Tenant: "t2", ID: "t2.a"}} {
		key := model.CheckpointKey{SourceInstance: "mt." + p.ID, Loop: "analytics", OutputFingerprint: "fp"}
		loop := &errLoop{key: key}
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, loadErrorCP{}, source.NewGuard(source.GuardConfig{}), 4, 1, m)
		sch.runOnce(leaderCtx(), LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, Pipeline: p}, now)
	}
	lines := linesWith(buf.String(), `msg="checkpoint load failed; skipping tick"`)
	if len(lines) != 2 {
		t.Fatalf("want one checkpoint-load warn per pipeline (2), got %d:\n%s", len(lines), buf.String())
	}
	for i, want := range []string{"tenant=t1 pipeline=t1.a", "tenant=t2 pipeline=t2.a"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("warn %d = %q, want %q", i, lines[i], want)
		}
	}
}

// TestRunnerLogsCarryPipeline [#521]: a runner built with WithPipeline logs tenant and pipeline on every
// line: the first-commit info, a failed emit's warning and the degrade error. Without the option a line
// carries neither, as today.
func TestRunnerLogsCarryPipeline(t *testing.T) {
	buf := captureLogs(t)

	key := model.CheckpointKey{SourceInstance: "mt.t1.a", Loop: "analytics", OutputFingerprint: "fp"}
	em := &fakeEmitter{byTS: map[int64]error{
		120: errors.New("upstream 503 after retries"),
		180: &emit.RejectError{Reason: emit.ReasonUnknown, Status: 400, Msg: "bad"},
	}}
	r := NewLoopRunner(fakeLoop{key: key}, em, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{},
		WithPipeline(Pipeline{Tenant: "t1", ID: "t1.a"}))
	r.ProcessBatch(leaderCtx(), batchAt(key, 60))  // first commit: info
	r.ProcessBatch(leaderCtx(), batchAt(key, 120)) // retryable: warn
	r.ProcessBatch(leaderCtx(), batchAt(key, 180)) // terminal: degrade error

	for _, msg := range []string{
		`msg="loop committed first watermark advance (leader healthy)"`,
		`msg="emit failed after retries; will re-pull next tick"`,
		`msg="loop degraded (loud halt, scheduler will back off)"`,
	} {
		lines := linesWith(buf.String(), msg)
		if len(lines) != 1 {
			t.Fatalf("want one %s line, got %d:\n%s", msg, len(lines), buf.String())
		}
		if !strings.Contains(lines[0], "loop=analytics") || !strings.Contains(lines[0], "tenant=t1 pipeline=t1.a") {
			t.Errorf("%s line %q lacks loop=analytics tenant=t1 pipeline=t1.a", msg, lines[0])
		}
	}

	// Legacy: no option, no tenant or pipeline attribute.
	buf.Reset()
	legacy := NewLoopRunner(fakeLoop{key: key}, &fakeEmitter{byTS: map[int64]error{120: errors.New("503")}}, newMemCP(),
		source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
	legacy.ProcessBatch(leaderCtx(), batchAt(key, 120))
	if out := buf.String(); !strings.Contains(out, "emit failed after retries") || strings.Contains(out, "tenant=") || strings.Contains(out, "pipeline=") {
		t.Fatalf("a runner without WithPipeline must log neither tenant nor pipeline:\n%s", out)
	}
}

// linesWith returns the captured log lines containing sub, in order.
func linesWith(out, sub string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			lines = append(lines, l)
		}
	}
	return lines
}
