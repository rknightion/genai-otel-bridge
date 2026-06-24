// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"bytes"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/grafana-ps/aip-oi/internal/model"
)

func logRec(body, sev string, indexed, record map[string]string, ts time.Time) model.LogRecord {
	return model.LogRecord{Timestamp: ts, Body: body, Severity: sev, IndexedAttributes: indexed, RecordAttributes: record}
}

// decodeLogs unmarshals EncodeLogs output. The encoder hand-wraps repeated ResourceLogs as field 1 of
// the request; LogsData also has field 1 = repeated ResourceLogs, so it decodes the same wire bytes
// without importing the collector package the encoder deliberately avoids.
func decodeLogs(t *testing.T, body []byte) *logspb.LogsData {
	t.Helper()
	var ld logspb.LogsData
	if err := proto.Unmarshal(body, &ld); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	return &ld
}

// TestEncodeLogsDeterministic: differing map iteration + slice order must produce byte-identical output
// (same conditional-idempotency precondition as metrics — a re-emitted chunk must be byte-stable).
func TestEncodeLogsDeterministic(t *testing.T) {
	ts1 := time.Unix(1_700_000_000, 0).UTC()
	ts2 := time.Unix(1_700_000_005, 0).UTC()
	id1 := map[string]string{"service.namespace": "aip-oi-meta", "deployment.environment.name": "dev"}
	id2 := map[string]string{"deployment.environment.name": "dev", "service.namespace": "aip-oi-meta"}
	l1 := []model.LogRecord{
		logRec("", "INFO", map[string]string{"ai_model": "gpt-5", "ai_provider": "openai"}, map[string]string{"trace_id": "t1", "cost": "0.1"}, ts1),
		logRec("", "INFO", map[string]string{"ai_provider": "openai", "ai_model": "gpt-5"}, map[string]string{"cost": "0.2", "trace_id": "t2"}, ts2),
		logRec("", "ERROR", map[string]string{"ai_model": "claude"}, map[string]string{"trace_id": "t3"}, ts1),
	}
	l2 := []model.LogRecord{l1[2], l1[0], l1[1]} // reversed-ish
	b1, err := EncodeLogs(id1, l1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := EncodeLogs(id2, l2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("EncodeLogs not byte-identical under map/slice shuffle")
	}
}

// TestEncodeLogsTraceID: a 16-byte LogRecord.TraceID is encoded onto the OTLP log record's trace_id
// field (native logs↔traces correlation); a nil/short TraceID leaves it empty.
func TestEncodeLogsTraceID(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	tid := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	recs := []model.LogRecord{
		{Timestamp: ts, RecordAttributes: map[string]string{"correlation_id": "x"}, TraceID: tid},
		{Timestamp: ts.Add(time.Second), RecordAttributes: map[string]string{"id": "y"}}, // no trace id
	}
	ld := decodeLogs(t, mustEncodeLogs(t, nil, recs))
	var withTID, withoutTID int
	for _, rl := range ld.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				if len(lr.TraceId) == 0 {
					withoutTID++
					continue
				}
				if !bytes.Equal(lr.TraceId, tid) {
					t.Fatalf("trace_id=%x want %x", lr.TraceId, tid)
				}
				withTID++
			}
		}
	}
	if withTID != 1 || withoutTID != 1 {
		t.Fatalf("withTID=%d withoutTID=%d, want 1/1", withTID, withoutTID)
	}
}

// TestEncodeLogsGroupsByIndexedAttrs: records are grouped into one ResourceLogs per distinct
// IndexedAttributes set (so GS1 can promote those to Loki stream labels); each resource carries
// identity ∪ that group's indexed attrs.
func TestEncodeLogsGroupsByIndexedAttrs(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	id := map[string]string{"service.namespace": "aip-oi-meta"}
	logs := []model.LogRecord{
		logRec("", "INFO", map[string]string{"ai_model": "gpt-5"}, nil, ts),
		logRec("", "INFO", map[string]string{"ai_model": "gpt-5"}, nil, ts),
		logRec("", "INFO", map[string]string{"ai_model": "claude"}, nil, ts),
	}
	ld := decodeLogs(t, mustEncodeLogs(t, id, logs))
	if len(ld.ResourceLogs) != 2 {
		t.Fatalf("ResourceLogs groups=%d want 2 (gpt-5, claude)", len(ld.ResourceLogs))
	}
	// Every resource must carry the identity attr; find the gpt-5 group and assert it holds 2 records.
	var gpt5Records int
	for _, rl := range ld.ResourceLogs {
		attrs := kvMap(rl.Resource.Attributes)
		if attrs["service.namespace"] != "aip-oi-meta" {
			t.Fatalf("resource missing identity attr: %v", attrs)
		}
		if attrs["ai_model"] == "gpt-5" {
			for _, sl := range rl.ScopeLogs {
				gpt5Records += len(sl.LogRecords)
			}
		}
	}
	if gpt5Records != 2 {
		t.Fatalf("gpt-5 group records=%d want 2", gpt5Records)
	}
}

// TestEncodeLogsAttributeMapping: IndexedAttributes→resource, RecordAttributes→log-record attrs,
// empty Body→nil, Severity→SeverityText, Timestamp→TimeUnixNano.
func TestEncodeLogsAttributeMapping(t *testing.T) {
	ts := time.Unix(1_700_000_123, 0).UTC()
	id := map[string]string{"service.namespace": "aip-oi-meta"}
	logs := []model.LogRecord{
		logRec("", "INFO", map[string]string{"ai_model": "gpt-5"}, map[string]string{"trace_id": "abc", "response_time": "1485"}, ts),
	}
	ld := decodeLogs(t, mustEncodeLogs(t, id, logs))
	if len(ld.ResourceLogs) != 1 {
		t.Fatalf("ResourceLogs=%d want 1", len(ld.ResourceLogs))
	}
	rl := ld.ResourceLogs[0]
	res := kvMap(rl.Resource.Attributes)
	if res["ai_model"] != "gpt-5" || res["service.namespace"] != "aip-oi-meta" {
		t.Fatalf("resource attrs wrong: %v", res)
	}
	if _, isContent := res["trace_id"]; isContent {
		t.Fatal("RecordAttributes must NOT land on the resource (only IndexedAttributes do)")
	}
	if len(rl.ScopeLogs) != 1 || len(rl.ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("want 1 scope/1 record, got %d scopes", len(rl.ScopeLogs))
	}
	lr := rl.ScopeLogs[0].LogRecords[0]
	rec := kvMap(lr.Attributes)
	if rec["trace_id"] != "abc" || rec["response_time"] != "1485" {
		t.Fatalf("record attrs wrong: %v", rec)
	}
	if rl.ScopeLogs[0].Scope == nil || rl.ScopeLogs[0].Scope.Name != scopeName {
		t.Fatalf("scope name not %q", scopeName)
	}
	if lr.Body != nil {
		t.Fatalf("empty Body must encode to nil (FR10 no content), got %v", lr.Body)
	}
	if lr.SeverityText != "INFO" {
		t.Fatalf("severity=%q want INFO", lr.SeverityText)
	}
	if lr.TimeUnixNano != uint64(ts.UnixNano()) {
		t.Fatalf("ts=%d want %d", lr.TimeUnixNano, ts.UnixNano())
	}
}

func mustEncodeLogs(t *testing.T, id map[string]string, logs []model.LogRecord) []byte {
	t.Helper()
	b, err := EncodeLogs(id, logs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func kvMap(kvs []*commonpb.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[kv.Key] = kv.Value.GetStringValue()
	}
	return m
}
