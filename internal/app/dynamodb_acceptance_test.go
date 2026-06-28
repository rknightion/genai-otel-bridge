// SPDX-License-Identifier: AGPL-3.0-only

//go:build acceptance

// DynamoDB HA acceptance gate (DESIGN §9, ECS target). Run:
// `DYNAMODB_ENDPOINT=http://localhost:8000 go test -tags acceptance ./internal/app/ -run TestDynamoDB`.
// Exercises the REAL DynamoDB coordinator + checkpoint against dynamodb-local: failover bumps the
// monotonic fence epoch, and the checkpoint fence then rejects a demoted leader's late write (no
// double-advance) — the ECS-path equivalent of TestFailoverHandoffIsContiguous. Skips if
// DYNAMODB_ENDPOINT is unset so a bare `-tags acceptance` run without docker stays green.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	cpddb "github.com/rknightion/genai-otel-bridge/internal/checkpoint/dynamodb"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	ddbcoord "github.com/rknightion/genai-otel-bridge/internal/coordinate/dynamodb"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func ddbAcceptanceClient(t *testing.T) *awsddb.Client {
	t.Helper()
	ep := os.Getenv("DYNAMODB_ENDPOINT")
	if ep == "" {
		t.Skip("DYNAMODB_ENDPOINT unset; skipping dynamodb-local acceptance test")
	}
	// A shell-active AWS_PROFILE would otherwise hijack the static dummy creds; isolate to dynamodb-local.
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_PROFILE", "")
	cfg, err := awscfg.LoadDefaultConfig(context.Background(),
		awscfg.WithRegion("us-east-1"),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("dummy", "dummy", "")),
		awscfg.WithSharedConfigFiles([]string{}),
		awscfg.WithSharedCredentialsFiles([]string{}),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	return awsddb.NewFromConfig(cfg, func(o *awsddb.Options) { o.BaseEndpoint = aws.String(ep) })
}

func ddbAcceptanceTable(t *testing.T, db *awsddb.Client) string {
	t.Helper()
	name := fmt.Sprintf("acc-%d", time.Now().UnixNano())
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &awsddb.CreateTableInput{
		TableName:            aws.String(name),
		BillingMode:          ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: ddbtypes.ScalarAttributeTypeS}},
		KeySchema:            []ddbtypes.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: ddbtypes.KeyTypeHash}},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &awsddb.DeleteTableInput{TableName: aws.String(name)})
	})
	return name
}

func TestDynamoDBFailoverNoDoubleAdvance(t *testing.T) {
	db := ddbAcceptanceClient(t)
	table := ddbAcceptanceTable(t, db)
	store := cpddb.New(db, table, "ckpt#")
	key := model.CheckpointKey{SourceInstance: "portkey-acc", Loop: "analytics", OutputFingerprint: "fp"}
	t0 := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	mk := func(id string) *ddbcoord.Coordinator {
		return ddbcoord.New(db, table, "lock#leader", id, 1500*time.Millisecond, 700*time.Millisecond, 150*time.Millisecond)
	}

	// Leader A elected → record its epoch and write the first checkpoint at that epoch.
	ctxA, cancelA := context.WithCancel(context.Background())
	epochA := make(chan int64, 1)
	go func() {
		_ = mk("node-a").Run(ctxA, func(lc context.Context) {
			epochA <- coordinate.EpochFromContext(lc)
			<-lc.Done()
		})
	}()
	var ea int64
	select {
	case ea = <-epochA:
	case <-time.After(10 * time.Second):
		t.Fatal("node-a was never elected")
	}
	if err := store.Save(context.Background(), key, model.Watermark{Time: t0, Epoch: ea}); err != nil {
		t.Fatalf("leader A save: %v", err)
	}

	// B contends; takes over only after A steps down, with a STRICTLY GREATER fence epoch.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	epochB := make(chan int64, 1)
	go func() {
		_ = mk("node-b").Run(ctxB, func(lc context.Context) {
			epochB <- coordinate.EpochFromContext(lc)
			<-lc.Done()
		})
	}()
	cancelA()
	var eb int64
	select {
	case eb = <-epochB:
	case <-time.After(10 * time.Second):
		t.Fatal("node-b never took over after node-a stepped down")
	}
	if eb <= ea {
		t.Fatalf("failover epoch %d not > previous %d (fence must advance on transition)", eb, ea)
	}

	// New leader B advances the checkpoint at the higher epoch.
	if err := store.Save(context.Background(), key, model.Watermark{Time: t0.Add(time.Hour), Epoch: eb}); err != nil {
		t.Fatalf("leader B save: %v", err)
	}
	// The demoted leader A's late write carries the OLD epoch — it MUST be fenced out (no double-advance)
	// even though its Time is further forward. This is the clock-independent safety property.
	err := store.Save(context.Background(), key, model.Watermark{Time: t0.Add(2 * time.Hour), Epoch: ea})
	if !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("demoted-leader late write err=%v, want ErrStaleWrite (epoch fence must reject it)", err)
	}
}
