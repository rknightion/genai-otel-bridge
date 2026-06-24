// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/config"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// TestGroupsOneInstanceNPasses asserts that configuring two api_key_use_cases produces ONE groups loop
// instance with two passes (not two separate loop instances) — required by M7 ownership invariant
// (SeriesDeclarer loops may only emit a given metric name from one Key()).
func TestGroupsOneInstanceNPasses(t *testing.T) {
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1", SourceInstance: "portkey-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "tok"},
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 3},
		Loops:     map[string]config.LoopConfig{"groups": {Enabled: true}},
		APIKeyUseCases: []config.APIKeyUseCase{
			{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
			{Label: "Content Gen", APIKeyIDs: []string{"uuid-b"}},
		},
	}
	src, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Loops()) != 1 {
		t.Fatalf("want 1 groups instance, got %d", len(src.Loops()))
	}
	gl := src.Loops()[0].(*groupsLoop)
	if len(gl.passes) != 2 || gl.passes[0].slug != "data_gen" || gl.passes[1].apiKeyIDsCSV != "uuid-b" {
		t.Fatalf("passes wrong: %+v", gl.passes)
	}
}

// TestGroupsKeyUnchangedWithUseCases asserts that the groups loop Key() is byte-identical regardless
// of whether api_key_use_cases is set. One Key = one watermark = no reset on migration.
func TestGroupsKeyUnchangedWithUseCases(t *testing.T) {
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, nil, nil)
	defer srv.Close()

	plain, err := New(groupsCfg(srv, map[string]string{}), source.Deps{})
	if err != nil {
		t.Fatal(err)
	}

	cfg := groupsCfg(srv, map[string]string{})
	cfg.APIKeyUseCases = []config.APIKeyUseCase{{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}}}
	withUC, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}

	plainKey := plain.Loops()[0].Key().String()
	ucKey := withUC.Loops()[0].Key().String()
	if plainKey != ucKey {
		t.Fatalf("groups Key changed with use-cases: %q vs %q", plainKey, ucKey)
	}
}

// TestGroupsLegacySettingsAPIKeyIDsPreserved asserts that when api_key_use_cases is empty, the legacy
// settings.api_key_ids is used as the single unlabelled pass (backward compatible).
func TestGroupsLegacySettingsAPIKeyIDsPreserved(t *testing.T) {
	srv := fakeGroups(t, map[string][]groupsResponse{"ai-models": {modelRows("a")}}, nil, nil)
	defer srv.Close()

	cfg := groupsCfg(srv, map[string]string{"api_key_ids": "legacy-uuid"})
	src, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	gl := src.Loops()[0].(*groupsLoop)
	if len(gl.passes) != 1 || gl.passes[0].apiKeyIDsCSV != "legacy-uuid" || gl.passes[0].slug != "" {
		t.Fatalf("legacy pass wrong: %+v", gl.passes)
	}
}

// TestGroupsCollectStampsUseCaseAndFilters is the e2e test (C2): drives a use-case-configured
// groupsLoop against fakeGroups, asserts every returned sample carries Labels["api_key_use_case"]
// with the correct slug, and that the recorded queries contain api_key_ids.
func TestGroupsCollectStampsUseCaseAndFilters(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	var queries []string
	srv := fakeGroups(t, map[string][]groupsResponse{
		"ai-models": {modelRows("gpt-4o")},
		"prompt":    {promptRows("summarize-v2")},
	}, nil, &queries)
	defer srv.Close()

	cfg := groupsCfg(srv, map[string]string{"page_size": "100"})
	cfg.APIKeyUseCases = []config.APIKeyUseCase{
		{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
	}

	gl := mkGroups(t, cfg, source.Deps{}, now)

	b, err := gl.Collect(context.Background(), model.Watermark{Time: now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(b.Samples) == 0 {
		t.Fatal("expected at least one sample")
	}

	// Every sample must carry the api_key_use_case label with value "data_gen".
	for _, s := range b.Samples {
		got := s.Labels[useCaseLabelKey]
		if got != "data_gen" {
			t.Errorf("sample %q: Labels[%q]=%q want %q", s.Name, useCaseLabelKey, got, "data_gen")
		}
	}

	// At least one recorded query must include api_key_ids=uuid-a.
	var sawFilter bool
	for _, q := range queries {
		if strings.Contains(q, "api_key_ids=uuid-a") {
			sawFilter = true
			break
		}
	}
	if !sawFilter {
		t.Fatalf("no query contained api_key_ids=uuid-a; queries: %v", queries)
	}
}
