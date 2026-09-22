// SPDX-License-Identifier: Apache-2.0

package portkey

import (
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// SetLoopClockForTest overrides a loop's wall-clock so acceptance tests (in another package) can
// drive deterministic windows. Returns false if lp is not a supported Portkey loop. TEST-ONLY seam —
// the production clock is UTC time.Now (set in New); nothing in prod calls this.
func SetLoopClockForTest(lp source.Loop, now func() time.Time) bool {
	switch l := lp.(type) {
	case *analyticsLoop:
		l.now = now
	case *logsExportLoop:
		l.now = now
	default:
		return false
	}
	return true
}
