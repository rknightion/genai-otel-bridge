// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// --- fakes ---

type gateClock struct {
	mu sync.Mutex
	t  time.Time
}

func newGateClock() *gateClock { return &gateClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)} }

func (c *gateClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *gateClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeAsserter returns its queued outcomes in order, then repeats the last one.
type fakeAsserter struct {
	mu    sync.Mutex
	calls int
	out   []assertOutcome
	last  *assertOutcome // repeated once the queue is empty
}

type assertOutcome struct {
	res ScopeResult
	err error
}

func (a *fakeAsserter) next(res ScopeResult, err error) {
	a.mu.Lock()
	a.out = append(a.out, assertOutcome{res, err})
	a.mu.Unlock()
}

func (a *fakeAsserter) AssertScope(context.Context) (ScopeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if len(a.out) == 0 {
		if a.last != nil {
			return a.last.res, a.last.err
		}
		return 0, errors.New("fakeAsserter: no outcome queued")
	}
	o := a.out[0]
	if len(a.out) > 1 {
		a.out = a.out[1:]
	} else {
		a.out = a.out[:0]
		a.last = &o
	}
	return o.res, o.err
}

func (a *fakeAsserter) Calls() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

// fakeStore is a one-key checkpoint store. With fence set it refuses a write as the real backends do,
// through checkpoint.CheckMonotonic. failSaves makes the next n saves fail.
type fakeStore struct {
	mu        sync.Mutex
	has       bool
	wm        model.Watermark
	fence     bool
	loadErr   error
	failSaves int
	saves     []model.Watermark // every accepted write
	attempts  int
}

func (s *fakeStore) Load(_ context.Context, key model.CheckpointKey) (model.Watermark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != ScopeMatchKey("mt.t1.a") {
		return model.Watermark{}, errors.New("fakeStore: unexpected key " + key.String())
	}
	if s.loadErr != nil {
		return model.Watermark{}, s.loadErr
	}
	return s.wm, nil
}

func (s *fakeStore) Save(_ context.Context, key model.CheckpointKey, w model.Watermark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if key != ScopeMatchKey("mt.t1.a") {
		return errors.New("fakeStore: unexpected key " + key.String())
	}
	if s.failSaves > 0 {
		s.failSaves--
		return errors.New("fakeStore: injected save failure")
	}
	if s.fence && s.has {
		if err := checkpoint.CheckMonotonic(s.wm, w); err != nil {
			return err
		}
	}
	s.has, s.wm = true, w
	s.saves = append(s.saves, w)
	return nil
}

func (s *fakeStore) Saves() []model.Watermark {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.saves)
}

const testDigestFP = "fp"

var testDigest = ScopeMatchDigest(testDigestFP, "ws-a", "id-1")

type gateRig struct {
	g        *ScopeGate
	clock    *gateClock
	asserter *fakeAsserter
	store    *fakeStore
	states   []string
	results  []string
	jitterIn []time.Duration
}

func newRig(t *testing.T, store *fakeStore, digest string) *gateRig {
	t.Helper()
	r := &gateRig{clock: newGateClock(), asserter: &fakeAsserter{}, store: store}
	if r.store == nil {
		r.store = &fakeStore{}
	}
	if digest == "" {
		digest = testDigest
	}
	r.g = NewScopeGate(ScopeGateConfig{
		Asserter: r.asserter, Store: r.store, SourceInstance: "mt.t1.a", Digest: digest, Now: r.clock.Now,
		Jitter:   func(d time.Duration) time.Duration { r.jitterIn = append(r.jitterIn, d); return 3 * time.Minute },
		OnResult: func(s string) { r.results = append(r.results, s) },
		OnState:  func(s string) { r.states = append(r.states, s) },
	})
	return r
}

// before asks the gate once and checks the decision, the error and how many calls the asserter has seen.
func (r *gateRig) before(t *testing.T, ctx context.Context, want ScopeDecision, wantErr bool, wantCalls int) error {
	t.Helper()
	got, err := r.g.Before(ctx)
	if got != want || (err != nil) != wantErr {
		t.Fatalf("Before = (%v, %v), want (%v, err=%v)", got, err, want, wantErr)
	}
	if c := r.asserter.Calls(); c != wantCalls {
		t.Fatalf("asserter calls = %d, want %d", c, wantCalls)
	}
	return err
}

var errUpstream = errors.New("upstream 503")

