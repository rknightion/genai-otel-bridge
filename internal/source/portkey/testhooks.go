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
	case *groupsLoop:
		l.now = now
	default:
		return false
	}
	return true
}

// SetSourceClockForTest overrides the wall-clock of a Portkey source: its scope assertion's (when it has
// a scope) and each of its loops'. Returns false if src is not a Portkey source or holds a loop
// SetLoopClockForTest does not support. TEST-ONLY seam, like SetLoopClockForTest.
func SetSourceClockForTest(src source.Source, now func() time.Time) bool {
	var ps *portkeySource
	switch s := src.(type) {
	case *portkeySource:
		ps = s
	case *scopedSource:
		s.now = now
		ps = s.portkeySource
	default:
		return false
	}
	for _, lp := range ps.loops {
		if !SetLoopClockForTest(lp, now) {
			return false
		}
	}
	return true
}
