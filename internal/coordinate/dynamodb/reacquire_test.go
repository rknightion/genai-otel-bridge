// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// storeFake emulates just enough DynamoDB conditional-write semantics for the acquire/renew condition
// expressions so a test can prove the `holder = :me` re-acquire clause (#84). It reads the ACTUAL
// ConditionExpression the coordinator sends, so reverting the fix (dropping the clause) makes the test
// fail. failNextErr models a committed-but-lost response (the write lands, the SDK sees a transport error).
type storeFake struct {
	exists      bool
	holder      string
	expiresAtMs int64
	fence       int64
	failNextErr error
}

func (s *storeFake) UpdateItem(_ context.Context, in *awsddb.UpdateItemInput, _ ...func(*awsddb.Options)) (*awsddb.UpdateItemOutput, error) {
	cond := *in.ConditionExpression
	vals := in.ExpressionAttributeValues
	getN := func(k string) int64 {
		n, _ := strconv.ParseInt(vals[k].(*ddbtypes.AttributeValueMemberN).Value, 10, 64)
		return n
	}
	me := vals[":me"].(*ddbtypes.AttributeValueMemberS).Value

	if strings.HasPrefix(cond, "attribute_not_exists(pk)") { // acquire
		now := getN(":now")
		acquirable := !s.exists || s.expiresAtMs < now
		if strings.Contains(cond, "holder = :me") && s.exists && s.holder == me {
			acquirable = true // #84: re-take our own committed lock
		}
		if !acquirable {
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}
		s.exists = true
		s.holder = me
		s.expiresAtMs = getN(":exp")
		s.fence += getN(":one")
		if s.failNextErr != nil { // committed, but the response is lost
			err := s.failNextErr
			s.failNextErr = nil
			return nil, err
		}
		return &awsddb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{"fence": nstr(s.fence)}}, nil
	}

	// renew: holder = :me AND fence = :fence
	if s.exists && s.holder == me && s.fence == getN(":fence") {
		s.expiresAtMs = getN(":exp")
		return &awsddb.UpdateItemOutput{}, nil
	}
	return nil, &ddbtypes.ConditionalCheckFailedException{}
}

// TestAcquireReacquiresOwnCommittedButErroredWrite: attempt 1 commits (holder=me, fence bumped, fresh
// expiry) but returns a transport error; the retry must re-acquire immediately via the `holder = :me`
// clause instead of misreading its own fresh item as "held by a live leader" and waiting out the lease.
// Fence stays strictly monotonic across the re-acquire. [#84 acceptance #1 + #2]
func TestAcquireReacquiresOwnCommittedButErroredWrite(t *testing.T) {
	fixed := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	s := &storeFake{failNextErr: errors.New("connection reset by peer")}
	c := New(s, "t", "lock#l", "me", 15*time.Second, 10*time.Second, 2*time.Second)
	c.setNow(func() time.Time { return fixed })

	// Attempt 1: commits under the hood, then reports a transient error.
	if _, ok, err := c.acquire(context.Background()); ok || err == nil {
		t.Fatalf("attempt 1: ok=%v err=%v, want ok=false + transient error (committed-but-lost response)", ok, err)
	}
	if s.fence != 1 || s.holder != "me" {
		t.Fatalf("attempt 1 must have committed the write: fence=%d holder=%q, want 1/\"me\"", s.fence, s.holder)
	}

	// Attempt 2 (the retry): the item is fresh+unexpired and NOT expiry-acquirable — only `holder = :me`
	// lets us re-take our own lock.
	fence, ok, err := c.acquire(context.Background())
	if !ok || err != nil {
		t.Fatalf("attempt 2: ok=%v err=%v, want an immediate re-acquire (holder = :me clause missing?)", ok, err)
	}
	if fence != 2 {
		t.Fatalf("re-acquire fence=%d, want 2 (strictly monotonic across the re-acquire)", fence)
	}
}

// TestAcquireStillBlockedByLiveForeignHolder guards the safety side of #84: a fresh, unexpired lock held
// by a DIFFERENT replica is NOT acquirable (holder != :me and not expired), so single-active-replica holds.
func TestAcquireStillBlockedByLiveForeignHolder(t *testing.T) {
	fixed := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	s := &storeFake{exists: true, holder: "other", fence: 5, expiresAtMs: fixed.Add(15 * time.Second).UnixMilli()}
	c := New(s, "t", "lock#l", "me", 15*time.Second, 10*time.Second, 2*time.Second)
	c.setNow(func() time.Time { return fixed })
	if _, ok, err := c.acquire(context.Background()); ok || err != nil {
		t.Fatalf("acquire against a live foreign holder: ok=%v err=%v, want ok=false err=nil (blocked)", ok, err)
	}
	if s.fence != 5 {
		t.Fatalf("a blocked acquire must not bump fence: got %d, want 5", s.fence)
	}
}