// --- §5.1 policy, one case per rule ---

// Rule: no result calls the assertion; Matched makes the state matched and the tick goes on.
func TestGateNoResultCallsAndMatchedProceeds(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMatched, nil)
	r.before(t, context.Background(), ScopeProceed, false, 1)
	if !slices.Equal(r.states, []string{"matched"}) || !slices.Equal(r.results, []string{"matched"}) {
		t.Fatalf("states %q results %q, want [matched] [matched]", r.states, r.results)
	}
}

// Rule: matched and the re-assertion not yet due: no call; due one interval after a fresh match.
func TestGateReassertsOneIntervalAfterMatch(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMatched, nil)
	r.before(t, context.Background(), ScopeProceed, false, 1)
	r.clock.Advance(10*time.Minute - time.Second)
	r.before(t, context.Background(), ScopeProceed, false, 1)
	r.clock.Advance(time.Second)
	r.before(t, context.Background(), ScopeProceed, false, 2)
}

// Rule: unverified ends the tick and asserts again at most once a minute.
func TestGateUnverifiedWaitsAndRetriesOncePerMinute(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeUnverifiable, nil)
	r.before(t, context.Background(), ScopeWait, false, 1)
	r.clock.Advance(59 * time.Second)
	r.before(t, context.Background(), ScopeWait, false, 1)
	r.clock.Advance(time.Second)
	r.before(t, context.Background(), ScopeWait, false, 2)
	if !slices.Equal(r.states, []string{"unverified"}) {
		t.Fatalf("states %q, want [unverified]", r.states)
	}
}

// Rule: mismatched ends the tick (ScopeMismatched, every tick, so each loop degrades) and asserts again
// at most once a minute.
func TestGateMismatchedRetriesOncePerMinute(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMismatch, nil)
	r.before(t, context.Background(), ScopeMismatched, false, 1)
	r.clock.Advance(59 * time.Second)
	r.before(t, context.Background(), ScopeMismatched, false, 1)
	r.clock.Advance(time.Second)
	r.before(t, context.Background(), ScopeMismatched, false, 2)
	if !slices.Equal(r.states, []string{"mismatched"}) {
		t.Fatalf("states %q, want [mismatched]", r.states)
	}
}

// Rule: Unverifiable keeps a match, and the next re-assertion is one interval later.
func TestGateUnverifiableKeepsMatch(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMatched, nil)
	r.asserter.next(ScopeUnverifiable, nil)
	r.before(t, context.Background(), ScopeProceed, false, 1)
	r.clock.Advance(10 * time.Minute)
	r.before(t, context.Background(), ScopeProceed, false, 2)
	r.clock.Advance(10*time.Minute - time.Second)
	r.before(t, context.Background(), ScopeProceed, false, 2)
	if !slices.Equal(r.states, []string{"matched"}) || !slices.Equal(r.results, []string{"matched", "unverifiable"}) {
		t.Fatalf("states %q results %q", r.states, r.results)
	}
}

// Rule: a failed call keeps a match for two intervals after the last successful assertion, retrying at
// most once a minute; past that it is handled as a failure, and the ticks between its calls wait.
func TestGateFailedCallKeepsMatchForGrace(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMatched, nil)
	r.asserter.next(0, errUpstream)
	ctx := context.Background()
	r.before(t, ctx, ScopeProceed, false, 1) // t0: last success
	r.clock.Advance(10 * time.Minute)
	r.before(t, ctx, ScopeProceed, false, 2) // t0+10m: fails inside grace, still collects
	r.clock.Advance(30 * time.Second)
	r.before(t, ctx, ScopeProceed, false, 2) // no call inside the minute
	r.clock.Advance(30 * time.Second)
	r.before(t, ctx, ScopeProceed, false, 3) // t0+11m: due again, fails, still inside grace
	r.clock.Advance(9 * time.Minute)
	r.before(t, ctx, ScopeProceed, false, 4) // t0+20m: the edge of the grace still collects
	r.clock.Advance(time.Minute)
	err := r.before(t, ctx, ScopeWait, true, 5) // t0+21m: past the grace, a failure
	if !errors.Is(err, errUpstream) {
		t.Fatalf("error %v, want the asserter's", err)
	}
	r.clock.Advance(30 * time.Second)
	r.before(t, ctx, ScopeWait, false, 5) // between calls past the grace: no collect, no call
	if !slices.Equal(r.states, []string{"matched"}) {
		t.Fatalf("states %q, want [matched] (a failed call does not change the state)", r.states)
	}
	if n := len(r.results); n != 5 || r.results[4] != "error" || r.results[1] != "error" {
		t.Fatalf("results %q, want matched then four errors", r.results)
	}
	// A success ends the failure: the pipeline collects again with a fresh interval.
	r.asserter.next(ScopeMatched, nil)
	r.clock.Advance(30 * time.Second)
	r.before(t, ctx, ScopeProceed, false, 6)
}

