// SPDX-License-Identifier: AGPL-3.0-only

// Command gen renders the telemetry signal catalogue from each emitting package's Signals() and
// splices it into docs/telemetry.md between the generated markers. Run via `make generate`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rknightion/genai-otel-bridge/internal/docs/gen/render"
	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	langsmith "github.com/rknightion/genai-otel-bridge/internal/source/langsmith"
	portkey "github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

// AllSignals is the single collection point — keep in sync with the gate test (telemetry_generated_test.go).
func AllSignals() []signal.Signal {
	var s []signal.Signal
	s = append(s, portkey.Signals()...)
	s = append(s, langsmith.Signals()...)
	s = append(s, selfobs.Signals()...)
	return s
}

func main() {
	path := flag.String("telemetry", "docs/telemetry.md", "path to the telemetry doc to splice")
	flag.Parse()

	cur, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	out, err := render.Splice(cur, render.Catalogue(AllSignals()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*path, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	fmt.Printf("gen: wrote telemetry catalogue to %s\n", *path)
}
