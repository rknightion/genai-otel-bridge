// SPDX-License-Identifier: Apache-2.0

package dynamodb

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// blockingUpdateAPI blocks every UpdateItem (the renew calls in lead) until release is closed OR the
// request context is cancelled — modelling a silent/blackholed DynamoDB connection whose UpdateItem
// never returns on its own (aws-sdk-go-v2's default client sets no request timeout). [#30]
type blockingUpdateAPI struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (f *blockingUpdateAPI) UpdateItem(ctx context.Context, _ *awsddb.UpdateItemInput, _ ...func(*awsddb.Options)) (*awsddb.UpdateItemOutput, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	select {
	case <-f.release:
		return nil, errors.New("released")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *blockingUpdateAPI) numCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRenewStallCancelsLeaderCtxWithinDeadline: a renew UpdateItem that blocks longer than
// renew_deadline must cancel leaderCtx no later than ~renew_deadline after the last successful renew
// (here, after election, which seeds the deadline). Without the out-of-band watchdog the deadline check
// is unreachable while renew blocks and leaderCtx is never cancelled (the #30 bug). [#30 acceptance #1]
func TestRenewStallCancelsLeaderCtxWithinDeadline(t *testing.T) {
	f := &blockingUpdateAPI{release: make(chan struct{})}
	// lease(200ms) > renewDeadline(100ms) > retry(20ms)
	c := New(f, "t", "lock#l", "me", 200*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond)

	cancelledAt := make(chan time.Time, 1)
	leadReturned := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(leadReturned)
		c.lead(context.Background(), 1, time.Now(), func(lc context.Context) {
			<-lc.Done()
			cancelledAt <- time.Now()
		})
	}()

	select {
	case ts := <-cancelledAt:
		if d := ts.Sub(start); d > 500*time.Millisecond {
			t.Fatalf("leaderCtx cancelled after %v; watchdog should step down ~renew_deadline (100ms) after election", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leaderCtx never cancelled despite a stalled renew — watchdog missing (the #30 bug)")
	}

	close(f.release) // let any blocked renew unwind
	select {
	case <-leadReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("lead() did not return after the stalled renew was released")
	}
}

// TestRenewInFlightWhenLeaderCtxCancelled proves onElected's ctx is cancelled while a renew UpdateItem
// is still in flight (blocked, not yet released) — i.e. the in-flight single-emit fence fires even
// though the renew call itself has not returned. [#30 acceptance #2]
func TestRenewInFlightWhenLeaderCtxCancelled(t *testing.T) {
	f := &blockingUpdateAPI{release: make(chan struct{})}
	c := New(f, "t", "lock#l", "me", 200*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond)

	inFlight := make(chan bool, 1)
	leadReturned := make(chan struct{})
	go func() {
		defer close(leadReturned)
		c.lead(context.Background(), 1, time.Now(), func(lc context.Context) {
			<-lc.Done()
			// The blocking renew has NOT been released, so its UpdateItem is still in flight.
			select {
			case <-f.release:
				inFlight <- false // released — not in flight anymore
			default:
				inFlight <- f.numCalls() > 0
			}
		})
	}()

	select {
	case v := <-inFlight:
		if !v {
			t.Fatal("leaderCtx was cancelled but no renew UpdateItem was in flight")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leaderCtx never cancelled")
	}

	close(f.release)
	select {
	case <-leadReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("lead() did not return")
	}
}

// slowThenHangAPI models a DynamoDB path that answers one request slowly and then goes dead: the first
// acquire (and the first renew) succeed after the configured delay, and every later call blocks until
// its context is cancelled. It records when each successful request started and the expiresAtMs that
// request wrote, which is the lock expiry a standby compares against. [#377]
type slowThenHangAPI struct {
	acquireDelay, renewDelay time.Duration
	hangAllRenews            bool // every renew hangs, including the first

	mu               sync.Mutex
	acquires, renews int
	lastStart        time.Time
	lastExpiresAt    time.Time
}

func (f *slowThenHangAPI) UpdateItem(ctx context.Context, in *awsddb.UpdateItemInput, _ ...func(*awsddb.Options)) (*awsddb.UpdateItemOutput, error) {
	start := time.Now()
	isAcquire := strings.Contains(aws.ToString(in.UpdateExpression), "ADD fence")
	f.mu.Lock()
	var n int
	var delay time.Duration
	if isAcquire {
		f.acquires++
		n, delay = f.acquires, f.acquireDelay
	} else {
		f.renews++
		n, delay = f.renews, f.renewDelay
	}
	f.mu.Unlock()
	if n > 1 || (!isAcquire && f.hangAllRenews) {
		<-ctx.Done() // dead path: hang until the caller gives up
		return nil, ctx.Err()
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	expMs, err := strconv.ParseInt(in.ExpressionAttributeValues[":exp"].(*ddbtypes.AttributeValueMemberN).Value, 10, 64)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.lastStart, f.lastExpiresAt = start, time.UnixMilli(expMs)
	f.mu.Unlock()
	if isAcquire {
		return &awsddb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{"fence": nstr(1)}}, nil
	}
	return &awsddb.UpdateItemOutput{}, nil
}

func (f *slowThenHangAPI) last() (start, expiresAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastStart, f.lastExpiresAt
}

// runUntilFirstStepDown runs the coordinator against f and returns when the first leader context is
// cancelled.
func runUntilFirstStepDown(t *testing.T, c *Coordinator) time.Time {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancelledAt := make(chan time.Time, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = c.Run(ctx, func(lc context.Context) {
			<-lc.Done()
			select {
			case cancelledAt <- time.Now():
			default:
			}
		})
	}()
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}()
	select {
	case at := <-cancelledAt:
		return at
	case <-time.After(15 * time.Second):
		t.Fatal("leader context never cancelled")
		return time.Time{}
	}
}

// TestRenewWatchdogMeasuredFromRequestStart: a renew that succeeds after longer than
// lease_duration - renew_deadline, followed by renews that hang, must not leave the leader context
// alive past the lock expiry that renew wrote. The renew deadline is measured from when the request
// started (as client-go does), so step-down comes no later than request start + renew_deadline.
// [#377 AC1, AC2]
func TestRenewWatchdogMeasuredFromRequestStart(t *testing.T) {
	const lease, renewDeadline, retry = 3 * time.Second, 2 * time.Second, 100 * time.Millisecond
	f := &slowThenHangAPI{renewDelay: 1800 * time.Millisecond} // > lease - renewDeadline (1s)
	c := New(f, "t", "lock#l", "me", lease, renewDeadline, retry)

	cancelledAt := runUntilFirstStepDown(t, c)
	start, expiresAt := f.last()
	if start.IsZero() {
		t.Fatal("the slow renew never completed")
	}
	if !cancelledAt.Before(expiresAt) {
		t.Errorf("leader context cancelled %v after the lock expired (a standby could already lead)", cancelledAt.Sub(expiresAt))
	}
	if limit := start.Add(renewDeadline + 500*time.Millisecond); cancelledAt.After(limit) {
		t.Errorf("leader context cancelled %v after the renew request started; want <= renew_deadline (%v) plus slack", cancelledAt.Sub(start), renewDeadline)
	}
}

// TestAcquireWatchdogMeasuredFromRequestStart: the first watchdog deadline is measured from the start
// of the acquire request, not from when lead begins, so a slow acquire followed by hung renews still
// steps down before the lock it wrote expires. [#377 AC1, AC2]
func TestAcquireWatchdogMeasuredFromRequestStart(t *testing.T) {
	const lease, renewDeadline, retry = 3 * time.Second, 2 * time.Second, 100 * time.Millisecond
	f := &slowThenHangAPI{acquireDelay: 1800 * time.Millisecond, hangAllRenews: true}
	c := New(f, "t", "lock#l", "me", lease, renewDeadline, retry)

	cancelledAt := runUntilFirstStepDown(t, c)
	start, expiresAt := f.last()
	if !cancelledAt.Before(expiresAt) {
		t.Errorf("leader context cancelled %v after the lock expired (a standby could already lead)", cancelledAt.Sub(expiresAt))
	}
	if limit := start.Add(renewDeadline + 500*time.Millisecond); cancelledAt.After(limit) {
		t.Errorf("leader context cancelled %v after the acquire request started; want <= renew_deadline (%v) plus slack", cancelledAt.Sub(start), renewDeadline)
	}
}

// TestAcquireSlowerThanRenewDeadlineStepsDownAtOnce: when the acquire itself took longer than
// renew_deadline, the deadline has already passed by the time lead starts, so the leader context is
// cancelled at once rather than a full renew_deadline later. [#377 AC2]
func TestAcquireSlowerThanRenewDeadlineStepsDownAtOnce(t *testing.T) {
	const lease, renewDeadline, retry = 600 * time.Millisecond, 400 * time.Millisecond, 50 * time.Millisecond
	f := &slowThenHangAPI{acquireDelay: 500 * time.Millisecond, hangAllRenews: true}
	c := New(f, "t", "lock#l", "me", lease, renewDeadline, retry)

	cancelledAt := runUntilFirstStepDown(t, c)
	_, expiresAt := f.last()
	if !cancelledAt.Before(expiresAt) {
		t.Errorf("leader context cancelled %v after the lock expired", cancelledAt.Sub(expiresAt))
	}
}

// lostLockAPI acquires, then reports every renew as a failed condition (another holder took the lock).
type lostLockAPI struct{}

func (lostLockAPI) UpdateItem(_ context.Context, in *awsddb.UpdateItemInput, _ ...func(*awsddb.Options)) (*awsddb.UpdateItemOutput, error) {
	if strings.Contains(aws.ToString(in.UpdateExpression), "ADD fence") {
		return &awsddb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{"fence": nstr(1)}}, nil
	}
	return nil, &ddbtypes.ConditionalCheckFailedException{}
}

// TestLeadStepsDownOnLostLock: a renew that finds the lock taken steps down at the next renew, well
// inside renew_deadline. [#377 AC3]
func TestLeadStepsDownOnLostLock(t *testing.T) {
	const lease, renewDeadline, retry = 3 * time.Second, 2 * time.Second, 50 * time.Millisecond
	c := New(lostLockAPI{}, "t", "lock#l", "me", lease, renewDeadline, retry)
	start := time.Now()
	cancelledAt := runUntilFirstStepDown(t, c)
	if d := cancelledAt.Sub(start); d > time.Second {
		t.Fatalf("lost lock stepped down after %v; want about one retry period (%v)", d, retry)
	}
}