// Rule: the grace of a restored match runs from Elected.
func TestGateFailedCallGraceFromRestoredMatch(t *testing.T) {
	r := newRig(t, &fakeStore{has: true, wm: model.Watermark{Time: time.Unix(1, 0), Cursor: testDigest}}, "")
	r.asserter.next(0, errUpstream)
	ctx := context.Background()
	r.g.Elected(ctx)
	r.clock.Advance(20 * time.Minute)
	r.before(t, ctx, ScopeProceed, false, 1)
	r.clock.Advance(time.Minute)
	r.before(t, ctx, ScopeWait, true, 2)
}

// Rule: a failed call otherwise does not change the state and ends the tick with the error; the next
// call is at most a minute later and the ticks between wait without an error.
func TestGateFailedCallWithoutMatch(t *testing.T) {
	for _, start := range []ScopeResult{0, ScopeUnverifiable, ScopeMismatch} {
		r := newRig(t, nil, "")
		ctx := context.Background()
		wantStates := []string(nil)
		calls := 0
		between := ScopeWait
		if start != 0 {
			r.asserter.next(start, nil)
			calls++
			want := ScopeWait
			if start == ScopeMismatch {
				want, between = ScopeMismatched, ScopeMismatched
				wantStates = []string{"mismatched"}
			} else {
				wantStates = []string{"unverified"}
			}
			r.before(t, ctx, want, false, calls)
			r.clock.Advance(time.Minute)
		}
		r.asserter.next(0, &AuthError{Status: 401})
		calls++
		err := r.before(t, ctx, ScopeWait, true, calls)
		var ae *AuthError
		if !errors.As(err, &ae) || ae.Status != 401 {
			t.Fatalf("start %d: error %v, want *AuthError{401}", start, err)
		}
		r.clock.Advance(59 * time.Second)
		r.before(t, ctx, between, false, calls)
		r.clock.Advance(time.Second)
		calls++
		r.before(t, ctx, ScopeWait, true, calls)
		if !slices.Equal(r.states, wantStates) {
			t.Fatalf("start %d: states %q, want %q", start, r.states, wantStates)
		}
		if r.results[len(r.results)-1] != "error" {
			t.Fatalf("start %d: results %q, want a trailing error", start, r.results)
		}
	}
}

// Rule: a real Mismatch ends the grace at once.
func TestGateMismatchEndsGrace(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMatched, nil)
	r.asserter.next(0, errUpstream)
	r.asserter.next(ScopeMismatch, nil)
	ctx := context.Background()
	r.before(t, ctx, ScopeProceed, false, 1)
	r.clock.Advance(10 * time.Minute)
	r.before(t, ctx, ScopeProceed, false, 2)
	r.clock.Advance(time.Minute)
	r.before(t, ctx, ScopeMismatched, false, 3)
}

// Rule: only Matched ends a mismatch; Unverifiable does not.
func TestGateOnlyMatchedEndsMismatch(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMismatch, nil)
	r.asserter.next(ScopeUnverifiable, nil)
	r.asserter.next(ScopeMatched, nil)
	ctx := context.Background()
	r.before(t, ctx, ScopeMismatched, false, 1)
	r.clock.Advance(time.Minute)
	r.before(t, ctx, ScopeMismatched, false, 2)
	r.clock.Advance(time.Minute)
	r.before(t, ctx, ScopeProceed, false, 3)
	if !slices.Equal(r.states, []string{"mismatched", "matched"}) {
		t.Fatalf("states %q", r.states)
	}
}

