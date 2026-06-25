// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"bytes"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func TestReviewEncodeDeterministicAcrossUnits(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	a := model.Sample{Name: "same_name", Unit: "s", Kind: model.Gauge, Labels: map[string]string{"k": "v"}, Timestamp: ts, Value: 1}
	b := model.Sample{Name: "same_name", Unit: "ms", Kind: model.Gauge, Labels: map[string]string{"k": "v"}, Timestamp: ts, Value: 2}

	body1, err := Encode(nil, []model.Sample{a, b})
	if err != nil {
		t.Fatal(err)
	}
	body2, err := Encode(nil, []model.Sample{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body1, body2) {
		t.Fatal("Encode is not byte-deterministic when same-name samples differ only by unit")
	}
}

func TestReviewEncodeDeterministicAcrossLabelKeyCollision(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	a := model.Sample{Name: "same_name", Kind: model.Gauge, Labels: map[string]string{"a": "b;c=d"}, Timestamp: ts, Value: 1}
	b := model.Sample{Name: "same_name", Kind: model.Gauge, Labels: map[string]string{"a": "b", "c": "d"}, Timestamp: ts, Value: 2}

	body1, err := Encode(nil, []model.Sample{a, b})
	if err != nil {
		t.Fatal(err)
	}
	body2, err := Encode(nil, []model.Sample{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body1, body2) {
		t.Fatal("Encode is not byte-deterministic when labelKey collisions preserve input order")
	}
}
