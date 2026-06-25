// SPDX-License-Identifier: AGPL-3.0-only

package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func TestFileRoundTripAndFence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wm.yaml")
	s, err := New(p, false)
	if err != nil {
		t.Fatal(err)
	}
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	ctx := context.Background()

	// Absent ⇒ zero watermark, no error.
	if w, err := s.Load(ctx, key); err != nil || !w.Time.IsZero() {
		t.Fatalf("absent load: w=%+v err=%v", w, err)
	}
	// Save forward, reload.
	w1 := model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 1}
	if err := s.Save(ctx, key, w1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx, key); !got.Time.Equal(w1.Time) {
		t.Fatalf("reload: %+v", got)
	}
	// Stale (backward) ⇒ ErrStaleWrite, stored unchanged.
	if err := s.Save(ctx, key, model.Watermark{Time: time.Unix(50, 0).UTC(), Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("want ErrStaleWrite, got %v", err)
	}
	// Persistence across reopen.
	s2, _ := New(p, false)
	if got, _ := s2.Load(ctx, key); !got.Time.Equal(w1.Time) {
		t.Fatalf("reopen: %+v", got)
	}
}

// TestFileCursorFence proves the cursor-relaxed fence holds THROUGH the real store (review-H1: the
// store must pass stored.Cursor to CheckMonotonic, else a same-Time/same-cursor write is wrongly
// accepted). A same-Time write that advances the Cursor is accepted; one that repeats Time+Cursor is
// rejected; a backward Time is rejected even with a new cursor.
func TestFileCursorFence(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "logs_export", OutputFingerprint: "fp"}
	ctx := context.Background()
	t100 := time.Unix(100, 0).UTC()
	if err := s.Save(ctx, key, model.Watermark{Time: t100, Cursor: "a", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	// same Time, SAME cursor ⇒ no progress ⇒ rejected (the property review-H1 found inert).
	if err := s.Save(ctx, key, model.Watermark{Time: t100, Cursor: "a", Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("same-Time/same-cursor must be ErrStaleWrite, got %v", err)
	}
	// same Time, NEW cursor ⇒ accepted (job step within a window).
	if err := s.Save(ctx, key, model.Watermark{Time: t100, Cursor: "b", Epoch: 1}); err != nil {
		t.Fatalf("same-Time/new-cursor must be accepted, got %v", err)
	}
	if got, _ := s.Load(ctx, key); got.Cursor != "b" || !got.Time.Equal(t100) {
		t.Fatalf("load after cursor advance: %+v", got)
	}
	// backward Time rejected even with a new cursor.
	if err := s.Save(ctx, key, model.Watermark{Time: time.Unix(50, 0).UTC(), Cursor: "c", Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("backward Time must be ErrStaleWrite even with a cursor change, got %v", err)
	}
}

func TestFileUnreadableRefusesByDefault(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wm.yaml")
	os.WriteFile(p, []byte("{ not: valid: yaml :"), 0o600)
	if _, err := New(p, false); err == nil {
		t.Fatal("corrupt file must refuse-start when ignoreInvalid=false")
	}
	if _, err := New(p, true); err != nil {
		t.Fatalf("ignoreInvalid=true should bootstrap with a warning, got %v", err)
	}
}