// Rule: Elected restores a remembered match for the current digest, with the first re-assertion at
// Jitter(interval).
func TestGateElectedRestoresMatch(t *testing.T) {
	r := newRig(t, &fakeStore{has: true, wm: model.Watermark{Time: time.Unix(1, 0), Cursor: testDigest}}, "")
	r.asserter.next(ScopeMatched, nil)
	ctx := context.Background()
	r.g.Elected(ctx)
	if !slices.Equal(r.jitterIn, []time.Duration{10 * time.Minute}) {
		t.Fatalf("Jitter called with %v, want [10m]", r.jitterIn)
	}
	if !slices.Equal(r.states, []string{"", "matched"}) {
		t.Fatalf("states %q, want reset then matched", r.states)
	}
	r.before(t, ctx, ScopeProceed, false, 0)
	r.clock.Advance(3*time.Minute - time.Second)
	r.before(t, ctx, ScopeProceed, false, 0)
	r.clock.Advance(time.Second)
	r.before(t, ctx, ScopeProceed, false, 1)
	if n := len(r.store.Saves()); n != 0 {
		t.Fatalf("a Matched equal to the stored digest wrote %d times, want 0", n)
	}
}

// Rule: an empty or other stored digest, or a read error, starts with no result.
func TestGateElectedStartsWithNoResult(t *testing.T) {
	for name, st := range map[string]*fakeStore{
		"absent":     {},
		"empty":      {has: true, wm: model.Watermark{Time: time.Unix(1, 0)}},
		"other":      {has: true, wm: model.Watermark{Time: time.Unix(1, 0), Cursor: ScopeMatchDigest("fp", "ws-b", "id-1")}},
		"read error": {loadErr: errors.New("configmap unreadable"), wm: model.Watermark{Cursor: testDigest}},
	} {
		r := newRig(t, st, "")
		r.asserter.next(ScopeUnverifiable, nil)
		r.g.Elected(context.Background())
		if !slices.Equal(r.states, []string{""}) {
			t.Fatalf("%s: states %q, want only the reset", name, r.states)
		}
		r.before(t, context.Background(), ScopeWait, false, 1)
	}
}

// Rule: Reset sets no result and reports it, so the next leadership asserts afresh.
func TestGateResetReportsNoResult(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeMatched, nil)
	ctx := context.Background()
	r.before(t, ctx, ScopeProceed, false, 1)
	r.g.Reset()
	if !slices.Equal(r.states, []string{"matched", ""}) {
		t.Fatalf("states %q, want matched then no result", r.states)
	}
	r.before(t, ctx, ScopeProceed, false, 2)
}

