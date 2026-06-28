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
