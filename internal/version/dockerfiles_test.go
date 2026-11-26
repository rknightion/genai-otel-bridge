// SPDX-License-Identifier: Apache-2.0

package version

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The production, Kaniko and e2e helper images must stay on the same pinned golang builder and
// distroless runtime (AGENTS.md, "Runtime trust"). Renovate manages all three files, so a bump
// that lands in only some of them is caught here.

var fromRef = regexp.MustCompile(`(?m)^FROM\s+(?:--\S+\s+)*(\S+)`)

var dockerfiles = []string{
	"Dockerfile",
	"Dockerfile.kaniko",
	"test/e2e/harness/Dockerfile",
}

// baseRefs returns the image references of every FROM line in the repo-root-relative file.
func baseRefs(t *testing.T, rel string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var refs []string
	for _, m := range fromRef.FindAllStringSubmatch(string(b), -1) {
		refs = append(refs, m[1])
	}
	return refs
}

func TestDockerfileBasesAligned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker string
	}{
		{"golang builder", "/library/golang:"},
		{"distroless runtime", "distroless/static-debian13:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first, firstFile string
			for _, f := range dockerfiles {
				var found []string
				for _, ref := range baseRefs(t, f) {
					if strings.Contains(ref, tc.marker) {
						found = append(found, ref)
					}
				}
				if len(found) != 1 {
					t.Fatalf("%s: want exactly one %s FROM line, got %v", f, tc.name, found)
				}
				if !strings.Contains(found[0], "@sha256:") {
					t.Errorf("%s: %s is not pinned by digest: %s", f, tc.name, found[0])
				}
				if first == "" {
					first, firstFile = found[0], f
					continue
				}
				if found[0] != first {
					t.Errorf("%s: %s differs from %s:\n  %s\n  %s", f, tc.name, firstFile, found[0], first)
				}
			}
		})
	}
}