// Rule: a call cut short by the leader context's cancellation is not a failed call: nothing is counted
// and nothing changes.
func TestGateCancelledCallNotCounted(t *testing.T) {
	r := newRig(t, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.asserter.next(0, context.Canceled)
	if d, err := r.g.Before(ctx); d != ScopeWait || err == nil {
		t.Fatalf("Before = (%v, %v), want (ScopeWait, error)", d, err)
	}
	if len(r.results) != 0 || len(r.states) != 0 {
		t.Fatalf("a cancelled call reported results %q states %q", r.results, r.states)
	}
	r.asserter.next(ScopeMatched, nil)
	r.before(t, context.Background(), ScopeProceed, false, 2) // no one-minute wait after a cancellation
}

// An asserter returning a value outside the three results fails closed, as a failed call.
func TestGateUnknownResultIsFailedCall(t *testing.T) {
	r := newRig(t, nil, "")
	r.asserter.next(ScopeResult(42), nil)
	r.before(t, context.Background(), ScopeWait, true, 1)
	if !slices.Equal(r.results, []string{"error"}) {
		t.Fatalf("results %q, want [error]", r.results)
	}
}

// --- §7 remembered match ---

// Writes: a match when the stored digest differs (once, not every interval), empty on every Mismatch
// whose stored digest is not already empty, fenced by the leader epoch on the context.
func TestGateWritesRememberedMatch(t *testing.T) {
	st := &fakeStore{has: true, wm: model.Watermark{Time: time.Unix(1, 0), Epoch: 1}, fence: true}
	r := newRig(t, st, "")
	ctx := coordinate.WithEpoch(context.Background(), 4)
	r.g.Elected(ctx)
	r.asserter.next(ScopeMatched, nil)
	r.before(t, ctx, ScopeProceed, false, 1)
	t0 := r.clock.Now()
	r.clock.Advance(10 * time.Minute)
	r.before(t, ctx, ScopeProceed, false, 2) // Matched again: stored digest is current, no write
	r.asserter.next(ScopeMismatch, nil)
	r.clock.Advance(10 * time.Minute)
	r.before(t, ctx, ScopeMismatched, false, 3)
	t1 := r.clock.Now()
	r.clock.Advance(time.Minute)
	r.before(t, ctx, ScopeMismatched, false, 4) // stored digest already empty, no write
	want := []model.Watermark{{Time: t0, Cursor: testDigest, Epoch: 4}, {Time: t1, Cursor: "", Epoch: 4}}
	if got := st.Saves(); !slices.Equal(got, want) {
		t.Fatalf("saves %+v, want %+v", got, want)
	}
}

// AC2: a remembered match is reused only for the same key fingerprint, scope name and scope id.
func TestGateRemembersMatchPerDigest(t *testing.T) {
	stored := ScopeMatchDigest("fp-1", "ws-a", "id-1")
	same := newRig(t, &fakeStore{has: true, wm: model.Watermark{Time: time.Unix(1, 0), Cursor: stored}}, stored)
	same.g.Elected(context.Background())
	if !slices.Equal(same.states, []string{"", "matched"}) {
		t.Fatalf("same digest: states %q, want a restored match", same.states)
	}
	for name, d := range map[string]string{
		"key fingerprint": ScopeMatchDigest("fp-2", "ws-a", "id-1"),
		"scope name":      ScopeMatchDigest("fp-1", "ws-b", "id-1"),
		"scope id":        ScopeMatchDigest("fp-1", "ws-a", "id-2"),
		"shifted join":    ScopeMatchDigest("fp-1ws-a", "", "id-1"),
	} {
		if d == stored {
			t.Fatalf("%s: digest collides with the stored one", name)
		}
		r := newRig(t, &fakeStore{has: true, wm: model.Watermark{Time: time.Unix(1, 0), Cursor: stored}}, d)
		r.asserter.next(ScopeUnverifiable, nil)
		r.g.Elected(context.Background())
		if !slices.Equal(r.states, []string{""}) {
			t.Fatalf("%s: states %q, want no result", name, r.states)
		}
		r.before(t, context.Background(), ScopeWait, false, 1) // asserts afresh; never starts matched
	}
}

// AC3: a store whose record is an hour ahead of this leader's clock, fenced by CheckMonotonic, accepts
// both the match write and the clearing write.
func TestGateWriteNotRefusedWhenClockBehind(t *testing.T) {
	clockNow := newGateClock().Now()
	ahead := clockNow.Add(time.Hour)
	st := &fakeStore{has: true, fence: true, wm: model.Watermark{Time: ahead, Cursor: ScopeMatchDigest("old", "ws-a", "id-1"), Epoch: 3}}
	r := newRig(t, st, "")
	ctx := coordinate.WithEpoch(context.Background(), 3)
	r.g.Elected(ctx)
	r.asserter.next(ScopeMatched, nil)
	r.before(t, ctx, ScopeProceed, false, 1)
	r.asserter.next(ScopeMismatch, nil)
	r.clock.Advance(10 * time.Minute)
	r.before(t, ctx, ScopeMismatched, false, 2)
	want := []model.Watermark{
		{Time: ahead.Add(time.Second), Cursor: testDigest, Epoch: 3},
		{Time: ahead.Add(2 * time.Second), Cursor: "", Epoch: 3},
	}
	if got := st.Saves(); !slices.Equal(got, want) {
		t.Fatalf("saves %+v, want %+v (attempts %d)", got, want, st.attempts)
	}
}

// AC3: a refused clearing write is written at the next Mismatch; the gate's view moves only on success.
func TestGateRetriesClearOnMismatch(t *testing.T) {
	st := &fakeStore{has: true, fence: true, wm: model.Watermark{Time: time.Unix(1, 0), Cursor: testDigest}, failSaves: 1}
	r := newRig(t, st, "")
	ctx := context.Background()
	r.g.Elected(ctx)
	r.asserter.next(ScopeMismatch, nil)
	r.clock.Advance(3 * time.Minute) // the restored match's first re-assertion
	r.before(t, ctx, ScopeMismatched, false, 1)
	if n := len(st.Saves()); n != 0 || st.attempts != 1 {
		t.Fatalf("first clearing write: %d accepted of %d attempts, want 0 of 1", n, st.attempts)
	}
	r.clock.Advance(time.Minute)
	r.before(t, ctx, ScopeMismatched, false, 2)
	got := st.Saves()
	if len(got) != 1 || got[0].Cursor != "" || !got[0].Time.Equal(r.clock.Now()) {
		t.Fatalf("saves %+v, want one clearing write at the second Mismatch", got)
	}
	r.clock.Advance(time.Minute)
	r.before(t, ctx, ScopeMismatched, false, 3)
	if st.attempts != 2 {
		t.Fatalf("attempts %d, want 2: a cleared store is not cleared again", st.attempts)
	}
}

// A refused match write keeps the in-memory match and is tried again at the next Matched.
func TestGateRetriesMatchWrite(t *testing.T) {
	st := &fakeStore{failSaves: 1}
	r := newRig(t, st, "")
	ctx := context.Background()
	r.g.Elected(ctx)
	r.asserter.next(ScopeMatched, nil)
	r.before(t, ctx, ScopeProceed, false, 1)
	r.clock.Advance(10 * time.Minute)
	r.before(t, ctx, ScopeProceed, false, 2)
	if got := st.Saves(); len(got) != 1 || got[0].Cursor != testDigest {
		t.Fatalf("saves %+v, want the match written at the second Matched", got)
	}
}

// AC4: one assertion at a time. Every concurrent caller of a pipeline's gate waits for the first call's
// result and decides on it, so a burst of ticks makes one call.
func TestGateOneCallAtATime(t *testing.T) {
	release := make(chan struct{})
	var inflight, peak, calls atomic.Int32
	a := asserterFunc(func(context.Context) (ScopeResult, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		calls.Add(1)
		<-release
		inflight.Add(-1)
		return ScopeMatched, nil
	})
	clock := newGateClock()
	g := NewScopeGate(ScopeGateConfig{Asserter: a, Store: &fakeStore{}, SourceInstance: "mt.t1.a", Digest: testDigest, Now: clock.Now})
	const n = 16
	var wg sync.WaitGroup
	decisions := make([]ScopeDecision, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisions[i], _ = g.Before(context.Background())
		}()
	}
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // give the other callers time to pile up behind the first
	close(release)
	wg.Wait()
	if peak.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("peak concurrent calls %d, total calls %d, want 1 and 1", peak.Load(), calls.Load())
	}
	for i, d := range decisions {
		if d != ScopeProceed {
			t.Fatalf("caller %d decided %v, want ScopeProceed from the first call's Matched", i, d)
		}
	}
}

