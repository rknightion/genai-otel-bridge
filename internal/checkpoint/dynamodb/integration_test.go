// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

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

// TestSaveUpgradesVersionlessItem guards the legacy/hand-seeded path (Copilot review, PR #13): an item
// that exists but has no `version` attribute must be upgradable, not spin to RMW exhaustion on a
// `version = 0` condition that can never match an absent attribute.
func TestSaveUpgradesVersionlessItem(t *testing.T) {
	db := newTestClient(t)
	table := createTable(t, db)
	s := New(db, table, "ckpt#")
	key := model.CheckpointKey{SourceInstance: "ls", Loop: "runs", OutputFingerprint: "legacy"}
	ctx := context.Background()
	t0 := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)

	// Seed a versionless item directly (a pre-`version` / hand-seeded checkpoint).
	if _, err := db.PutItem(ctx, &awsddb.PutItemInput{
		TableName: aws.String(table),
		Item: map[string]ddbtypes.AttributeValue{
			"pk":    &ddbtypes.AttributeValueMemberS{Value: s.pk(key)},
			"time":  &ddbtypes.AttributeValueMemberS{Value: t0.Format(time.RFC3339Nano)},
			"epoch": &ddbtypes.AttributeValueMemberN{Value: "1"},
		},
	}); err != nil {
		t.Fatalf("seed versionless item: %v", err)
	}
	// A forward Save must upgrade it (attribute_not_exists(version)), not exhaust retries.
	if err := s.Save(ctx, key, model.Watermark{Time: t0.Add(time.Hour), Epoch: 1}); err != nil {
		t.Fatalf("upgrade versionless item: %v", err)
	}
	// A subsequent Save must then succeed via the normal version path.
	if err := s.Save(ctx, key, model.Watermark{Time: t0.Add(2 * time.Hour), Epoch: 1}); err != nil {
		t.Fatalf("second save after upgrade: %v", err)
	}
}
