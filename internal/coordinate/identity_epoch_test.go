// SPDX-License-Identifier: AGPL-3.0-only

package coordinate

import (
	"context"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// TestRequireIdentity: leader election (lease|dynamodb) with an empty identity must be rejected; a
// single-replica (none) run needs no identity. [#87 acceptance #3]
func TestRequireIdentity(t *testing.T) {
	cases := []struct {
		coordinator, identity string
		wantErr               bool
	}{
		{"lease", "", true},
		{"dynamodb", "", true},
		{"lease", "pod-1", false},
		{"dynamodb", "arn:task/abc", false},
		{"none", "", false},
		{"", "", false}, // buildHA runs before config validation; a bare/invalid value must not trip the guard
	}
	for _, tc := range cases {
		err := RequireIdentity(tc.coordinator, tc.identity)
		if (err != nil) != tc.wantErr {
			t.Errorf("RequireIdentity(%q, %q) err=%v, wantErr=%v", tc.coordinator, tc.identity, err, tc.wantErr)
		}
	}
}

// noopEpoch runs a Noop and returns the leader epoch it stamps.
func noopEpoch(t *testing.T, n Noop) int64 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan int64, 1)
	go func() {
		_ = n.Run(ctx, func(lc context.Context) {
			got <- EpochFromContext(lc)
			<-lc.Done()
		})
	}()
	select {
	case e := <-got:
		return e
	case <-time.After(time.Second):
		t.Fatal("Noop did not elect")
		return 0
	}
}

// TestNoopEpochMaxSemantics: the default Noop stamps epoch 1 (back-compat); NoopWithEpoch(n) stamps
// max(1, n). [#45 mechanism]
func TestNoopEpochMaxSemantics(t *testing.T) {
	if e := noopEpoch(t, Noop{}); e != 1 {
		t.Fatalf("default Noop epoch=%d, want 1", e)
	}
	if e := noopEpoch(t, NoopWithEpoch(3)); e != 3 {
		t.Fatalf("NoopWithEpoch(3) epoch=%d, want 3", e)
	}
	if e := noopEpoch(t, NoopWithEpoch(0)); e != 1 {
		t.Fatalf("NoopWithEpoch(0) epoch=%d, want 1 (max(1,0))", e)
	}
}

// TestNoopEpochAvoidsCheckpointFence reproduces the #45 downgrade trap end-to-end against the real
// checkpoint fence: a checkpoint saved at epoch 3, then a Noop-coordinated run. The default Noop (epoch 1)
// is permanently fenced (ErrStaleWrite); NoopWithEpoch(3) makes the same forward write succeed.
// [#45 acceptance #1 — "watermark writes must succeed"]
func TestNoopEpochAvoidsCheckpointFence(t *testing.T) {
	stored := model.Watermark{Time: time.Unix(100, 0), Epoch: 3}

	fencedWM := model.Watermark{Time: time.Unix(200, 0), Epoch: noopEpoch(t, Noop{})}
	if err := checkpoint.CheckMonotonic(stored, fencedWM); err == nil {
		t.Fatal("default Noop (epoch 1) should be fenced by a stored epoch-3 checkpoint (the #45 bug)")
	}

	okWM := model.Watermark{Time: time.Unix(200, 0), Epoch: noopEpoch(t, NoopWithEpoch(3))}
	if err := checkpoint.CheckMonotonic(stored, okWM); err != nil {
		t.Fatalf("NoopWithEpoch(3) forward write over a stored epoch-3 checkpoint must succeed, got %v", err)
	}
}
