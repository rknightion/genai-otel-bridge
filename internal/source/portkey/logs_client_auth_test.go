// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// TestStatusErrorIsErrAuth [#537]: every logs_export lifecycle call (create, start, poll, download-url,
// cancel) returns a *statusError, so 401/403 satisfy errors.Is(err, source.ErrAuth), even when wrapped; other
// statuses do not.
func TestStatusErrorIsErrAuth(t *testing.T) {
	for code, want := range map[int]bool{401: true, 403: true, 404: false, 429: false, 500: false} {
		err := fmt.Errorf("step: %w", &statusError{method: "GET", path: "/logs/exports/x", code: code})
		if got := errors.Is(err, source.ErrAuth); got != want {
			t.Errorf("status %d: errors.Is(ErrAuth)=%v want %v", code, got, want)
		}
	}
}
