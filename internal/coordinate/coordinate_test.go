// SPDX-License-Identifier: AGPL-3.0-only

package coordinate

import (
	"context"
	"testing"
	"time"
)

func TestNoopElectsImmediatelyWithEpoch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	elected := make(chan int64, 1)
	go Noop{}.Run(ctx, func(lc context.Context) {
		elected <- EpochFromContext(lc)
		<-lc.Done()
	})
	select {
	case e := <-elected:
		if e != 1 {
			t.Fatalf("noop epoch=%d want 1", e)
		}
	case <-time.After(time.Second):
		t.Fatal("noop did not elect")
	}
	cancel()
}

func TestEpochContextRoundTrip(t *testing.T) {
	if EpochFromContext(context.Background()) != 0 {
		t.Fatal("absent epoch must be 0")
	}
	if EpochFromContext(WithEpoch(context.Background(), 7)) != 7 {
		t.Fatal("epoch round-trip failed")
	}
}
