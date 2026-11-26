// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// --- doubles for the scope gate's scheduler hook (design §5.1, unit 4a) ---

// scriptedAsserter returns one fixed outcome and counts its calls.
type scriptedAsserter struct {
	mu    sync.Mutex
	res   source.ScopeResult
	err   error
	calls int
}

func (a *scriptedAsserter) AssertScope(context.Context) (source.ScopeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.res, a.err
}

func (a *scriptedAsserter) Calls() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

// gateMetrics records what the scheduler reports for a gated tick.
type gateMetrics struct {
	NoopMetrics
	mu       sync.Mutex
	lags     int
	skipped  map[string]int
	emitErrs map[string]int
	auth     []string
	degraded []string
}

func newGateMetrics() *gateMetrics {
	return &gateMetrics{skipped: map[string]int{}, emitErrs: map[string]int{}}
}

func (m *gateMetrics) WindowLag(LoopID, time.Duration) { m.mu.Lock(); m.lags++; m.mu.Unlock() }
func (m *gateMetrics) SamplesSkipped(_ LoopID, reason string, n int) {
	m.mu.Lock()
	m.skipped[reason] += n
	m.mu.Unlock()
}
func (m *gateMetrics) EmitError(_ LoopID, kind string) {
	m.mu.Lock()
	m.emitErrs[kind]++
	m.mu.Unlock()
}
func (m *gateMetrics) AuthError(loop, src string) {
	m.mu.Lock()
	m.auth = append(m.auth, loop+"/"+src)
	m.mu.Unlock()
}
func (m *gateMetrics) LoopDegraded(_ LoopID, reason string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on {
		m.degraded = append(m.degraded, reason)
	}
}

// countingLoop counts its collects and returns one bucket at the watermark it was given plus a minute.
type countingLoop struct {
	key     model.CheckpointKey
	mu      sync.Mutex
	n       int
	onEvent func(string)
	// holdUntilDone makes Collect return only after its context ends, and log that it returned.
	holdUntilDone bool
}

func (l *countingLoop) Key() model.CheckpointKey { return l.key }
func (l *countingLoop) Cadence() time.Duration   { return time.Minute }
func (l *countingLoop) Collect(ctx context.Context, since model.Watermark) (model.Batch, error) {
	l.mu.Lock()
	l.n++
	l.mu.Unlock()
	if l.onEvent != nil {
		l.onEvent("collect:" + l.key.Loop)
	}
	if l.holdUntilDone {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // a straggler still running when leadership ends
		l.onEvent("returned:" + l.key.Loop)
		return model.Batch{}, ctx.Err()
	}
	return batchAt(l.key, since.Time.Unix()+60), nil
}

func (l *countingLoop) Collects() int { l.mu.Lock(); defer l.mu.Unlock(); return l.n }

