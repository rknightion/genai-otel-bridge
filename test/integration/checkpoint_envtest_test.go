// SPDX-License-Identifier: AGPL-3.0-only

//go:build envtest

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	cpcm "github.com/rknightion/genai-otel-bridge/internal/checkpoint/configmap"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// Under a REAL apiserver (resource-version optimistic concurrency), a forward Save persists and a
// backwards Save is refused with ErrStaleWrite — the fake clientset does not enforce conflicts, so
// this is the layer that proves the monotonic fence holds against real etcd RMW.
func TestRealApiserverCheckpointMonotonic(t *testing.T) {
	cs, ns := startEnv(t)
	store := cpcm.New(cs, ns, "genai-otel-bridge-checkpoints")
	ctx := context.Background()
	key := model.CheckpointKey{SourceInstance: "portkey-e2e", Loop: "analytics", OutputFingerprint: "testfp"}

	t0 := time.Now().UTC().Truncate(time.Second)
	if err := store.Save(ctx, key, model.Watermark{Time: t0, Epoch: 1}); err != nil {
		t.Fatalf("save t0: %v", err)
	}
	if err := store.Save(ctx, key, model.Watermark{Time: t0.Add(time.Minute), Epoch: 1}); err != nil {
		t.Fatalf("forward save: %v", err)
	}
	if err := store.Save(ctx, key, model.Watermark{Time: t0.Add(-time.Minute), Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("expected ErrStaleWrite on backward save, got %v", err)
	}
	got, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Time.Equal(t0.Add(time.Minute)) {
		t.Fatalf("watermark not at forward value: got %v want %v", got.Time, t0.Add(time.Minute))
	}
}
