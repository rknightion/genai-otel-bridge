// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
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
