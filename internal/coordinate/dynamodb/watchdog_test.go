// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
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
		c.lead(context.Background(), 1, func(lc context.Context) {
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
		c.lead(context.Background(), 1, func(lc context.Context) {
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
