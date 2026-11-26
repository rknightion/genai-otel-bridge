// SPDX-License-Identifier: Apache-2.0

package source

import (
	"errors"
	"fmt"
	"testing"
)

// TestWrapAuth [#537]: 401 and 403 wrap ErrAuth and keep the original error; other codes pass through.
func TestWrapAuth(t *testing.T) {
	base := errors.New("vendor: status")
	for _, code := range []int{401, 403} {
		err := WrapAuth(code, fmt.Errorf("ctx: %w", base))
		if !errors.Is(err, ErrAuth) || !errors.Is(err, base) {
			t.Errorf("code %d: want ErrAuth and the original error in the chain, got %v", code, err)
		}
	}
	for _, code := range []int{200, 404, 429, 500} {
		if err := WrapAuth(code, base); err != base || errors.Is(err, ErrAuth) {
			t.Errorf("code %d: want the error unchanged, got %v", code, err)
		}
	}
}
