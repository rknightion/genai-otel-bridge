// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

type fakeAPI struct {
	getOut *awsddb.GetItemOutput
	getErr error
	putErr error
	puts   int
}

func (f *fakeAPI) GetItem(context.Context, *awsddb.GetItemInput, ...func(*awsddb.Options)) (*awsddb.GetItemOutput, error) {
	return f.getOut, f.getErr
}
func (f *fakeAPI) PutItem(context.Context, *awsddb.PutItemInput, ...func(*awsddb.Options)) (*awsddb.PutItemOutput, error) {
	f.puts++
	return &awsddb.PutItemOutput{}, f.putErr
}

func TestSaveRefusesCorruptStored(t *testing.T) {
	corrupt := &awsddb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
		"pk":        &ddbtypes.AttributeValueMemberS{Value: "ckpt#x"},
		"timeNanos": &ddbtypes.AttributeValueMemberN{Value: "1"},
		// epoch + version intentionally missing → decode error
	}}
	f := &fakeAPI{getOut: corrupt}
	s := New(f, "t", "ckpt#")
	err := s.Save(context.Background(), model.CheckpointKey{SourceInstance: "i", Loop: "l", OutputFingerprint: "fp"}, model.Watermark{Time: time.Now(), Epoch: 1})
	if err == nil || f.puts != 0 {
		t.Fatalf("corrupt stored: err=%v puts=%d, want error and zero puts (never clobber)", err, f.puts)
	}
}

// TestDecodeRejectsNonNumericVersion [copilot-pr13]: a PRESENT-but-non-numeric `version` (e.g. a
// string from a hand-seeded/corrupt item) must be a decode error, NOT a silent version=0. Silent 0
// sends Save() down the versionPresent path where `version = :v` (a Number) can never match a String
// attribute, so it would spin on conditional failures until retries exhaust and return an opaque error.
func TestDecodeRejectsNonNumericVersion(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		"time":    &ddbtypes.AttributeValueMemberS{Value: "2026-01-01T00:00:00Z"},
		"epoch":   &ddbtypes.AttributeValueMemberN{Value: "3"},
		"version": &ddbtypes.AttributeValueMemberS{Value: "not-a-number"}, // wrong DynamoDB type
	}
	if _, _, err := decode(item); err == nil {
		t.Fatal("expected a corruption error for a present-but-non-numeric version, got nil")
	}
}

// TestSaveRejectsUnencodableTime [#81]: encode() would RFC3339Nano-format a >9999 year to a string
// (e.g. "10001-01-01T00:00:00Z") that decode()'s time.Parse cannot read back, durably poisoning the
// item. Save must reject such a Time BEFORE any PutItem, so nothing is written.
func TestSaveRejectsUnencodableTime(t *testing.T) {
	f := &fakeAPI{getOut: &awsddb.GetItemOutput{}} // absent item
	s := New(f, "t", "ckpt#")
	bad := model.Watermark{Time: time.Date(10001, 1, 1, 0, 0, 0, 0, time.UTC), Epoch: 1}
	err := s.Save(context.Background(), model.CheckpointKey{SourceInstance: "i", Loop: "l", OutputFingerprint: "fp"}, bad)
	if !errors.Is(err, checkpoint.ErrUnencodable) || f.puts != 0 {
		t.Fatalf("year-10001 Save must be ErrUnencodable with zero puts, got err=%v puts=%d", err, f.puts)
	}
}

// TestEncodeDecodeRoundTripAtYearBound proves the guard threshold matches the backend's real
// capability: a year-9999 watermark (the max the guard allows) encodes and decodes back intact.
func TestEncodeDecodeRoundTripAtYearBound(t *testing.T) {
	w := model.Watermark{Time: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), Cursor: "c", Epoch: 4}
	item := encode("ckpt#x", w, 1)
	got, ver, err := decode(item)
	if err != nil || ver != 1 || !got.Time.Equal(w.Time) || got.Cursor != "c" || got.Epoch != 4 {
		t.Fatalf("year-9999 must round-trip; got wm=%+v ver=%d err=%v", got, ver, err)
	}
}

// TestDecodeMissingVersionOK: a MISSING version stays acceptable → (0, nil). It's an internal-token
// absence (legacy/migrated item), not corruption — Save() upgrades it via attribute_not_exists(version).
func TestDecodeMissingVersionOK(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		"time":  &ddbtypes.AttributeValueMemberS{Value: "2026-01-01T00:00:00Z"},
		"epoch": &ddbtypes.AttributeValueMemberN{Value: "3"},
	}
	wm, ver, err := decode(item)
	if err != nil || ver != 0 || wm.Epoch != 3 {
		t.Fatalf("missing version must decode to (epoch=3, ver=0, nil); got (epoch=%d, ver=%d, %v)", wm.Epoch, ver, err)
	}
}
