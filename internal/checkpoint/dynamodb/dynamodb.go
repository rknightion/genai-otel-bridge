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

func (s *Store) pk(key model.CheckpointKey) string { return s.prefix + key.String() }

func nstr(n int64) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}

func sstr(v string) *ddbtypes.AttributeValueMemberS {
	return &ddbtypes.AttributeValueMemberS{Value: v}
}

func numAttr(m map[string]ddbtypes.AttributeValue, k string) (int64, error) {
	v, ok := m[k].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("checkpoint/dynamodb: attribute %q missing or not a number", k)
	}
	return strconv.ParseInt(v.Value, 10, 64)
}

func strAttr(m map[string]ddbtypes.AttributeValue, k string) (string, bool) {
	v, ok := m[k].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

func encode(pk string, w model.Watermark, version int64) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"pk":      sstr(pk),
		"time":    sstr(w.Time.UTC().Format(time.RFC3339Nano)), // string like configmap JSON — zero time round-trips
		"cursor":  sstr(w.Cursor),
		"epoch":   nstr(w.Epoch),
		"version": nstr(version),
	}
}

func decode(item map[string]ddbtypes.AttributeValue) (model.Watermark, int64, error) {
	ts, ok := strAttr(item, "time")
	if !ok {
		return model.Watermark{}, 0, fmt.Errorf("checkpoint/dynamodb: attribute \"time\" missing")
	}
	// "0001-01-01T00:00:00Z" parses back to a zero time (IsZero()==true) — required for the logs-export
	// cursor-only watermark (runner.go writes Time==zero with a Cursor on the first window).
	tm, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return model.Watermark{}, 0, fmt.Errorf("checkpoint/dynamodb: parse time %q: %w", ts, err)
	}
	ep, err := numAttr(item, "epoch")
	if err != nil {
		return model.Watermark{}, 0, err
	}
	// A missing `version` is an internal-token absence, NOT data corruption → default 0 (don't refuse Load).
	var ver int64
	if v, ok := item["version"].(*ddbtypes.AttributeValueMemberN); ok {
		if ver, err = strconv.ParseInt(v.Value, 10, 64); err != nil {
			return model.Watermark{}, 0, err
		}
	}
	cursor, _ := strAttr(item, "cursor")
	return model.Watermark{Time: tm.UTC(), Cursor: cursor, Epoch: ep}, ver, nil
}

func (s *Store) Load(ctx context.Context, key model.CheckpointKey) (model.Watermark, error) {
	out, err := s.db.GetItem(ctx, &awsddb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            map[string]ddbtypes.AttributeValue{"pk": sstr(s.pk(key))},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return model.Watermark{}, fmt.Errorf("checkpoint/dynamodb: get: %w", err)
	}
	if out.Item == nil {
		return model.Watermark{}, nil // absent ⇒ zero watermark
	}
	wm, _, err := decode(out.Item)
	if err != nil {
		return model.Watermark{}, fmt.Errorf("checkpoint/dynamodb: decode %s: %w", key, err) // never clobber
	}
	return wm, nil
}

func (s *Store) Save(ctx context.Context, key model.CheckpointKey, w model.Watermark) error {
	for attempt := 0; attempt < s.retries; attempt++ {
		out, err := s.db.GetItem(ctx, &awsddb.GetItemInput{
			TableName:      aws.String(s.table),
			Key:            map[string]ddbtypes.AttributeValue{"pk": sstr(s.pk(key))},
			ConsistentRead: aws.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("checkpoint/dynamodb: get: %w", err)
		}
		var stored model.Watermark
		var version int64
		exists := out.Item != nil
		versionPresent := false
		if exists {
			stored, version, err = decode(out.Item)
			if err != nil {
				return fmt.Errorf("checkpoint/dynamodb: decode %s: %w", key, err) // never clobber (CP-C10 parity)
			}
			_, versionPresent = out.Item["version"]
		}
		if err := checkpoint.CheckMonotonic(stored, w); err != nil {
			return err // ErrStaleWrite — benign to the caller
		}
		put := &awsddb.PutItemInput{TableName: aws.String(s.table), Item: encode(s.pk(key), w, version+1)}
		switch {
		case !exists:
			put.ConditionExpression = aws.String("attribute_not_exists(pk)")
		case versionPresent:
			put.ConditionExpression = aws.String("version = :v")
			put.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{":v": nstr(version)}
		default:
			// Item exists but predates the `version` attribute (hand-seeded / migrated). Condition on
			// its absence so this write upgrades the item rather than looping forever on `version = 0`.
			put.ConditionExpression = aws.String("attribute_not_exists(version)")
		}
		_, err = s.db.PutItem(ctx, put)
		if err == nil {
			return nil
		}
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			continue // concurrent writer moved version → re-read + retry
		}
		return fmt.Errorf("checkpoint/dynamodb: put: %w", err)
	}
	return fmt.Errorf("checkpoint/dynamodb: exhausted RMW retries for %s", key)
}

var _ checkpoint.Checkpointer = (*Store)(nil)
