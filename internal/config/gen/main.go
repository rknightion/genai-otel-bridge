// SPDX-License-Identifier: AGPL-3.0-only

// Command gen renders the Helm chart's default config block from the Go config schema and splices it
// into deploy/helm/values.yaml (between the BEGIN/END generated-config markers). Run via `make
// generate` (which runs it from the module root, so the default relative paths resolve).
//
// The render logic lives in internal/config/gen/helmgen so the in-package gate test
// (internal/config/helm_generated_test.go) can reuse it with no import cycle. This main only owns
// the config.Config type binding and file I/O.
package main

import (
	"flag"
	"fmt"
	"os"
	"reflect"

	"github.com/grafana-ps/aip-oi/internal/config"
	"github.com/grafana-ps/aip-oi/internal/config/gen/helmgen"
	"github.com/grafana-ps/aip-oi/internal/source/langsmith"
	"github.com/grafana-ps/aip-oi/internal/source/portkey"
)

func main() {
	configSrc := flag.String("config", "internal/config/config.go", "path to the Go config source (for doc-comments)")
	valuesPath := flag.String("values", "deploy/helm/values.yaml", "path to the Helm values.yaml to splice")
	flag.Parse()

	block, err := helmgen.RenderConfigBlock(reflect.TypeFor[config.Config](), *configSrc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	cur, err := os.ReadFile(*valuesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen: read values.yaml:", err)
		os.Exit(1)
	}
	out, err := helmgen.SpliceConfigBlock(cur, block)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	// Source examples region: per-source-type example configs (commented-out), rendered from each
	// source package's ExampleSource(). Vendor-specific example shape stays in the vendor package.
	exBlock, err := helmgen.RenderExampleBlock([]helmgen.Example{
		{Value: portkey.ExampleSource(), SettingsComments: portkey.ExampleSettingsComments()},
		{Value: langsmith.ExampleSource(), SettingsComments: langsmith.ExampleSettingsComments()},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	out, err = helmgen.SpliceExampleBlock(out, exBlock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*valuesPath, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen: write values.yaml:", err)
		os.Exit(1)
	}
	fmt.Printf("gen: wrote generated config + source-examples blocks to %s\n", *valuesPath)
}
