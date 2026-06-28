// SPDX-License-Identifier: AGPL-3.0-only

// Package dynamodb implements coordinate.Coordinator using a single DynamoDB lock item.
// A conditional-write CAS acquires/renews the lease; a monotonic `fence` attribute is the leader
// epoch (the LeaseTransitions analogue), read once at election and threaded via coordinate.WithEpoch.
package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
)

// UpdateAPI is the subset of *dynamodb.Client this coordinator needs (fakeable in tests).
type UpdateAPI interface {
	UpdateItem(ctx context.Context, in *awsddb.UpdateItemInput, optFns ...func(*awsddb.Options)) (*awsddb.UpdateItemOutput, error)
}

// Clock returns the current time; injectable for deterministic tests.
type Clock func() time.Time

// Coordinator implements coordinate.Coordinator over a DynamoDB lock item.
type Coordinator struct {
	db            UpdateAPI
	table         string
	pk            string // full partition key: <keyPrefix>lock#<lockName>
	identity      string
	lease         time.Duration // LeaseDuration: how long a renew extends the lock
	renewDeadline time.Duration // give up leadership if no successful renew within this
	retry         time.Duration // attempt cadence (acquire poll + renew attempts)
	now           Clock
}

// New builds a Coordinator. lease > renewDeadline > 0 and retry > 0 (validated upstream).
func New(db UpdateAPI, table, pk, identity string, lease, renewDeadline, retry time.Duration) *Coordinator {
	return &Coordinator{db: db, table: table, pk: pk, identity: identity, lease: lease, renewDeadline: renewDeadline, retry: retry, now: time.Now}
}

func (c *Coordinator) Run(ctx context.Context, onElected func(leaderCtx context.Context)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fence, acquired, err := c.acquire(ctx)
		if err != nil {
			slog.Warn("dynamodb coordinator: acquire error", "err", err)
		}
		if !acquired {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.retry):
				continue
			}
		}
		c.lead(ctx, fence, onElected)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func numAttr(m map[string]ddbtypes.AttributeValue, k string) (int64, error) {
	v, ok := m[k].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("dynamodb: attribute %q missing or not a number", k)
	}
	return strconv.ParseInt(v.Value, 10, 64)
}

func nstr(n int64) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}

func sstr(s string) *ddbtypes.AttributeValueMemberS {
	return &ddbtypes.AttributeValueMemberS{Value: s}
}

// setNow injects a deterministic clock (test-only). The renew/retry TIMERS still use real time — the
// integration tests use generous real-time margins; this only makes the expiresAtMs computation testable.
func (c *Coordinator) setNow(f Clock) { c.now = f }

// acquire takes an empty/expired lock with a conditional UpdateItem, bumping `fence` (the epoch) by 1.
// The lock item is intentionally NOT given a DynamoDB TTL: auto-deletion of the lock would reset `fence`
// to 1 on the next acquire, below a surviving checkpoint's epoch, permanently fencing all writes after a
// long full outage. Expiry is by the `expiresAtMs` comparison only; the item persists (parity with the
// K8s Lease, which is never auto-deleted either).
func (c *Coordinator) acquire(ctx context.Context) (fence int64, acquired bool, err error) {
	now := c.now()
	exp := now.Add(c.lease).UnixMilli()
	out, err := c.db.UpdateItem(ctx, &awsddb.UpdateItemInput{
		TableName:           aws.String(c.table),
		Key:                 map[string]ddbtypes.AttributeValue{"pk": sstr(c.pk)},
		ConditionExpression: aws.String("attribute_not_exists(pk) OR expiresAtMs < :now"),
		UpdateExpression:    aws.String("SET holder = :me, expiresAtMs = :exp ADD fence :one"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now": nstr(now.UnixMilli()), ":me": sstr(c.identity),
			":exp": nstr(exp), ":one": nstr(1),
		},
		ReturnValues: ddbtypes.ReturnValueUpdatedNew,
	})
	if err != nil {
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return 0, false, nil // held by a live leader
		}
		return 0, false, err // transient
	}
	fence, err = numAttr(out.Attributes, "fence")
	if err != nil {
		return 0, false, err
	}
	return fence, true, nil
}

// renew extends the lease iff we still hold it (holder + fence match). It never bumps `fence`.
func (c *Coordinator) renew(ctx context.Context, fence int64) (bool, error) {
	now := c.now()
	exp := now.Add(c.lease).UnixMilli()
	_, err := c.db.UpdateItem(ctx, &awsddb.UpdateItemInput{
		TableName:           aws.String(c.table),
		Key:                 map[string]ddbtypes.AttributeValue{"pk": sstr(c.pk)},
		ConditionExpression: aws.String("holder = :me AND fence = :fence"),
		UpdateExpression:    aws.String("SET expiresAtMs = :exp"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":me": sstr(c.identity), ":fence": nstr(fence), ":exp": nstr(exp),
		},
	})
	if err != nil {
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return false, nil // lost: holder/fence changed
		}
		return false, err // transient
	}
	return true, nil
}

// lead runs the leadership term: spawn onElected, renew on a ticker, cancel + drain-barrier on loss.
func (c *Coordinator) lead(ctx context.Context, fence int64, onElected func(context.Context)) {
	leaderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); onElected(coordinate.WithEpoch(leaderCtx, fence)) }()

	ticker := time.NewTicker(c.retry)
	defer ticker.Stop()
	lastRenew := c.now()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done // [round3 HIGH-1 parity] barrier on onElected drain before returning
			return
		case <-done:
			return // scheduler exited on its own
		case <-ticker.C:
			ok, err := c.renew(leaderCtx, fence)
			switch {
			case err == nil && ok:
				lastRenew = c.now()
			case err == nil && !ok:
				slog.Info("dynamodb coordinator: leadership lost (lock taken)", "identity", c.identity)
				cancel()
				<-done
				return
			default: // transient error
				if c.now().Sub(lastRenew) >= c.renewDeadline {
					slog.Warn("dynamodb coordinator: renew deadline exceeded; stepping down", "err", err)
					cancel()
					<-done
					return
				}
			}
		}
	}
}

var _ coordinate.Coordinator = (*Coordinator)(nil)
