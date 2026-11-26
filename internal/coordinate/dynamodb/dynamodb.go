// SPDX-License-Identifier: Apache-2.0

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

// New builds a Coordinator. lease > renewDeadline > retry > 0 (validated upstream by config.Validate,
// which rejects retry_period >= renew_deadline — a retry cadence at/above the deadline guarantees a
// recurring split-brain window — and enforces the full lease > renew_deadline ordering).
func New(db UpdateAPI, table, pk, identity string, lease, renewDeadline, retry time.Duration) *Coordinator {
	return &Coordinator{db: db, table: table, pk: pk, identity: identity, lease: lease, renewDeadline: renewDeadline, retry: retry, now: time.Now}
}

func (c *Coordinator) Run(ctx context.Context, onElected func(leaderCtx context.Context)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// [#377] Read before acquire's own clock read, so the first watchdog deadline (start +
		// renew_deadline) can never fall after the expiry the acquire writes (its now + lease).
		start := time.Now()
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
		c.lead(ctx, fence, start, onElected)
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
//
// The `attribute_not_exists(expiresAtMs)` clause is a self-heal: a normally-written lock ALWAYS has
// expiresAtMs (acquire+renew both SET it), but a hand-seeded/partially-written/corrupt item could have
// pk without expiresAtMs — and `expiresAtMs < :now` is FALSE for a missing attribute, which would
// otherwise wedge leadership forever. Treating a missing expiresAtMs as acquirable lets the coordinator
// recover; it can't cause double-acquire because no live leader writes pk without expiresAtMs.
//
// The `holder = :me` clause [#84] handles a committed-but-error-reported acquire: acquire's UpdateItem
// is non-idempotent (ADD fence, SET expiresAtMs), so if attempt 1 commits but its response is lost
// (transient network fault), the SDK's transport retry hits an item that is now fresh+unexpired and NOT
// ours-by-holder — without this clause that ConditionalCheckFailed maps to "held by a live leader" and
// the process is locked out of ITS OWN lock until the self-inflicted lease expires (up to lease_duration
// with no leader). Re-acquiring our own item is safe: single-active-replica holds (lead() only returns
// after the drain barrier, so a same-process re-acquire never overlaps a live scheduler) and the extra
// `ADD fence :one` keeps the epoch strictly monotonic. If a DIFFERENT replica has taken the lock, holder
// is theirs (not :me), so this clause does not match and expiry still gates the takeover.
func (c *Coordinator) acquire(ctx context.Context) (fence int64, acquired bool, err error) {
	now := c.now()
	exp := now.Add(c.lease).UnixMilli()
	out, err := c.db.UpdateItem(ctx, &awsddb.UpdateItemInput{
		TableName:           aws.String(c.table),
		Key:                 map[string]ddbtypes.AttributeValue{"pk": sstr(c.pk)},
		ConditionExpression: aws.String("attribute_not_exists(pk) OR attribute_not_exists(expiresAtMs) OR expiresAtMs < :now OR holder = :me"),
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
// acquiredAt is when the acquire request that won the lock started; the first renew deadline is
// measured from it [#377].
func (c *Coordinator) lead(ctx context.Context, fence int64, acquiredAt time.Time, onElected func(context.Context)) {
	leaderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); onElected(coordinate.WithEpoch(leaderCtx, fence)) }()

	// [#30] Watchdog: step down (cancel leaderCtx) if renewDeadline elapses with no successful renew,
	// INDEPENDENTLY of whether a renew UpdateItem is currently blocked. Enforcing the deadline only in the
	// ticker branch below is unreachable while renew() blocks — a silent-connection / blackholed-endpoint
	// UpdateItem never returns to the loop (aws-sdk-go-v2's default client sets no request timeout), so the
	// in-flight single-emit fence (leaderCtx cancellation, DESIGN F11) would never fire → an unbounded
	// dual-leader window after a standby has already acquired with fence+1. The watchdog runs in its own
	// goroutine so the deadline is honoured regardless; cancelling leaderCtx also aborts the in-flight
	// UpdateItem (net/http honours request-ctx cancellation), so a hung renew unblocks. client-go parity:
	// leaderelection wraps every renew in context.WithTimeout(RenewDeadline). Fence/CAS semantics are
	// untouched — this only adds an out-of-band deadline (see #30 acceptance).
	//
	// [#377] The deadline is measured from when the last successful request STARTED, not from when its
	// response arrived. The lock expiry a request writes is its start + lease, so measuring from the
	// response let a slow successful renew (one taking longer than lease - renew_deadline) push step-down
	// past the expiry, leaving the old leader's leaderCtx alive after a standby had legitimately acquired.
	// client-go likewise starts its RenewDeadline timeout before the request.
	renewOK := make(chan time.Time, 1)
	watchdogDone := make(chan struct{})
	go c.renewWatchdog(leaderCtx, cancel, acquiredAt, renewOK, watchdogDone)

	ticker := time.NewTicker(c.retry)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-watchdogDone
			<-done // [round3 HIGH-1 parity] barrier on onElected drain before returning
			return
		case <-done:
			cancel()
			<-watchdogDone
			return // scheduler exited on its own
		case <-leaderCtx.Done():
			// The watchdog stepped us down (renew deadline exceeded). Drain onElected before returning so
			// app.Run cannot re-enter a new acquire while a prior term's emit worker is still running.
			<-watchdogDone
			<-done
			return
		case <-ticker.C:
			start := time.Now() // before renew's own clock read; see the acquire start in Run
			ok, err := c.renew(leaderCtx, fence)
			switch {
			case err == nil && ok:
				// Signal the successful renew's start to the watchdog. This loop is the only sender, so
				// after dropping an unread older start the send cannot block; the newest start wins.
				select {
				case <-renewOK:
				default:
				}
				renewOK <- start
			case err == nil && !ok:
				slog.Info("dynamodb coordinator: leadership lost (lock taken)", "identity", c.identity)
				cancel()
				<-watchdogDone
				<-done
				return
			default: // transient error — the watchdog enforces the renew deadline out-of-band
				slog.Warn("dynamodb coordinator: renew failed (transient); watchdog enforces renew deadline", "err", err)
			}
		}
	}
}

// renewWatchdog cancels leaderCtx once c.renewDeadline has passed since the start of the last
// successful request: the acquire (acquiredAt) or a renew whose start arrives on renewOK. A deadline
// already past (a request slower than renew_deadline) steps down at once. It runs in its own goroutine
// (the fix for #30) so the deadline is enforced even while a renew UpdateItem is blocked. It exits when
// leaderCtx is cancelled — by itself here, by the parent ctx, or by the lead loop on a lost/relinquished
// lease. The timer and the start times use the real monotonic clock like the renew ticker, not the
// injectable c.now (see setNow), which only feeds the expiresAtMs written to the table.
func (c *Coordinator) renewWatchdog(leaderCtx context.Context, cancel context.CancelFunc, acquiredAt time.Time, renewOK <-chan time.Time, done chan<- struct{}) {
	defer close(done)
	timer := time.NewTimer(time.Until(acquiredAt.Add(c.renewDeadline)))
	defer timer.Stop()
	for {
		select {
		case <-leaderCtx.Done():
			return
		case start := <-renewOK:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Until(start.Add(c.renewDeadline)))
		case <-timer.C:
			slog.Warn("dynamodb coordinator: renew deadline exceeded; stepping down (watchdog)",
				"identity", c.identity, "renew_deadline", c.renewDeadline)
			cancel()
			return
		}
	}
}

var _ coordinate.Coordinator = (*Coordinator)(nil)
