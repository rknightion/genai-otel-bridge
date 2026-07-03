// SPDX-License-Identifier: AGPL-3.0-only

// Package checkpoint defines the Checkpointer seam (ARCHITECTURE.md §5) and the monotonic+
// epoch write fence — the real guarantee against a backward/double-advanced frontier, since a
// Lease only reduces overlap, it is not a write fence (Cdx-C14).
package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// ErrStaleWrite means the incoming watermark does not strictly advance the stored one, or
// comes from a stale (demoted) leader epoch. It is BENIGN to the caller: the frontier is
// already at/ahead — log at debug and continue, do not crash.
var ErrStaleWrite = errors.New("checkpoint: stale or non-advancing write rejected")

// ErrUnencodable means the watermark cannot be durably encoded AND parsed back by the backends.
// Unlike ErrStaleWrite this is NOT benign: a bad Time must surface as a loud per-write error so it
// can never become a durable poison value (an empty/garbled key that later Load/Save refuse forever).
var ErrUnencodable = errors.New("checkpoint: watermark not durably encodable")

// CheckEncodable rejects a watermark whose Time cannot round-trip through the durable backends.
// Both prod encodings cap the year at [0,9999] — configmap marshals the Time as JSON
// (time.Time.MarshalJSON errors outside [0,9999]) and dynamodb stores time.RFC3339Nano (whose parse
// cannot read a 5-digit year back), so a year outside [1,9999] would either be dropped as "" or written
// as an unparseable string that permanently wedges the loop. The lower bound is 1 because the zero
// Watermark is year 1 (0001-01-01) — the legitimate minimum (a cursor-only logs-export watermark).
// Backends call this at the top of Save so a transient bad input is a loud reject, never durable poison.
func CheckEncodable(w model.Watermark) error {
	if y := w.Time.Year(); y < 1 || y > 9999 {
		return fmt.Errorf("%w: time %s has year %d outside [1,9999]", ErrUnencodable, w.Time, y)
	}
	return nil
}

// Checkpointer durably stores watermarks keyed by CheckpointKey, shared across replicas. FROZEN.
type Checkpointer interface {
	Load(ctx context.Context, key model.CheckpointKey) (model.Watermark, error) // zero wm if absent; error if present-but-unreadable
	Save(ctx context.Context, key model.CheckpointKey, w model.Watermark) error // monotonic + epoch-fenced
}

// CheckMonotonic accepts incoming iff it is from a >= epoch AND makes forward progress: Time strictly
// advances, OR Time is unchanged but the Cursor changed. The cursor relaxation lets a stateful loop
// (the logs-export job state machine) persist in-flight job progress across ticks at a NON-advancing
// Time — `Watermark.Time` still only moves forward when a whole window completes, so the gap-free
// frontier is unchanged; the Cursor is an opaque resume token the fence does not interpret. Time must
// never regress, and the epoch fence still wins (a demoted leader cannot advance Time OR the cursor).
func CheckMonotonic(stored, incoming model.Watermark) error {
	if incoming.Epoch < stored.Epoch {
		return fmt.Errorf("%w: epoch %d < stored %d", ErrStaleWrite, incoming.Epoch, stored.Epoch)
	}
	if incoming.Time.Before(stored.Time) {
		return fmt.Errorf("%w: time %s before stored %s", ErrStaleWrite, incoming.Time, stored.Time)
	}
	if incoming.Time.Equal(stored.Time) && incoming.Cursor == stored.Cursor {
		return fmt.Errorf("%w: time %s not after stored %s and cursor unchanged", ErrStaleWrite, incoming.Time, stored.Time)
	}
	return nil
}
