// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

func baseAnalyticsCfg() config.SourceConfig {
	return config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1",
		SourceInstance: "portkey-test",
		Auth:           config.AuthConfig{Header: "x-portkey-api-key", Value: "tok"},
		RateLimit:      config.RateLimitConfig{RPS: 1, Burst: 3},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Window: config.Duration(50 * time.Minute), Graphs: []string{"requests"},
			BucketSettle: config.Duration(10 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
			MaxBackfill: config.Duration(90 * time.Minute),
		}},
	}
}

// One analytics loop instance regardless of use-case count (ownership-safe); passes carry the filters.
func TestAnalyticsOneInstanceNPasses(t *testing.T) {
	t.Run("empty ⇒ one instance, one unlabelled pass", func(t *testing.T) {
		src, err := New(baseAnalyticsCfg(), source.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if len(src.Loops()) != 1 {
			t.Fatalf("want 1 loop, got %d", len(src.Loops()))
		}
		al := src.Loops()[0].(*analyticsLoop)
		if len(al.passes) != 1 || al.passes[0].slug != "" {
			t.Fatalf("want one unlabelled pass, got %+v", al.passes)
		}
	})
	t.Run("two use-cases ⇒ still ONE instance, two passes", func(t *testing.T) {
		cfg := baseAnalyticsCfg()
		cfg.APIKeyUseCases = []config.APIKeyUseCase{
			{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
			{Label: "Content Gen", APIKeyIDs: []string{"uuid-b"}},
		}
		src, err := New(cfg, source.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if len(src.Loops()) != 1 { // NOT fan-out — ownership requires one Key
			t.Fatalf("want 1 analytics instance, got %d", len(src.Loops()))
		}
		al := src.Loops()[0].(*analyticsLoop)
		if len(al.passes) != 2 || al.passes[0].slug != "data_gen" || al.passes[0].apiKeyIDsCSV != "uuid-a" {
			t.Fatalf("passes wrong: %+v", al.passes)
		}
	})
	t.Run("legacy settings.api_key_ids preserved when no use-cases", func(t *testing.T) {
		cfg := baseAnalyticsCfg()
		lc := cfg.Loops["analytics"]
		lc.Settings = map[string]string{"api_key_ids": "legacy-uuid"}
		cfg.Loops["analytics"] = lc
		src, _ := New(cfg, source.Deps{})
		al := src.Loops()[0].(*analyticsLoop)
		if len(al.passes) != 1 || al.passes[0].apiKeyIDsCSV != "legacy-uuid" || al.passes[0].slug != "" {
			t.Fatalf("legacy pass wrong: %+v", al.passes)
		}
	})
}

// Key() must be byte-identical regardless of use-cases (one watermark; no reset).
func TestAnalyticsKeyStableAcrossUseCases(t *testing.T) {
	plain, _ := New(baseAnalyticsCfg(), source.Deps{})
	cfg := baseAnalyticsCfg()
	cfg.APIKeyUseCases = []config.APIKeyUseCase{{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}}}
	withUC, _ := New(cfg, source.Deps{})
	if plain.Loops()[0].Key().String() != withUC.Loops()[0].Key().String() {
		t.Fatalf("analytics Key changed with use-cases: %s vs %s", plain.Loops()[0].Key(), withUC.Loops()[0].Key())
	}
}

// TestAnalyticsCollectStampsUseCaseAndFilters: e2e test using the real fakePortkey httptest server
// and clock injection. Verifies that Collect with a use-case:
//  1. stamps api_key_use_case="data_gen" on every emitted sample
//  2. sends api_key_ids=uuid-a in the fetch URL
func TestAnalyticsCollectStampsUseCaseAndFilters(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute)

	// Capture request URLs so we can assert api_key_ids was sent.
	var mu sync.Mutex
	var recordedURLs []string

	// Build a fake server that records requests and returns settled data.
	var dps []dataPoint
	for i := 1; i <= 6; i++ {
		dps = append(dps, dataPoint{Timestamp: tAt(base, i), Total: float64(i)})
	}
	bodies := map[string]graphResponse{"requests": {DataPoints: dps}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recordedURLs = append(recordedURLs, r.URL.RequestURI())
		mu.Unlock()
		graph := r.URL.Path[len("/analytics/graphs/"):]
		b, ok := bodies[graph]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(b)
	}))
	defer srv.Close()

	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: srv.URL, SourceInstance: "pk-uc-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"analytics": {
			Enabled: true, Cadence: config.Duration(time.Minute), Window: config.Duration(50 * time.Minute),
			BucketSettle: config.Duration(3 * time.Minute), BootstrapLookback: config.Duration(50 * time.Minute),
			MaxBackfill: config.Duration(55 * time.Minute), MetricPrefix: "portkey_api",
			Graphs: []string{"requests"},
		}},
		APIKeyUseCases: []config.APIKeyUseCase{
			{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
		},
	}

	src, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Loops()) != 1 {
		t.Fatalf("want 1 analytics loop, got %d", len(src.Loops()))
	}
	al := src.Loops()[0].(*analyticsLoop)
	al.now = func() time.Time { return now }

	b, err := al.Collect(context.Background(), model.Watermark{Time: base})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// startSemantics=true, settle=3m, now=base+10m → settled cutoff = base+7m.
	// buckets base+1..base+6 (end = ts+1m = base+2m..base+7m) → 6 samples with api_key_use_case stamped.
	if len(b.Samples) == 0 {
		t.Fatalf("expected >0 samples, got 0")
	}
	for _, s := range b.Samples {
		if s.Labels[useCaseLabelKey] != "data_gen" {
			t.Errorf("sample %q missing api_key_use_case=data_gen: labels=%v", s.Name, s.Labels)
		}
	}

	// Assert the fetch URL contained api_key_ids=uuid-a.
	mu.Lock()
	captured := append([]string(nil), recordedURLs...)
	mu.Unlock()
	if len(captured) == 0 {
		t.Fatal("no requests reached the fake server")
	}
	found := false
	for _, u := range captured {
		if strings.Contains(u, "api_key_ids=uuid-a") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no request contained api_key_ids=uuid-a; recorded URLs: %v", captured)
	}
}
