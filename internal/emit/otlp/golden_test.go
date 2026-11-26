// SPDX-License-Identifier: Apache-2.0

package otlp

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// update rewrites the committed golden bytes. It is never on by default: run
// `go test ./internal/emit/otlp/ -run TestEncodeGolden -update` and review the diff of testdata/.
var update = flag.Bool("update", false, "rewrite the golden OTLP bodies in testdata/")

// legacyIdentity is the three-attribute product identity of a legacy install
// (config.IdentityConfig.ProductIdentity), written out so the test does not import config.
var legacyIdentity = map[string]string{
	"service.name":                "genai-otel-bridge",
	"service.namespace":           "genai-otel-bridge",
	"deployment.environment.name": "dev",
}

// TestEncodeGolden [unit 1, G6] compares the uncompressed OTLP bodies of one fixed metrics batch and
// one fixed logs batch with committed bytes. Re-emitting a settled batch must be byte-identical across
// versions, so a change to the encoding (field order, attribute order, an added resource attribute)
// fails here and must be a conscious decision.
func TestEncodeGolden(t *testing.T) {
	ts := time.Unix(1_700_000_040, 0).UTC()
	samples := []model.Sample{
		{Name: "portkey_api_requests", Kind: model.Gauge, Unit: "1", Labels: map[string]string{"workspace": "w1", "ai_model": "m1"}, Timestamp: ts, Value: 12},
		{Name: "portkey_api_requests", Kind: model.Gauge, Unit: "1", Labels: map[string]string{"workspace": "w1", "ai_model": "m2"}, Timestamp: ts, Value: 7.5},
		{Name: "portkey_api_cost", Kind: model.Gauge, Unit: "USD", Timestamp: ts.Add(time.Minute), Value: 3.25},
		{Name: "portkey_api_latency", Kind: model.Gauge, Unit: "ms", Labels: map[string]string{"quantile": "0.95"}, Timestamp: ts, Value: 840},
	}
	logs := []model.LogRecord{
		{
			Timestamp: ts, Severity: "INFO",
			IndexedAttributes: map[string]string{"ai_org": "org1"},
			RecordAttributes:  map[string]string{"status": "200", "request_id": "r-1"},
			TraceID:           []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		},
		{
			Timestamp: ts.Add(time.Second), Severity: "ERROR",
			IndexedAttributes: map[string]string{"ai_org": "org1"},
			RecordAttributes:  map[string]string{"status": "500"},
		},
		{
			Timestamp: ts.Add(2 * time.Second), Severity: "WARN",
			IndexedAttributes: map[string]string{"ai_org": "org2"},
			RecordAttributes:  map[string]string{"status": "429"},
		},
	}
	metricsBody, err := Encode(legacyIdentity, samples)
	if err != nil {
		t.Fatal(err)
	}
	logsBody, err := EncodeLogs(legacyIdentity, logs)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file string
		got  []byte
	}{{"metrics.pb", metricsBody}, {"logs.pb", logsBody}} {
		path := filepath.Join("testdata", c.file)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, c.got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
		}
		if !bytes.Equal(c.got, want) {
			t.Errorf("%s: encoded body differs from the committed golden (%d bytes, want %d); first difference at byte %d",
				c.file, len(c.got), len(want), firstDiff(c.got, want))
		}
	}
}

func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
