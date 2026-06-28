// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func TestRoundTripAndFence(t *testing.T) {
	db := newTestClient(t)
	table := createTable(t, db)
	s := New(db, table, "ckpt#")
	key := model.CheckpointKey{SourceInstance: "portkey-dev", Loop: "analytics", OutputFingerprint: "fp1"}
	ctx := context.Background()

	if wm, err := s.Load(ctx, key); err != nil || !wm.Time.IsZero() {
		t.Fatalf("absent load: wm=%v err=%v, want zero/nil", wm, err)
	}
	t1 := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	if err := s.Save(ctx, key, model.Watermark{Time: t1, Epoch: 5}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	got, err := s.Load(ctx, key)
	if err != nil || !got.Time.Equal(t1) || got.Epoch != 5 {
		t.Fatalf("load after save: %v err=%v", got, err)
	}
	// stale epoch rejected with ErrStaleWrite (benign)
	err = s.Save(ctx, key, model.Watermark{Time: t1.Add(time.Hour), Epoch: 4})
	if !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("lower-epoch save err=%v, want ErrStaleWrite", err)
	}
	// forward time, same epoch accepted
	if err := s.Save(ctx, key, model.Watermark{Time: t1.Add(time.Hour), Epoch: 5}); err != nil {
		t.Fatalf("forward save: %v", err)
	}
}

func TestCursorRelaxation(t *testing.T) {
	db := newTestClient(t)
	table := createTable(t, db)
	s := New(db, table, "ckpt#")
	key := model.CheckpointKey{SourceInstance: "ls", Loop: "runs", OutputFingerprint: "fp"}
	ctx := context.Background()
	tm := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	if err := s.Save(ctx, key, model.Watermark{Time: tm, Cursor: "a", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	// same time, new cursor accepted
	if err := s.Save(ctx, key, model.Watermark{Time: tm, Cursor: "b", Epoch: 1}); err != nil {
		t.Fatalf("same-time new-cursor save: %v", err)
	}
	// same time, same cursor rejected
	if err := s.Save(ctx, key, model.Watermark{Time: tm, Cursor: "b", Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("same-time same-cursor err=%v, want ErrStaleWrite", err)
	}
}

// TestZeroTimeCursorWatermark guards the logs-export first-window watermark (Time==zero, cursor-only).
// A naive UnixNano encoding corrupts this; RFC3339Nano must round-trip the zero time to IsZero()==true.
func TestZeroTimeCursorWatermark(t *testing.T) {
	db := newTestClient(t)
	table := createTable(t, db)
	s := New(db, table, "ckpt#")
	key := model.CheckpointKey{SourceInstance: "ls", Loop: "runs", OutputFingerprint: "logs"}
	ctx := context.Background()

	if err := s.Save(ctx, key, model.Watermark{Time: time.Time{}, Cursor: "page-1", Epoch: 1}); err != nil {
		t.Fatalf("zero-time cursor save: %v", err)
	}
	got, err := s.Load(ctx, key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Time.IsZero() {
		t.Fatalf("loaded time = %v, want IsZero() (got corrupted by encoding)", got.Time)
	}
	if got.Cursor != "page-1" {
		t.Fatalf("loaded cursor = %q, want page-1", got.Cursor)
	}
	// cursor-relaxation still works at zero time: new cursor accepted, same cursor rejected
	if err := s.Save(ctx, key, model.Watermark{Time: time.Time{}, Cursor: "page-2", Epoch: 1}); err != nil {
		t.Fatalf("zero-time new-cursor save: %v", err)
	}
	if err := s.Save(ctx, key, model.Watermark{Time: time.Time{}, Cursor: "page-2", Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("zero-time same-cursor err=%v, want ErrStaleWrite", err)
	}
}
