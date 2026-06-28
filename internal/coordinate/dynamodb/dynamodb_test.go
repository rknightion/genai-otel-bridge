// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeUpdate struct {
	err  error
	out  *awsddb.UpdateItemOutput
	last *awsddb.UpdateItemInput // captured for assertions
}

func (f *fakeUpdate) UpdateItem(_ context.Context, in *awsddb.UpdateItemInput, _ ...func(*awsddb.Options)) (*awsddb.UpdateItemOutput, error) {
	f.last = in
	return f.out, f.err
}

func TestAcquireConditionalFailIsNotAcquired(t *testing.T) {
	c := New(&fakeUpdate{err: &ddbtypes.ConditionalCheckFailedException{}}, "t", "lock#l", "me", time.Second, time.Second, time.Second)
	_, ok, err := c.acquire(context.Background())
	if ok || err != nil {
		t.Fatalf("conditional-fail acquire: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestAcquireTransientErrorPropagates(t *testing.T) {
	sentinel := errors.New("throttled")
	c := New(&fakeUpdate{err: sentinel}, "t", "lock#l", "me", time.Second, time.Second, time.Second)
	_, ok, err := c.acquire(context.Background())
	if ok || !errors.Is(err, sentinel) {
		t.Fatalf("transient acquire: ok=%v err=%v, want ok=false err=throttled", ok, err)
	}
}

func TestRenewConditionalFailMeansLost(t *testing.T) {
	c := New(&fakeUpdate{err: &ddbtypes.ConditionalCheckFailedException{}}, "t", "lock#l", "me", time.Second, time.Second, time.Second)
	ok, err := c.renew(context.Background(), 1)
	if ok || err != nil {
		t.Fatalf("renew conditional-fail: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// TestAcquireUsesInjectedClockForExpiry deterministically checks expiresAtMs = now + lease (no docker).
func TestAcquireUsesInjectedClockForExpiry(t *testing.T) {
	fixed := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	f := &fakeUpdate{out: &awsddb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
		"fence": &ddbtypes.AttributeValueMemberN{Value: "1"},
	}}}
	c := New(f, "t", "lock#l", "me", 15*time.Second, 10*time.Second, 2*time.Second)
	c.setNow(func() time.Time { return fixed })
	fence, ok, err := c.acquire(context.Background())
	if !ok || err != nil || fence != 1 {
		t.Fatalf("acquire: fence=%d ok=%v err=%v, want 1/true/nil", fence, ok, err)
	}
	got := f.last.ExpressionAttributeValues[":exp"].(*ddbtypes.AttributeValueMemberN).Value
	want := strconv.FormatInt(fixed.Add(15*time.Second).UnixMilli(), 10)
	if got != want {
		t.Fatalf("expiresAtMs=%s, want %s (now+lease)", got, want)
	}
}
