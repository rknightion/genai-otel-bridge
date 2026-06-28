// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
)

func TestElectionAndFence(t *testing.T) {
	db := newTestClient(t)
	table := createTable(t, db)
	c := New(db, table, "lock#leader", "node-a", 2*time.Second, 1*time.Second, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var gotEpoch int64
	var mu sync.Mutex
	elected := make(chan struct{})
	go func() {
		_ = c.Run(ctx, func(leaderCtx context.Context) {
			mu.Lock()
			gotEpoch = coordinate.EpochFromContext(leaderCtx)
			mu.Unlock()
			close(elected)
			<-leaderCtx.Done()
		})
	}()
	select {
	case <-elected:
	case <-time.After(5 * time.Second):
		t.Fatal("node-a was never elected")
	}
	mu.Lock()
	if gotEpoch != 1 {
		t.Fatalf("first leader epoch = %d, want 1", gotEpoch)
	}
	mu.Unlock()
	cancel()
}

func TestFailoverBumpsFence(t *testing.T) {
	db := newTestClient(t)
	table := createTable(t, db)
	mk := func(id string) *Coordinator {
		return New(db, table, "lock#leader", id, 1500*time.Millisecond, 700*time.Millisecond, 150*time.Millisecond)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	epochA := make(chan int64, 1)
	go func() {
		_ = mk("node-a").Run(ctxA, func(lc context.Context) {
			epochA <- coordinate.EpochFromContext(lc)
			<-lc.Done()
		})
	}()
	var ea int64
	select {
	case ea = <-epochA:
	case <-time.After(10 * time.Second):
		t.Fatal("node-a was never elected")
	}
	if ea != 1 {
		t.Fatalf("node-a epoch=%d want 1", ea)
	}

	// node-b contends; must NOT win while A holds.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	epochB := make(chan int64, 1)
	go func() {
		_ = mk("node-b").Run(ctxB, func(lc context.Context) {
			epochB <- coordinate.EpochFromContext(lc)
			<-lc.Done()
		})
	}()
	select {
	case e := <-epochB:
		t.Fatalf("node-b won leadership (epoch=%d) while node-a held it", e)
	case <-time.After(1 * time.Second):
	}

	// A steps down → B must take over with a STRICTLY GREATER fence.
	cancelA()
	select {
	case eb := <-epochB:
		if eb <= ea {
			t.Fatalf("failover fence=%d not > previous=%d", eb, ea)
		}
	case <-time.After(10 * time.Second): // generous: integration test, real-time renew/expiry interplay
		t.Fatal("node-b never took over after node-a stepped down")
	}
}
