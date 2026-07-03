// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestLeadershipStoppedEvent covers the three OnStoppedLeading cases client-go conflates [#74]:
// a never-elected standby exiting on ctx cancel, a cleanly SIGTERM'd leader, and a genuine renewal lapse.
func TestLeadershipStoppedEvent(t *testing.T) {
	cases := []struct {
		name         string
		everLed      bool
		ctxErr       error
		wantLvl      slog.Level
		wantContains string
		wantAbsent   string // must NOT appear (e.g. a standby/shutdown must not say "leadership lost")
	}{
		{"never-elected standby, ctx cancelled", false, context.Canceled, slog.LevelDebug, "without ever leading", "leadership lost"},
		{"cleanly shut-down leader", true, context.Canceled, slog.LevelInfo, "released on shutdown", "leadership lost"},
		{"genuine renewal lapse", true, nil, slog.LevelWarn, "leadership lost", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvl, msg := leadershipStoppedEvent(tc.everLed, tc.ctxErr)
			if lvl != tc.wantLvl {
				t.Errorf("level=%v, want %v", lvl, tc.wantLvl)
			}
			if !strings.Contains(msg, tc.wantContains) {
				t.Errorf("msg %q does not contain %q", msg, tc.wantContains)
			}
			if tc.wantAbsent != "" && strings.Contains(msg, tc.wantAbsent) {
				t.Errorf("msg %q must not contain %q", msg, tc.wantAbsent)
			}
		})
	}
}
