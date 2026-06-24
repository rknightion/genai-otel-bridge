// SPDX-License-Identifier: AGPL-3.0-only

// Package coordinate provides single-active-replica semantics (ARCHITECTURE.md §5/§8). The
// leader epoch (lease transitions) rides in the leader context so the checkpoint write fence
// (Cdx-C14) can read it without changing the frozen onElected signature.
package coordinate

import "context"

// Coordinator runs onElected while this replica is leader; leaderCtx is cancelled on loss. FROZEN.
type Coordinator interface {
	Run(ctx context.Context, onElected func(leaderCtx context.Context)) error
}

type ctxKey int

const epochKey ctxKey = 0

// WithEpoch stamps the leader epoch onto a context.
func WithEpoch(ctx context.Context, epoch int64) context.Context {
	return context.WithValue(ctx, epochKey, epoch)
}

// EpochFromContext returns the leader epoch (0 if unset).
func EpochFromContext(ctx context.Context) int64 {
	if v, ok := ctx.Value(epochKey).(int64); ok {
		return v
	}
	return 0
}

// Noop is always leader (single-replica/dev). Epoch is a constant 1.
type Noop struct{}

func (Noop) Run(ctx context.Context, onElected func(context.Context)) error {
	onElected(WithEpoch(ctx, 1)) // blocks until ctx is cancelled (the scheduler honours leaderCtx)
	return ctx.Err()
}

var _ Coordinator = Noop{}
