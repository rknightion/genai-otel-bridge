// SPDX-License-Identifier: Apache-2.0

package tenancy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxSecretBytes bounds one secret file. An API key or an OTLP token is a few hundred bytes; a file past
// this is refused unread rather than held in memory.
const maxSecretBytes = 64 << 10

// DirSecretReader is the file-backed SecretReader (design §2, §4): Read(tenant, file) reads
// <root>/<tenant>/<file> and trims surrounding white space, as the legacy file: resolver does. tenant and
// file must each be one bare path element (no separator, not . or .., no NUL), so one tenant's read cannot
// reach another tenant's directory or leave root; config.Validate refuses such names at load, and the
// reader refuses them again. A file that is missing, unreadable, empty after trimming or larger than
// 64 KiB is an error. Every error names the tenant and the file and never carries a value; it wraps the
// underlying cause (fs.ErrNotExist, say) without the path.
func DirSecretReader(root string) SecretReader {
	return dirSecretReader{root: root}
}

type dirSecretReader struct{ root string }

func (d dirSecretReader) Read(tenant, file string) (string, error) {
	fail := func(cause error) (string, error) {
		return "", fmt.Errorf("tenant %q: secret file %q: %w", tenant, file, cause)
	}
	if err := bareElement(tenant); err != nil {
		return fail(fmt.Errorf("the tenant id %w", err))
	}
	if err := bareElement(file); err != nil {
		return fail(fmt.Errorf("the file name %w", err))
	}
	f, err := os.Open(filepath.Join(d.root, tenant, file))
	if err != nil {
		return fail(withoutPath(err))
	}
	defer func() { _ = f.Close() }()
	// One byte past the limit tells a file at the limit from a longer one.
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return fail(withoutPath(err))
	}
	if len(b) > maxSecretBytes {
		return fail(fmt.Errorf("larger than %d bytes", maxSecretBytes))
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return fail(errors.New("empty"))
	}
	return v, nil
}

// bareElement refuses a name that is not one path element.
func bareElement(name string) error {
	switch {
	case name == "":
		return errors.New("is empty")
	case name == "." || name == "..":
		return fmt.Errorf("must not be %q", name)
	case strings.ContainsAny(name, "/\\\x00"):
		return errors.New("must be a bare name, with no path separator or NUL")
	}
	return nil
}

// withoutPath unwraps a *fs.PathError to its cause, so the message names no path (the caller names the
// tenant and the file) while errors.Is still matches the cause.
func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
