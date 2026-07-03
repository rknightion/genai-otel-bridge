// SPDX-License-Identifier: AGPL-3.0-only

// Package coordinate provides single-active-replica semantics (ARCHITECTURE.md §5/§8). The
// leader epoch (lease transitions) rides in the leader context so the checkpoint write fence
// (Cdx-C14) can read it without changing the frozen onElected signature.
package coordinate

import (
	"context"
	"fmt"
)

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

// RequireIdentity returns a non-nil error when leader election is enabled (coordinator lease|dynamodb)
// but the replica identity is empty [#87]. An empty identity is silently unsafe: client-go's
// NewLeaderElector refuses an empty Lock identity (the pod crash-loops), and on the DynamoDB path an
// empty holder plus an empty service.instance.id collides self-telemetry across replicas, destroying the
// leader-overlap diagnostics the field exists for. Single-replica (coordinator=none) needs no identity.
// Callers must fail fast on a non-nil return (operationally honest — never run leader election blind).
func RequireIdentity(coordinator, identity string) error {
	if identity == "" && (coordinator == "lease" || coordinator == "dynamodb") {
		return fmt.Errorf("leader-election identity is empty but ha.coordinator=%q requires a stable per-replica identity: set -identity or $POD_NAME (k8s downward API), or on ECS ensure $ECS_CONTAINER_METADATA_URI_V4 is reachable so the Task ARN can be read", coordinator)
	}
	return nil
}

// Noop is always leader (single-replica/dev). It stamps max(1, Epoch) as the leader epoch (the zero
// value ⇒ epoch 1, the historical default).
//
// [#45] MIGRATION TRAP — switching ha.coordinator to `none` over a checkpoint store that a prior HA
// (lease/dynamodb) deployment advanced to epoch ≥ 2 would, with a constant epoch 1, permanently fence
// every watermark write: CheckMonotonic rejects incoming.Epoch < stored.Epoch and the fenced Save is
// benign (ErrStaleWrite), so the loop never backs off — it re-collects and re-emits the same window at
// full cadence forever (checkpoint_fenced fires, so it is alertable, not silent) and never self-heals.
// Constructing the Noop with a baseline epoch ≥ the surviving stored epoch (see NoopWithEpoch) lets
// writes succeed; single-replica has exactly one writer, so a higher epoch carries no cross-writer risk.
// See internal/coordinate/CLAUDE.md and ARCHITECTURE.md ledger #17 (the dynamodb-outage analogue).
type Noop struct{ Epoch int64 }

// NoopWithEpoch returns a single-replica coordinator that stamps max(1, epoch), so a checkpoint written
// at a higher epoch by a prior HA deployment is not permanently fenced on a downgrade to none (#45).
func NoopWithEpoch(epoch int64) Noop { return Noop{Epoch: epoch} }

func (n Noop) Run(ctx context.Context, onElected func(context.Context)) error {
	onElected(WithEpoch(ctx, max(int64(1), n.Epoch))) // blocks until ctx is cancelled (scheduler honours leaderCtx)
	return ctx.Err()
}

var _ Coordinator = Noop{}
