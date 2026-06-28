// SPDX-License-Identifier: AGPL-3.0-only

// Package dynamodb implements checkpoint.Checkpointer over DynamoDB items. Save does GetItem →
// checkpoint.CheckMonotonic (the single fence) → conditional PutItem (optimistic-concurrency on a
// `version` attribute), mirroring the ConfigMap RMW exactly.
package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// API is the subset of *dynamodb.Client this store needs (fakeable in tests).
type API interface {
	GetItem(ctx context.Context, in *awsddb.GetItemInput, optFns ...func(*awsddb.Options)) (*awsddb.GetItemOutput, error)
	PutItem(ctx context.Context, in *awsddb.PutItemInput, optFns ...func(*awsddb.Options)) (*awsddb.PutItemOutput, error)
}

// Store implements checkpoint.Checkpointer.
type Store struct {
	db      API
	table   string
	prefix  string // <keyPrefix>ckpt#
	retries int
}

// New builds a Store. prefix is "<keyPrefix>ckpt#".
func New(db API, table, prefix string) *Store {
	return &Store{db: db, table: table, prefix: prefix, retries: 5}
}

func (s *Store) Load(ctx context.Context, key model.CheckpointKey) (model.Watermark, error) {
	return model.Watermark{}, errors.New("not implemented") // Lane B
}

func (s *Store) Save(ctx context.Context, key model.CheckpointKey, w model.Watermark) error {
	return errors.New("not implemented") // Lane B
}

var _ = fmt.Sprintf
var _ = strconv.Itoa
var _ = time.Now
var _ = aws.String
var _ ddbtypes.AttributeValue
var _ = checkpoint.CheckMonotonic
var _ checkpoint.Checkpointer = (*Store)(nil)
