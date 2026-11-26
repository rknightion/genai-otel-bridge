// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// ScopeResult is what a scope assertion reports about the data an API key reads (design §5.1).
type ScopeResult int

const (
	// ScopeMatched: the assertion request, sent as the source's loops send theirs, returned only the
	// configured scope, from traffic in the source's assertion window.
	ScopeMatched ScopeResult = iota + 1
	// ScopeMismatch: another scope, more than one, or a row with no scope.
	ScopeMismatch
	// ScopeUnverifiable: no scope had traffic in the assertion window.
	ScopeUnverifiable
)

// ScopeAsserter is implemented by a Source that can report what its API key reads. It is an OPTIONAL
// capability, not part of the frozen Source. An error means the call failed; it is never a result.
type ScopeAsserter interface {
	AssertScope(ctx context.Context) (ScopeResult, error)
}

// AuthError is what an asserter returns when the upstream answers its assertion with 401 or 403. The
// scheduler raises the auth-error signal for it; the asserter does not call Deps.OnAuthError itself.
type AuthError struct {
	Status int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("source: scope assertion: upstream responded %d", e.Status)
}

// ScopeMatchStore is where a gate remembers its pipeline's match (design §7). checkpoint.Checkpointer
// satisfies it, so a remembered match is an ordinary, epoch-fenced checkpoint record.
type ScopeMatchStore interface {
	Load(ctx context.Context, key model.CheckpointKey) (model.Watermark, error)
	Save(ctx context.Context, key model.CheckpointKey, w model.Watermark) error
}

// ScopeMatchLoop and ScopeMatchFingerprint are the reserved loop name and fingerprint field of a
// pipeline's remembered-match checkpoint key. No vendor loop may use the loop name.
const (
	ScopeMatchLoop        = "scope-match"
	ScopeMatchFingerprint = "v1"
)

// KeyFingerprint is the lower-case hex SHA-256 of the exact auth header value a source sends. It is
// never logged or emitted; it only feeds ScopeMatchDigest.
func KeyFingerprint(headerValue string) string {
	sum := sha256.Sum256([]byte(headerValue))
	return hex.EncodeToString(sum[:])
}

// ScopeMatchDigest is the lower-case hex SHA-256 of the key fingerprint, scope name and scope id joined
// with a NUL byte (design §7). The load rules refuse a control character in a scope name or id, so the
// join is unambiguous. A remembered match is reused only for the same three.
func ScopeMatchDigest(keyFingerprint, scopeName, scopeID string) string {
	sum := sha256.Sum256([]byte(keyFingerprint + "\x00" + scopeName + "\x00" + scopeID))
	return hex.EncodeToString(sum[:])
}

// ScopeMatchKey is a pipeline's remembered-match checkpoint key: <source instance>/scope-match/v1.
func ScopeMatchKey(sourceInstance string) model.CheckpointKey {
	return model.CheckpointKey{SourceInstance: sourceInstance, Loop: ScopeMatchLoop, OutputFingerprint: ScopeMatchFingerprint}
}

// ScopeDecision is what the gate tells the scheduler to do with one tick.
type ScopeDecision int

const (
	// ScopeProceed: collect as usual.
	ScopeProceed ScopeDecision = iota + 1
	// ScopeWait: end the tick before the collect, with no lag recorded and nothing counted (no result
	// yet, or unverified).
	ScopeWait
	// ScopeMismatched: end the tick before the collect and degrade the loop.
	ScopeMismatched
)

// The gate's constants (design §5.1). None is a config key.
const (
	// scopeReassertInterval is how long a fresh match lasts before the next assertion is due.
	scopeReassertInterval = 10 * time.Minute
	// scopeRetryInterval is the least time between two calls while unverified, mismatched or after a
	// failed call.
	scopeRetryInterval = time.Minute
	// scopeFailGrace is how long after the last successful assertion (or a restored match) a failed
	// call still lets a matched pipeline collect.
	scopeFailGrace = 2 * scopeReassertInterval
)

// Gate state names, as OnState reports them. The empty string is no result.
const (
	scopeStateNone       = ""
	scopeStateMatched    = "matched"
	scopeStateUnverified = "unverified"
	scopeStateMismatched = "mismatched"
)

// Assertion result names, as OnResult reports them.
const (
	scopeResultMatched      = "matched"
	scopeResultMismatch     = "mismatch"
	scopeResultUnverifiable = "unverifiable"
	scopeResultError        = "error"
)

