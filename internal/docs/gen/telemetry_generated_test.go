// SPDX-License-Identifier: AGPL-3.0-only

// TestTelemetryDocUpToDate is the telemetry drift gate: it re-runs the catalogue render in-memory
// (via AllSignals from main.go) and byte-compares it against the committed generated region in
// docs/telemetry.md. A new/changed signal descriptor changes the bytes and fails this test until
// `make generate` is re-run and committed — the forcing function that keeps docs in lockstep with code.
package main

import (
	"os"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/docs/gen/render"
)

func TestTelemetryDocUpToDate(t *testing.T) {
	const path = "../../../docs/telemetry.md" // internal/docs/gen -> repo root
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got, err := render.Extract(cur)
	if err != nil {
		t.Fatalf("extract region: %v", err)
	}
	want := render.Catalogue(AllSignals())
	if string(got) != string(want) {
		t.Fatalf("docs/telemetry.md is stale — run `make generate` and commit.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}
