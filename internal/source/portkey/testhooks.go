// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"time"

	"github.com/grafana-ps/aip-oi/internal/source"
)

// SetLoopClockForTest overrides a loop's wall-clock so acceptance tests (in another package) can
// drive deterministic windows. Returns false if lp is not a portkey analytics loop. TEST-ONLY seam —
// the production clock is UTC time.Now (set in New); nothing in prod calls this.
func SetLoopClockForTest(lp source.Loop, now func() time.Time) bool {
	al, ok := lp.(*analyticsLoop)
	if !ok {
		return false
	}
	al.now = now
	return true
}