// TestScopeGateDecisionEffects: one tick of a windowed loop whose watermark is older than its lookback,
// for each thing the gate can say. Proceed collects as today; wait and mismatched end the tick before
// Since with no lag and no unreadable time counted, mismatched degrading the loop so it is next ticked at
// DegradedBackoff; a failed call does today's lag and backfill accounting and counts a collect error, and
// a 401 or 403 also raises the auth signal. All of it lands on the spec's own Metrics.
func TestScopeGateDecisionEffects(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	cases := []struct {
		name                 string
		res                  source.ScopeResult
		err                  error
		wantCollect, wantLag bool
		wantSkipped          bool
		wantCollectErr       int
		wantAuth             []string
		wantDegraded         []string
		wantAuthStreak       bool // the tick feeds the #537 auth backoff streak
	}{
		{name: "proceed", res: source.ScopeMatched, wantCollect: true, wantLag: true, wantSkipped: true},
		{name: "wait", res: source.ScopeUnverifiable},
		{name: "mismatched", res: source.ScopeMismatch, wantDegraded: []string{"scope_mismatch"}},
		{name: "failed call", err: errors.New("upstream 503"), wantLag: true, wantSkipped: true, wantCollectErr: 1},
		{name: "auth failure", err: fmt.Errorf("assert: %w", &source.AuthError{Status: 403}), wantLag: true, wantSkipped: true, wantCollectErr: 1,
			wantAuth: []string{"analytics/mt.t1.a"}, wantAuthStreak: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := model.CheckpointKey{SourceInstance: "mt.t1.a", Loop: "analytics", OutputFingerprint: "fp"}
			cp := newMemCP()
			cp.Save(context.Background(), key, model.Watermark{Time: time.Unix(200, 0).UTC(), Epoch: 1})
			loop := &countingLoop{key: key}
			specM, schedM := newGateMetrics(), newGateMetrics()
			r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, specM)
			a := &scriptedAsserter{res: tc.res, err: tc.err}
			gate := source.NewScopeGate(source.ScopeGateConfig{Asserter: a, Store: newMemCP(), SourceInstance: "mt.t1.a",
				Digest: source.ScopeMatchDigest("fp", "ws", "id"), Now: func() time.Time { return now }})
			sp := LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, Window: 50 * time.Minute, MaxBackfill: 10 * time.Minute,
				MaxCatchupPerTick: 1, Metrics: specM, Gate: gate, Pipeline: Pipeline{Tenant: "t1", ID: "t1.a"}}
			out := NewScheduler(nil, schedM).tick(leaderCtx(), sp, now)

			if a.Calls() != 1 {
				t.Fatalf("asserter calls = %d, want 1", a.Calls())
			}
			if got := loop.Collects() == 1; got != tc.wantCollect {
				t.Fatalf("collected = %v, want %v", got, tc.wantCollect)
			}
			if got := specM.lags == 1; got != tc.wantLag {
				t.Fatalf("window_lag recorded = %v (%d), want %v", got, specM.lags, tc.wantLag)
			}
			if got := specM.skipped["backfill_unstorable"] > 0; got != tc.wantSkipped {
				t.Fatalf("backfill_unstorable counted = %v (%v), want %v", got, specM.skipped, tc.wantSkipped)
			}
			if got := specM.emitErrs["collect"]; got != tc.wantCollectErr || len(specM.emitErrs) > min(got, 1) {
				t.Fatalf("emit errors = %v, want collect=%d and nothing else", specM.emitErrs, tc.wantCollectErr)
			}
			if !slices.Equal(specM.auth, tc.wantAuth) {
				t.Fatalf("auth errors = %v, want %v", specM.auth, tc.wantAuth)
			}
			if !slices.Equal(specM.degraded, tc.wantDegraded) || r.Degraded() != (tc.wantDegraded != nil) {
				t.Fatalf("degraded = %v (runner %v), want %v", specM.degraded, r.Degraded(), tc.wantDegraded)
			}
			if schedM.lags != 0 || len(schedM.emitErrs) != 0 || len(schedM.auth) != 0 || len(schedM.skipped) != 0 {
				t.Fatalf("the scheduler's own Metrics recorded %+v; a spec with Metrics records there", schedM)
			}
			var st tickState
			wait, _ := st.next(r.Degraded(), out, sp.MaxCatchupPerTick, sp.Cadence)
			if tc.wantDegraded != nil && wait != DegradedBackoff {
				t.Fatalf("mismatched loop next waits %v, want DegradedBackoff", wait)
			}
			if tc.wantDegraded == nil && wait == DegradedBackoff {
				t.Fatalf("%s: next wait is DegradedBackoff, want the cadence", tc.name)
			}
			// [#537] A 401/403 from the gate call stretches the wait like a collect's, so a bad key does
			// not spend the request budget at cadence; any other failure keeps the cadence.
			if out.auth != tc.wantAuthStreak {
				t.Fatalf("tickOutcome.auth = %v, want %v", out.auth, tc.wantAuthStreak)
			}
			if wantWait := map[bool]time.Duration{true: 2 * sp.Cadence, false: sp.Cadence}[tc.wantAuthStreak]; tc.wantDegraded == nil && wait != wantWait {
				t.Fatalf("next wait %v, want %v", wait, wantWait)
			}
		})
	}
}

