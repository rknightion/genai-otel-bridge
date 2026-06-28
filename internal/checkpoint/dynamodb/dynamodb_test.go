// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"testing"
	"time"

	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

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