type asserterFunc func(context.Context) (ScopeResult, error)

func (f asserterFunc) AssertScope(ctx context.Context) (ScopeResult, error) { return f(ctx) }

// --- names and digests ---

// The digests are pinned to values computed outside Go (printf ... | sha256sum), so a change to the
// join or the encoding fails here.
func TestScopeMatchNames(t *testing.T) {
	if got := KeyFingerprint("Bearer k-1"); got != "b29d335247338de45cc88f161190c091e5622e43d6894ddfc4b99b2395f6301c" {
		t.Fatalf("KeyFingerprint = %s", got)
	}
	if got := ScopeMatchDigest("fp", "ws-a", "id-1"); got != "93c95485278d16692fd316df63bdc3b8ce40fb82e7950ab92ff6e341732d8e6a" {
		t.Fatalf("ScopeMatchDigest = %s", got)
	}
	if got := ScopeMatchKey("mt.t1.a").String(); got != "mt.t1.a/scope-match/v1" {
		t.Fatalf("ScopeMatchKey = %s", got)
	}
	if got := (&AuthError{Status: 403}).Error(); got != "source: scope assertion: upstream responded 403" {
		t.Fatalf("AuthError = %q", got)
	}
}

// TestAuthErrorIsErrAuth [#537]: a scope assertion's *AuthError is a source.ErrAuth, bare or wrapped, so
// the scheduler's one auth check covers a gate call as it covers a collect; other errors are not.
func TestAuthErrorIsErrAuth(t *testing.T) {
	for _, err := range []error{&AuthError{Status: 401}, fmt.Errorf("assert: %w", &AuthError{Status: 403})} {
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("errors.Is(%v, ErrAuth) = false, want true", err)
		}
		if errors.Is(err, ErrQuotaExceeded) {
			t.Fatalf("errors.Is(%v, ErrQuotaExceeded) = true, want false", err)
		}
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("errors.As(%v, *AuthError) = false", err)
		}
	}
	if errors.Is(errUpstream, ErrAuth) {
		t.Fatal("a non-auth failure is not ErrAuth")
	}
}