// ScopeGateConfig builds a ScopeGate. Asserter and Store are required.
type ScopeGateConfig struct {
	Asserter ScopeAsserter
	Store    ScopeMatchStore
	// SourceInstance names the pipeline's remembered-match key (ScopeMatchKey).
	SourceInstance string
	// Digest is ScopeMatchDigest of the current key fingerprint, scope name and scope id.
	Digest string
	// Now is the gate's clock; nil means time.Now.
	Now func() time.Time
	// Jitter returns the delay of the first re-assertion after a restored match, inside [0, d); nil
	// means uniform random.
	Jitter func(d time.Duration) time.Duration
	// OnResult, if set, is told each call's result: matched, mismatch, unverifiable or error.
	OnResult func(result string)
	// OnState, if set, is told the gate's state when it changes: matched, unverified, mismatched, or
	// "" for no result.
	OnState func(state string)
}

// ScopeGate is the per-pipeline scope policy of design §5.1, asked by the scheduler before every
// collect of every loop of its pipeline. It holds one state in memory (no result, matched, unverified
// or mismatched) and the remembered match of design §7. It makes one assertion call at a time: the
// mutex is held across the call, so a second caller waits for the first's result and then decides on
// it rather than calling again.
type ScopeGate struct {
	cfg ScopeGateConfig

	mu    sync.Mutex
	state string
	// lastCall is when the gate last called the assertion (zero: never this leadership).
	lastCall time.Time
	// nextDue is when a matched pipeline's next assertion is due.
	nextDue time.Time
	// lastOK is the base of the failed-call grace: the last call that returned a result while the
	// pipeline ended matched, or the moment Elected restored a remembered match.
	lastOK time.Time
	// failed is set when the last call failed, cleared by any result.
	failed bool
	// The gate's view of the stored remembered match. viewKnown is false until a Load or a Save
	// succeeds, so a write is never skipped on a guess about what the store holds.
	viewKnown    bool
	storedTime   time.Time
	storedCursor string
}

// NewScopeGate returns a gate with no result. It panics on a nil Asserter or Store: a gate is built at
// assembly, and a gate that cannot assert or remember is a wiring bug.
func NewScopeGate(cfg ScopeGateConfig) *ScopeGate {
	if cfg.Asserter == nil {
		panic("source: NewScopeGate: nil Asserter")
	}
	if cfg.Store == nil {
		panic("source: NewScopeGate: nil Store")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Jitter == nil {
		cfg.Jitter = func(d time.Duration) time.Duration {
			if d <= 0 {
				return 0
			}
			return rand.N(d)
		}
	}
	return &ScopeGate{cfg: cfg}
}

// Elected resets the gate and reads the pipeline's remembered match. A non-empty stored digest equal to
// the configured one starts the gate matched, with the first re-assertion at Jitter(interval) and the
// failed-call grace counted from now. Anything else, or a read error, starts it with no result.
func (g *ScopeGate) Elected(ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resetLocked()
	wm, err := g.cfg.Store.Load(ctx, ScopeMatchKey(g.cfg.SourceInstance))
	if err != nil {
		slog.Warn("scope gate: remembered match unreadable; starting with no result",
			"source", g.cfg.SourceInstance, "err", err)
		return
	}
	g.viewKnown, g.storedTime, g.storedCursor = true, wm.Time, wm.Cursor
	if wm.Cursor == "" || wm.Cursor != g.cfg.Digest {
		return
	}
	now := g.cfg.Now()
	g.lastOK = now
	g.nextDue = now.Add(g.cfg.Jitter(scopeReassertInterval))
	g.setStateLocked(scopeStateMatched)
}

// Reset sets no result and forgets the stored view, reporting the empty state through OnState. The
// scheduler calls it when leadership ends, so a replica that is not leading reports no result.
func (g *ScopeGate) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resetLocked()
}

func (g *ScopeGate) resetLocked() {
	g.lastCall, g.nextDue, g.lastOK, g.failed = time.Time{}, time.Time{}, time.Time{}, false
	g.viewKnown, g.storedTime, g.storedCursor = false, time.Time{}, ""
	g.state = scopeStateNone
	if g.cfg.OnState != nil {
		g.cfg.OnState(scopeStateNone)
	}
}

func (g *ScopeGate) setStateLocked(s string) {
	if s == g.state {
		return
	}
	g.state = s
	if g.cfg.OnState != nil {
		g.cfg.OnState(s)
	}
}

