// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"bytes"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func sample(name string, labels map[string]string, ts time.Time, val float64) model.Sample {
	return model.Sample{Name: name, Kind: model.Gauge, Labels: labels, Value: val, Timestamp: ts}
}

// TestEncodeDeterministicUnderMapShuffle covers the conditional-idempotency precondition
// (DESIGN §4.5/§8, F30, Cdx-H14). Two inputs with differing map iteration order AND slice order
// must produce byte-identical output. The test uses:
//   - multiple metric names
//   - multiple buckets (different timestamps) per name
//   - varied float values
//   - identity map with reversed key insertion order
//   - sample slice in reversed order
func TestEncodeDeterministicUnderMapShuffle(t *testing.T) {
	ts1 := time.Unix(1_700_000_000, 0).UTC()
	ts2 := time.Unix(1_700_000_060, 0).UTC()
	ts3 := time.Unix(1_700_000_120, 0).UTC()

	// identity maps with reversed key insertion order — Go randomises map iteration
	id1 := map[string]string{"service.namespace": "decant", "deployment.environment.name": "dev", "region": "us-west-2"}
	id2 := map[string]string{"region": "us-west-2", "deployment.environment.name": "dev", "service.namespace": "decant"}

	// input 1: samples in forward order, labels in one insertion order
	s1 := []model.Sample{
		sample("portkey_api_requests", map[string]string{"a": "1", "b": "2"}, ts1, 10.5),
		sample("portkey_api_requests", map[string]string{"a": "1", "b": "2"}, ts2, 11.0),
		sample("portkey_api_requests", map[string]string{"a": "1", "b": "2"}, ts3, 12.75),
		sample("portkey_api_cost_usd", map[string]string{"b": "2", "a": "1"}, ts1, 0.003),
		sample("portkey_api_cost_usd", map[string]string{"b": "2", "a": "1"}, ts2, 0.006),
		sample("portkey_api_tokens", map[string]string{"c": "x"}, ts1, 500.0),
	}

	// input 2: samples in reversed order, labels in reversed insertion order — both maps must
	// produce the same sorted KVs and the encoder must produce the same series ordering
	s2 := []model.Sample{
		sample("portkey_api_tokens", map[string]string{"c": "x"}, ts1, 500.0),
		sample("portkey_api_cost_usd", map[string]string{"a": "1", "b": "2"}, ts2, 0.006),
		sample("portkey_api_cost_usd", map[string]string{"a": "1", "b": "2"}, ts1, 0.003),
		sample("portkey_api_requests", map[string]string{"b": "2", "a": "1"}, ts3, 12.75),
		sample("portkey_api_requests", map[string]string{"b": "2", "a": "1"}, ts2, 11.0),
		sample("portkey_api_requests", map[string]string{"b": "2", "a": "1"}, ts1, 10.5),
	}

	b1, err := Encode(id1, s1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Encode(id2, s2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("encode not byte-identical under map/resource shuffle (breaks conditional idempotency, C2)")
	}
}

func TestEncodeRejectsDelta(t *testing.T) {
	s := []model.Sample{{Name: "x", Kind: model.Sum, Temporality: model.Delta, Value: 1}}
	if _, err := Encode(nil, s); err == nil {
		t.Fatal("expected rejection of Delta temporality for the GC OTLP target")
	}
}
