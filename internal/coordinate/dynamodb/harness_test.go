// SPDX-License-Identifier: AGPL-3.0-only

package dynamodb

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// newTestClient returns a real *dynamodb.Client pointed at dynamodb-local, or skips the test if
// DYNAMODB_ENDPOINT is unset (so `make test` without docker stays green; CI + OrbStack set it).
func newTestClient(t *testing.T) *awsddb.Client {
	t.Helper()
	ep := os.Getenv("DYNAMODB_ENDPOINT")
	if ep == "" {
		t.Skip("DYNAMODB_ENDPOINT unset; skipping dynamodb-local integration test")
	}
	// Unset AWS_PROFILE so the SDK does not try to load a named profile from ~/.aws/config —
	// we use static dummy credentials and point the client at dynamodb-local.
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_PROFILE", "")
	cfg, err := awscfg.LoadDefaultConfig(context.Background(),
		awscfg.WithRegion("us-east-1"),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("dummy", "dummy", "")),
		awscfg.WithSharedConfigFiles(nil),      // skip ~/.aws/config so no profile lookup occurs
		awscfg.WithSharedCredentialsFiles(nil), // skip ~/.aws/credentials likewise
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	return awsddb.NewFromConfig(cfg, func(o *awsddb.Options) { o.BaseEndpoint = aws.String(ep) })
}

// createTable makes a uniquely-named pk-only table and registers teardown.
func createTable(t *testing.T, db *awsddb.Client) string {
	t.Helper()
	name := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &awsddb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: ddbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &awsddb.DeleteTableInput{TableName: aws.String(name)})
	})
	// dynamodb-local CreateTable is synchronous (ACTIVE immediately); no waiter needed.
	return name
}
