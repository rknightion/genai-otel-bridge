// SPDX-License-Identifier: Apache-2.0

package langsmith

import (
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// SetLoopClockForTest overrides a loop's wall-clock so acceptance tests (in another package) can
// drive deterministic snapshots. Returns false if lp is not a langsmith sessions or usage loop. TEST-ONLY
// seam — the production clock is UTC time.Now (set in newSource); nothing in prod calls this.
func SetLoopClockForTest(lp source.Loop, now func() time.Time) bool {
	switch l := lp.(type) {
	case *sessionsLoop:
		l.now = now
	case *usageLoop:
		l.now = now
	default:
		return false
	}
	return true
}
