// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
)

// docExample names a documented example config and how to find it: the first fenced yaml block that
// starts with startsWith (and contains contains, when set). A block that only holds the sources: tree
// gets docsBaseConfig prepended. Every listed document MUST yield a block: a miss fails the test, so
// nothing is skipped by a silent pattern miss. Add a document here to guard it; a deliberately partial
// block is simply not selected by these rules.
type docExample struct {
	path        string
	startsWith  string
	contains    string
	prependBase bool
}

var docExamples = []docExample{
	{path: "README.md", startsWith: "emit:"},
	{path: "docs/installation.md", startsWith: "emit:"},
	{path: "docs/portkey.md", startsWith: "sources:", contains: "analytics:", prependBase: true},
	{path: "docs/configuration.md", startsWith: "sources:", contains: "analytics:", prependBase: true},
}

const docsBaseConfig = `emit:
  telemetry:
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  service_namespace: genai-otel-bridge
  deployment_environment: docs-test
ha: {coordinator: none, checkpoint: file}
queue: {emit_workers: 1}
`

var (
	docsFenceRe  = regexp.MustCompile("(?s)```yaml\\n(.*?)\\n```")
	docsEnvRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// extractDocYAML returns the first fenced yaml block of body that matches ex, or "" when none does.
func extractDocYAML(body string, ex docExample) string {
	for _, m := range docsFenceRe.FindAllStringSubmatch(body, -1) {
		blk := m[1]
		if strings.HasPrefix(blk, ex.startsWith) && strings.Contains(blk, ex.contains) {
			return blk
		}
	}
	return ""
}

func TestDocsExampleConfigsBuild(t *testing.T) {
	for _, ex := range docExamples {
		t.Run(ex.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", ex.path))
			if err != nil {
				t.Fatalf("%s: %v", ex.path, err)
			}
			blk := extractDocYAML(string(raw), ex)
			if blk == "" {
				t.Fatalf("%s: no fenced yaml block starting with %q containing %q", ex.path, ex.startsWith, ex.contains)
			}
			if ex.prependBase {
				blk = docsBaseConfig + blk
			}
			for _, m := range docsEnvRefRe.FindAllStringSubmatch(blk, -1) {
				v := "x"
				if strings.Contains(m[1], "ENDPOINT") {
					v = "https://placeholder.example"
				}
				t.Setenv(m[1], v)
			}
			// The checkpoint path of the example is relative; keep it inside the test's temp dir.
			p := filepath.Join(t.TempDir(), "cfg.yaml")
			if err := os.WriteFile(p, []byte(blk), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := ValidateConfigFile(p); err != nil {
				t.Fatalf("%s: ValidateConfigFile: %v", ex.path, err)
			}
			cfg, err := config.Load(p)
			if err != nil {
				t.Fatalf("%s: config.Load: %v", ex.path, err)
			}
			cp, err := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
			if err != nil {
				t.Fatal(err)
			}
			ef, tf := fixedFactories(noopEmitter{}, schedule.NoopMetrics{})
			if _, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, ef, tf, Options{}); err != nil {
				t.Fatalf("%s: Build: %v", ex.path, err)
			}
		})
	}
}

// A listed document that yields no matching block must be a failure, not a silent skip.
func TestDocsExampleExtractionMissIsDetected(t *testing.T) {
	ex := docExample{path: "x.md", startsWith: "emit:"}
	if got := extractDocYAML("no fences here\n```yaml\nsources:\n  - a\n```\n", ex); got != "" {
		t.Fatalf("expected no block, got %q", got)
	}
	if got := extractDocYAML("```yaml\nemit:\n  x: 1\n```\n", ex); got != "emit:\n  x: 1" {
		t.Fatalf("expected the emit block, got %q", got)
	}
}
