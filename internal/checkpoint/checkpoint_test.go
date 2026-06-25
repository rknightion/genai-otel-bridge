// SPDX-License-Identifier: AGPL-3.0-only

package checkpoint

import (
	"errors"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func wm(sec, epoch int64) model.Watermark {
	return model.Watermark{Time: time.Unix(sec, 0).UTC(), Epoch: epoch}
}

func wmc(sec, epoch int64, cursor string) model.Watermark {
	return model.Watermark{Time: time.Unix(sec, 0).UTC(), Cursor: cursor, Epoch: epoch}
}

func TestCheckMonotonic(t *testing.T) {
	cases := []struct {
		name             string
		stored, incoming model.Watermark
		wantStale        bool
	}{
		{"forward same epoch", wm(100, 1), wm(160, 1), false},
		{"equal time = no progress", wm(100, 1), wm(100, 1), true},
		{"backward time", wm(100, 1), wm(40, 1), true},
		{"forward higher epoch (new leader)", wm(100, 1), wm(160, 2), false},
		{"forward but stale epoch (demoted leader)", wm(100, 2), wm(160, 1), true},
		{"first write over zero", model.Watermark{}, wm(100, 1), false},
		// Cursor relaxation (logs-export job state machine): a same-Time write that ADVANCES the cursor
		// is accepted (the in-flight window's job state stepped forward); Time still must not regress and
		// the epoch fence still wins.
		{"same time, cursor advances (job step)", wmc(100, 1, "phaseA"), wmc(100, 1, "phaseB"), false},
		{"same time, cursor unchanged = no progress", wmc(100, 1, "phaseA"), wmc(100, 1, "phaseA"), true},
		{"backward time rejected despite cursor change", wmc(100, 1, "phaseA"), wmc(40, 1, "phaseB"), true},
		{"stale epoch rejected despite cursor change", wmc(100, 2, "phaseA"), wmc(100, 1, "phaseB"), true},
		{"forward time with cursor change accepted", wmc(100, 1, "phaseA"), wmc(160, 1, "phaseB"), false},
	}
	for _, c := range cases {
		err := CheckMonotonic(c.stored, c.incoming)
		if c.wantStale && !errors.Is(err, ErrStaleWrite) {
			t.Errorf("%s: want ErrStaleWrite, got %v", c.name, err)
		}
		if !c.wantStale && err != nil {
			t.Errorf("%s: want accept, got %v", c.name, err)
		}
	}
}
