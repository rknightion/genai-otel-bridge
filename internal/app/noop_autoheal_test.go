// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// TestNoopEpochAutoHealsFromStoredCheckpoint (#45) — when ha.coordinator=none runs over a checkpoint a
// prior HA (lease/dynamodb) deployment advanced to epoch ≥ 2, Build must floor the single-replica Noop's
// epoch to the max stored epoch so watermark writes are NOT permanently fenced. Regression for the
// migration trap where a constant epoch 1 spun the loop re-emitting the same window forever.
func TestNoopEpochAutoHealsFromStoredCheckpoint(t *testing.T) {
	cpPath := filepath.Join(t.TempDir(), "wm.yaml")
	cp, err := file.New(cpPath, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := minimalConfig("http://127.0.0.1:1")

	// Discover the loop's real CheckpointKey (OutputFingerprint is only known post-build) via a first build.
	probe, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	key := probe.Specs()[0].Loop.Key()

	// Seed the durable checkpoint at epoch 3, as a prior HA (lease/dynamodb) deployment would have left it.
	seeded := model.Watermark{Time: time.Now().UTC().Add(-time.Hour).Truncate(time.Second), Epoch: 3}
	if err := cp.Save(context.Background(), key, seeded); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	// Rebuild over the surviving checkpoint with coordinator=none.
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	noop, ok := a.coord.(coordinate.Noop)
	if !ok {
		t.Fatalf("coordinator=none must stay a coordinate.Noop; got %T", a.coord)
	}
	if noop.Epoch < seeded.Epoch {
		t.Fatalf("Noop epoch=%d must be floored to the stored checkpoint epoch %d (#45 auto-heal)", noop.Epoch, seeded.Epoch)
	}

	// The point of the auto-heal: a watermark write at the coordinator's epoch must now be ACCEPTED by
	// the monotonic fence (not ErrStaleWrite), so the loop advances instead of spinning fenced forever.
	epoch := max(int64(1), noop.Epoch)
	next := model.Watermark{Time: seeded.Time.Add(time.Minute), Epoch: epoch}
	if err := cp.Save(context.Background(), key, next); err != nil {
		t.Fatalf("watermark write at coordinator epoch %d must succeed after auto-heal, got %v", epoch, err)
	}
}

// TestNoopEpochStaysMinimalWithoutStoredCheckpoint (#45) — a fresh single-replica deployment (no prior
// durable epoch) must NOT be elevated: stamping a maximal sentinel would re-create the trap in the
// none→lease direction. The Noop stays at the historical baseline (effective epoch 1).
func TestNoopEpochStaysMinimalWithoutStoredCheckpoint(t *testing.T) {
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	cfg := minimalConfig("http://127.0.0.1:1")
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	noop, ok := a.coord.(coordinate.Noop)
	if !ok {
		t.Fatalf("coordinator=none must stay a coordinate.Noop; got %T", a.coord)
	}
	if got := max(int64(1), noop.Epoch); got != 1 {
		t.Fatalf("fresh deployment must keep effective epoch 1 (no sentinel elevation), got %d", got)
	}
}
