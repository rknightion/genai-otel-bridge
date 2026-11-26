// SPDX-License-Identifier: Apache-2.0

package tenancy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeSecret writes root/tenant/file with content, creating its directories.
func writeSecret(t *testing.T, root, tenant, file, content string) {
	t.Helper()
	p := filepath.Join(root, tenant, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDirSecretReaderReadsTenantFile (#532): Read(tenant, file) returns <root>/<tenant>/<file> with
// surrounding white space trimmed, as the legacy file: resolver trims; each tenant reads its own
// directory; a Kubernetes-style symlinked key (file -> ..data/file) is followed.
func TestDirSecretReaderReadsTenantFile(t *testing.T) {
	root := t.TempDir()
	writeSecret(t, root, "team-a", "source-key", "  key-of-team-a\n")
	writeSecret(t, root, "team-b", "source-key", "key-of-team-b")
	writeSecret(t, root, "team-c", "..data/otlp-token", "\ttoken-of-team-c\r\n")
	if err := os.Symlink("..data/otlp-token", filepath.Join(root, "team-c", "otlp-token")); err != nil {
		t.Fatal(err)
	}
	r := DirSecretReader(root)
	for _, c := range []struct{ tenant, file, want string }{
		{"team-a", "source-key", "key-of-team-a"},
		{"team-b", "source-key", "key-of-team-b"},
		{"team-c", "otlp-token", "token-of-team-c"},
	} {
		got, err := r.Read(c.tenant, c.file)
		if err != nil || got != c.want {
			t.Errorf("Read(%q, %q) = %q, %v; want %q", c.tenant, c.file, got, err, c.want)
		}
	}
}

// TestDirSecretReaderErrorsNameTenantAndFileOnly (#532, fails closed): every way a read can fail is an
// error naming the tenant and the file, and no error carries a secret value. A missing file matches
// fs.ErrNotExist; an empty file, one over the size bound, a directory, and a tenant or file name that is
// not one bare path element are errors too, so none of them is ever used as a key or a token. A name that
// would escape the tenant's directory is refused before anything is opened: the file it points at holds
// a value that must not come back.
func TestDirSecretReaderErrorsNameTenantAndFileOnly(t *testing.T) {
	root := t.TempDir()
	const value = "secret-value-that-must-never-appear"
	writeSecret(t, root, "team-a", "blank", " \n\t ")
	writeSecret(t, root, "team-a", "huge", value+strings.Repeat("x", maxSecretBytes))
	writeSecret(t, root, "team-b", "source-key", value)
	writeSecret(t, root, "team-a", "dir/inner", value)
	if err := os.WriteFile(filepath.Join(root, "outside"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	r := DirSecretReader(root)
	cases := []struct {
		name, tenant, file string
		notExist           bool
	}{
		{"missing file", "team-a", "source-key", true},
		{"missing tenant directory", "team-z", "source-key", true},
		{"empty after trimming", "team-a", "blank", false},
		{"over the size bound", "team-a", "huge", false},
		{"a directory", "team-a", "dir", false},
		{"file name with a separator", "team-a", "../team-b/source-key", false},
		{"file name with a backslash", "team-a", "..\\outside", false},
		{"file name ..", "team-a", "..", false},
		{"file name .", "team-a", ".", false},
		{"empty file name", "team-a", "", false},
		{"file name with NUL", "team-a", "source-key\x00", false},
		{"tenant ..", "..", "outside", false},
		{"tenant with a separator", "team-a/../team-b", "source-key", false},
		{"empty tenant", "", "outside", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Read(c.tenant, c.file)
			if err == nil {
				t.Fatalf("Read(%q, %q) = %q, want an error", c.tenant, c.file, got)
			}
			if got != "" {
				t.Errorf("Read returned a value %q with its error", got)
			}
			msg := err.Error()
			if !strings.Contains(msg, "tenant "+strconv.Quote(c.tenant)) || !strings.Contains(msg, "secret file "+strconv.Quote(c.file)) {
				t.Errorf("error %q must name tenant %q and file %q", msg, c.tenant, c.file)
			}
			if strings.Contains(msg, value) || strings.Contains(msg, "xxxx") {
				t.Errorf("error %q carries a secret value", msg)
			}
			if strings.Contains(msg, root) {
				t.Errorf("error %q carries the secrets root path; it names tenant and file only", msg)
			}
			if c.notExist != errors.Is(err, fs.ErrNotExist) {
				t.Errorf("errors.Is(err, fs.ErrNotExist) = %v, want %v (err %v)", !c.notExist, c.notExist, err)
			}
		})
	}
}