func (g *ScopeGate) result(r string) {
	if g.cfg.OnResult != nil {
		g.cfg.OnResult(r)
	}
}

// Before decides one tick, calling the assertion when the policy says it is due. A non-nil error is a
// failed call that ends the tick: the scheduler records lag and counts it as a failed collect. The
// decision returned with an error is ScopeWait. An error while ctx is cancelled is the cancellation:
// it changes nothing and is not counted.
func (g *ScopeGate) Before(ctx context.Context) (ScopeDecision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.cfg.Now()
	retryWait := !g.lastCall.IsZero() && now.Sub(g.lastCall) < scopeRetryInterval
	switch g.state {
	case scopeStateMatched:
		if now.Before(g.nextDue) {
			if g.failed && !g.inGraceLocked(now) {
				return ScopeWait, nil // past the grace, a failed call keeps the tick from collecting
			}
			return ScopeProceed, nil
		}
	case scopeStateUnverified:
		if retryWait {
			return ScopeWait, nil
		}
	case scopeStateMismatched:
		if retryWait {
			return ScopeMismatched, nil
		}
	default: // no result
		if g.failed && retryWait {
			return ScopeWait, nil
		}
	}
	return g.assertLocked(ctx, now)
}

// inGraceLocked: a matched pipeline may still collect after a failed call, up to two intervals after
// its last successful assertion or restored match.
func (g *ScopeGate) inGraceLocked(now time.Time) bool {
	return !now.After(g.lastOK.Add(scopeFailGrace))
}

func (g *ScopeGate) assertLocked(ctx context.Context, now time.Time) (ScopeDecision, error) {
	res, err := g.cfg.Asserter.AssertScope(ctx)
	if err == nil && res != ScopeMatched && res != ScopeMismatch && res != ScopeUnverifiable {
		err = fmt.Errorf("source: scope assertion returned unknown result %d", res)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ScopeWait, err // leadership lost or shutdown mid-call: not a failed call
		}
		g.lastCall, g.failed = now, true
		g.result(scopeResultError)
		if g.state == scopeStateMatched {
			g.nextDue = now.Add(scopeRetryInterval)
			if g.inGraceLocked(now) {
				return ScopeProceed, nil
			}
		}
		return ScopeWait, err
	}
	g.lastCall, g.failed = now, false
	switch res {
	case ScopeMatched:
		g.result(scopeResultMatched)
		g.lastOK, g.nextDue = now, now.Add(scopeReassertInterval)
		g.setStateLocked(scopeStateMatched)
		if !g.viewKnown || g.storedCursor != g.cfg.Digest {
			g.writeLocked(ctx, now, g.cfg.Digest)
		}
		return ScopeProceed, nil
	case ScopeMismatch:
		g.result(scopeResultMismatch)
		g.setStateLocked(scopeStateMismatched)
		if !g.viewKnown || g.storedCursor != "" {
			g.writeLocked(ctx, now, "")
		}
		return ScopeMismatched, nil
	default: // ScopeUnverifiable
		g.result(scopeResultUnverifiable)
		switch g.state {
		case scopeStateMatched:
			g.lastOK, g.nextDue = now, now.Add(scopeReassertInterval)
			return ScopeProceed, nil
		case scopeStateMismatched:
			return ScopeMismatched, nil // only Matched ends a mismatch
		default:
			g.setStateLocked(scopeStateUnverified)
			return ScopeWait, nil
		}
	}
}

// writeLocked saves the remembered match (design §7). Its Time is the later of now and one second after
// the stored time the gate holds, so CheckMonotonic never refuses it for time when this leader's clock
// is behind the last one's. The gate's view changes only after the write succeeds, so a refused or
// failed write is tried again at the next such result.
func (g *ScopeGate) writeLocked(ctx context.Context, now time.Time, cursor string) {
	t := now
	if floor := g.storedTime.Add(time.Second); g.viewKnown && floor.After(t) {
		t = floor
	}
	w := model.Watermark{Time: t, Cursor: cursor, Epoch: coordinate.EpochFromContext(ctx)}
	if err := g.cfg.Store.Save(ctx, ScopeMatchKey(g.cfg.SourceInstance), w); err != nil {
		if ctx.Err() == nil {
			slog.Warn("scope gate: remembered match not saved; retried at the next result",
				"source", g.cfg.SourceInstance, "clear", cursor == "", "err", err)
		}
		return
	}
	g.viewKnown, g.storedTime, g.storedCursor = true, t, cursor
}