// TestScopeGateCancelledCallNotCounted: a gate call cut short by leadership loss is the cancellation,
// not a failure: nothing is recorded or counted and nothing is collected.
func TestScopeGateCancelledCallNotCounted(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "mt.t1.a", Loop: "analytics", OutputFingerprint: "fp"}
	loop := &countingLoop{key: key}
	m := newGateMetrics()
	r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, newMemCP(), source.NewGuard(source.GuardConfig{}), 4, 1, m)
	gate := source.NewScopeGate(source.ScopeGateConfig{Asserter: &scriptedAsserter{err: context.Canceled}, Store: newMemCP(),
		SourceInstance: "mt.t1.a", Digest: "d"})
	ctx, cancel := context.WithCancel(leaderCtx())
	cancel()
	NewScheduler(nil, m).tick(ctx, LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, Gate: gate}, time.Unix(2000, 0))
	if loop.Collects() != 0 || m.lags != 0 || len(m.emitErrs) != 0 || len(m.auth) != 0 {
		t.Fatalf("cancelled gate call: collects=%d lags=%d emitErrs=%v auth=%v, want nothing", loop.Collects(), m.lags, m.emitErrs, m.auth)
	}
}

// eventStore is the gate's remembered-match store; it logs each Load through onEvent.
type eventStore struct {
	*memCP
	onEvent func(string)
}

func (s eventStore) Load(ctx context.Context, k model.CheckpointKey) (model.Watermark, error) {
	s.onEvent("load:" + k.String())
	return s.memCP.Load(ctx, k)
}

