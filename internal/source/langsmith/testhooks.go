// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// SetLoopClockForTest overrides a loop's wall-clock so acceptance tests (in another package) can
// drive deterministic snapshots. Returns false if lp is not a langsmith sessions loop. TEST-ONLY
// seam — the production clock is UTC time.Now (set in newSource); nothing in prod calls this.
func SetLoopClockForTest(lp source.Loop, now func() time.Time) bool {
	sl, ok := lp.(*sessionsLoop)
	if !ok {
		return false
	}
	sl.now = now
	return true
}
