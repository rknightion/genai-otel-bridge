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
	return errors.New("not implemented") // filled in Lane A
}

var _ = slog.Info
var _ = strconv.Itoa
var _ = fmt.Sprintf
var _ = aws.String
var _ ddbtypes.AttributeValue
var _ coordinate.Coordinator = (*Coordinator)(nil)
