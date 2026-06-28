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

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/config/gen/helmgen"
	"github.com/rknightion/genai-otel-bridge/internal/source/langsmith"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

func main() {
	configSrc := flag.String("config", "internal/config/config.go", "path to the Go config source (for doc-comments)")
	valuesPath := flag.String("values", "deploy/helm/values.yaml", "path to the Helm values.yaml to splice")
	ecsConfigPath := flag.String("ecs-config", "deploy/ecs/terraform/config.example.yaml", "path to the generated ECS (DynamoDB-backed) config example")
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
	// The SAME example set feeds both the values.yaml examples region and the ECS config file below.
	examples := []helmgen.Example{
		{Value: portkey.ExampleSource(), SettingsComments: portkey.ExampleSettingsComments()},
		{Value: langsmith.ExampleSource(), SettingsComments: langsmith.ExampleSettingsComments()},
	}
	exBlock, err := helmgen.RenderExampleBlock(examples)
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

	// ECS deployment target: the full default config rendered under the DynamoDB-backed ECS profile
	// (helmgen.ECSProfile) PLUS the same commented all-loops source-examples (env-refs neutralized) —
	// a standalone file (no `config:` wrapper) the ECS Terraform module injects verbatim as
	// GENAI_OTEL_BRIDGE_CONFIG. Generated from the SAME config.Config schema + examples as values.yaml,
	// so it cannot drift; the package-config gate (TestECSConfigExampleUpToDate) enforces that.
	ecsConfig, err := helmgen.RenderECSConfigFile(reflect.TypeFor[config.Config](), *configSrc, examples)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*ecsConfigPath, ecsConfig, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen: write ECS config:", err)
		os.Exit(1)
	}
	fmt.Printf("gen: wrote generated ECS config to %s\n", *ecsConfigPath)
}
