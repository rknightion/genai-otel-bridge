// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func TestReviewGuardSanitizeConcurrentRace(t *testing.T) {
	g := NewGuard(GuardConfig{PerSeriesBudget: 100_000})
	batch := func(name string) model.Batch {
		return model.Batch{Samples: []model.Sample{{
			Name:      name,
			Kind:      model.Gauge,
			Value:     1,
			Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		}}}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range []string{"series_a", "series_b"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 1000; i++ {
				g.Sanitize(batch(name))
			}
		}()
	}
	close(start)
	wg.Wait()
}

// TestReviewGuardSanitizeLogsConcurrentRace [ext-review-8 / #134] extends the shared-state race guard to
// the LOGS plane and the mixed Sanitize+SanitizeLogs shape that is the real production topology since the
// logs loops shipped. okLog mutates the SAME g.seen map under g.mu from every logs loop runner; the
// original race test raced only the samples Sanitize path. Records carry an ALLOW-LISTED indexed
// attribute with varying (overlapping) values so okLog actually reaches the seen-map insert/lookup
// branches (a label-free or denied record short-circuits before the budget path). Under -race this fails
// (concurrent map read/write) if the g.mu lock in okLog is removed or narrowed.
func TestReviewGuardSanitizeLogsConcurrentRace(t *testing.T) {
	g := NewGuard(GuardConfig{PerSeriesBudget: 100_000, AllowLabelKeys: []string{"ai_model", "quantile"}})

	sample := func(series, q string) model.Batch {
		return model.Batch{Samples: []model.Sample{{
			Name:      series,
			Kind:      model.Gauge,
			Labels:    map[string]string{"quantile": q},
			Value:     1,
			Timestamp: time.Unix(1_700_000_000, 0).UTC(),
		}}}
	}
	logRec := func(m string) []model.LogRecord {
		return []model.LogRecord{{IndexedAttributes: map[string]string{"ai_model": m}}}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Two logs loops mutate okLog's per-loop seen buckets ("logs:x"/"logs:y") concurrently; overlapping
	// indexed values exercise BOTH the new-insert and already-seen branches.
	for _, loop := range []string{"x", "y"} {
		loop := loop
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 1000; i++ {
				g.SanitizeLogs(loop, logRec("m"+strconv.Itoa(i%8)))
			}
		}()
	}
	// Two samples loops race Sanitize on overlapping series with an allow-listed label set, so the
	// samples seen-map path runs concurrently with the logs seen-map path on the one shared mutex.
	for _, series := range []string{"series_a", "series_b"} {
		series := series
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 1000; i++ {
				g.Sanitize(sample(series, "p"+strconv.Itoa(i%8)))
			}
		}()
	}
	close(start)
	wg.Wait()
}