// TestRunElectsGatesBeforeFirstTickAndResetsAfterJoin: Run tells each distinct gate of its election
// once, before any loop ticks, so a remembered match lets the first tick of both loops collect with no
// assertion call; after the goroutines join (a collect still running when leadership ends included) it
// resets each gate, so a demoted replica reports no result.
func TestRunElectsGatesBeforeFirstTickAndResetsAfterJoin(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	snapshot := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(events) }

	digest := source.ScopeMatchDigest("fp", "ws", "id")
	store := eventStore{memCP: newMemCP(), onEvent: record}
	store.Save(context.Background(), source.ScopeMatchKey("mt.t1.a"), model.Watermark{Time: time.Unix(1, 0), Cursor: digest, Epoch: 1})
	a := &scriptedAsserter{res: source.ScopeMatched}
	gate := source.NewScopeGate(source.ScopeGateConfig{Asserter: a, Store: store, SourceInstance: "mt.t1.a", Digest: digest,
		Jitter:  func(d time.Duration) time.Duration { return d },
		OnState: func(s string) { record("state:" + s) }})

	var specs []LoopSpec
	var loops []*countingLoop
	for _, name := range []string{"analytics", "groups"} {
		key := model.CheckpointKey{SourceInstance: "mt.t1.a", Loop: name, OutputFingerprint: "fp"}
		cp := newMemCP()
		cp.Save(context.Background(), key, model.Watermark{Time: time.Now().Add(-time.Minute).Truncate(time.Second), Epoch: 1})
		loop := &countingLoop{key: key, onEvent: record, holdUntilDone: name == "groups"}
		loops = append(loops, loop)
		r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, NoopMetrics{})
		specs = append(specs, LoopSpec{Runner: r, Loop: loop, Cadence: time.Hour, Gate: gate})
	}
	ctx, cancel := context.WithCancel(leaderCtx())
	done := make(chan struct{})
	go func() { NewScheduler(specs, NoopMetrics{}).Run(ctx); close(done) }()
	deadline := time.After(10 * time.Second)
	for loops[0].Collects() == 0 || loops[1].Collects() == 0 {
		select {
		case <-deadline:
			t.Fatalf("both loops did not collect on their first tick; events %q", snapshot())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done

	got := snapshot()
	head := []string{"state:", "load:mt.t1.a/scope-match/v1", "state:matched"}
	if len(got) < 6 || !slices.Equal(got[:3], head) {
		t.Fatalf("events %q, want the election (%q) before any collect", got, head)
	}
	if n := slices.IndexFunc(got, func(e string) bool { return e == "load:mt.t1.a/scope-match/v1" }); slices.Index(got[n+1:], got[n]) != -1 {
		t.Fatalf("events %q: the shared gate was elected more than once", got)
	}
	if got[len(got)-2] != "returned:groups" || got[len(got)-1] != "state:" || slices.Contains(got[len(head):len(got)-1], "state:") {
		t.Fatalf("events %q, want one reset to no result, last, after the straggling collect returned", got)
	}
	if a.Calls() != 0 {
		t.Fatalf("asserter calls = %d, want 0: the restored match needs no call on the first ticks", a.Calls())
	}
}

// TestScopeGateAuthInsideGraceKeepsSignal [PR #548 review]: a matched pipeline whose assertion gets a
// 401/403 inside the grace still collects, and the scheduler raises auth_errors_total once for the gate's
// call. The collect's own 401 is the source's to count through its hook (one count per 401 response), so
// the scheduler records exactly one AuthError per tick here. The auth backoff follows the tick: it
// stretches when the collect fails too, and not when the collect succeeds.
func TestScopeGateAuthInsideGraceKeepsSignal(t *testing.T) {
	cases := []struct {
		name          string
		gateErr       error
		collectErr    error
		wantAuth      int
		wantCollected bool
		wantStreak    bool
		wantCollErrs  int
	}{
		{name: "auth, collect fails with auth", gateErr: &source.AuthError{Status: 401}, collectErr: source.WrapAuth(401, errors.New("analytics 401")),
			wantAuth: 1, wantStreak: true, wantCollErrs: 1},
		{name: "auth, collect fails otherwise", gateErr: &source.AuthError{Status: 403}, collectErr: errors.New("upstream 503"),
			wantAuth: 1, wantStreak: true, wantCollErrs: 1},
		{name: "auth, collect succeeds", gateErr: &source.AuthError{Status: 401}, wantAuth: 1, wantCollected: true},
		{name: "other failure, collect succeeds", gateErr: errors.New("upstream 503"), wantCollected: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := model.CheckpointKey{SourceInstance: "mt.t1.a", Loop: "analytics", OutputFingerprint: "fp"}
			cp := newMemCP()
			now := time.Unix(100000, 0).UTC()
			cp.Save(context.Background(), key, model.Watermark{Time: now.Add(-2 * time.Minute), Epoch: 1})
			loop := &errorOnceLoop{key: key, err: tc.collectErr}
			m := newGateMetrics()
			r := NewLoopRunner(loop, &fakeEmitter{byTS: map[int64]error{}}, cp, source.NewGuard(source.GuardConfig{}), 4, 1, m)
			a := &scriptedAsserter{res: source.ScopeMatched}
			clock := now
			gate := source.NewScopeGate(source.ScopeGateConfig{Asserter: a, Store: newMemCP(), SourceInstance: "mt.t1.a",
				Digest: source.ScopeMatchDigest("fp", "ws", "id"), Now: func() time.Time { return clock }})
			if d, err := gate.Before(leaderCtx()); d != source.ScopeProceed || err != nil {
				t.Fatalf("first assertion = (%v, %v), want a match", d, err)
			}
			clock = clock.Add(10 * time.Minute) // re-assertion due, well inside the 20-minute grace
			a.res, a.err = 0, tc.gateErr
			sp := LoopSpec{Runner: r, Loop: loop, Cadence: time.Minute, Window: time.Hour, MaxBackfill: time.Hour,
				MaxCatchupPerTick: 1, Metrics: m, Gate: gate}
			out := NewScheduler(nil, NoopMetrics{}).tick(leaderCtx(), sp, clock)

			if a.Calls() != 2 || loop.calls != 1 {
				t.Fatalf("asserter calls %d, collects %d, want 2 and 1 (the tick still collects inside the grace)", a.Calls(), loop.calls)
			}
			if len(m.auth) != tc.wantAuth {
				t.Fatalf("auth errors %v, want %d", m.auth, tc.wantAuth)
			}
			if out.collected != tc.wantCollected || out.auth != tc.wantStreak {
				t.Fatalf("outcome %+v, want collected=%v auth=%v", out, tc.wantCollected, tc.wantStreak)
			}
			if m.emitErrs["collect"] != tc.wantCollErrs {
				t.Fatalf("collect errors %v, want %d: the gate's call inside the grace is not a failed collect", m.emitErrs, tc.wantCollErrs)
			}
		})
	}
}

// errorOnceLoop returns err from Collect when set, else one bucket after the watermark.
type errorOnceLoop struct {
	key   model.CheckpointKey
	err   error
	calls int
}

func (l *errorOnceLoop) Key() model.CheckpointKey { return l.key }
func (l *errorOnceLoop) Cadence() time.Duration   { return time.Minute }
func (l *errorOnceLoop) Collect(_ context.Context, since model.Watermark) (model.Batch, error) {
	l.calls++
	if l.err != nil {
		return model.Batch{}, l.err
	}
	return batchAt(l.key, since.Time.Unix()+60), nil
}
