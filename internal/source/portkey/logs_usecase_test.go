// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/config"
	"github.com/grafana-ps/aip-oi/internal/model"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// logsUseCaseCfg builds a SourceConfig for use-case fan-out tests (no httptest.Server needed for
// construction; the required safety keys are hard-coded).
func logsUseCaseCfg(ucs []config.APIKeyUseCase) config.SourceConfig {
	return config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1", SourceInstance: "portkey-test",
		Auth:      config.AuthConfig{Header: "x-portkey-api-key", Value: "tok"},
		RateLimit: config.RateLimitConfig{RPS: 1, Burst: 3},
		Loops: map[string]config.LoopConfig{"logs_export": {Enabled: true, Settings: map[string]string{
			"workspace_id": "ws-x", "signed_url_allow_hosts": "host.example.com",
		}}},
		APIKeyUseCases: ucs,
	}
}

// logsCfgWithUseCases builds a SourceConfig wired to an httptest.Server with api_key_use_cases set.
// Extends logsCfg (from logs_collect_test.go) with APIKeyUseCases.
func logsCfgWithUseCases(srv *httptest.Server, settings map[string]string, ucs []config.APIKeyUseCase) config.SourceConfig {
	cfg := logsCfg(srv, settings)
	cfg.APIKeyUseCases = ucs
	return cfg
}

// TestLogsFanout: logs DO fan out (not a SeriesDeclarer ⇒ ownership-safe): one instance per
// use-case, distinct checkpoint keys.
func TestLogsFanout(t *testing.T) {
	src, err := New(logsUseCaseCfg([]config.APIKeyUseCase{
		{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
		{Label: "Content Gen", APIKeyIDs: []string{"uuid-b"}},
	}), source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Loops()) != 2 {
		t.Fatalf("want 2 logs instances, got %d", len(src.Loops()))
	}
	a, b := src.Loops()[0].(*logsExportLoop), src.Loops()[1].(*logsExportLoop)
	if a.useCase != "data_gen" {
		t.Fatalf("loop[0] useCase=%q want data_gen", a.useCase)
	}
	if a.apiKeyIDs != "uuid-a" {
		t.Fatalf("loop[0] apiKeyIDs=%q want uuid-a", a.apiKeyIDs)
	}
	if a.Key().String() == b.Key().String() {
		t.Fatalf("fan-out loops must have distinct keys: a=%s b=%s", a.Key(), b.Key())
	}
}

// TestLogsStripStampsUseCaseRecord: strip stamps the constant slug on RecordAttributes
// (record-tier), never IndexedAttributes.
func TestLogsStripStampsUseCaseRecord(t *testing.T) {
	p := defaultLogFieldPolicy()
	p.useCase = "data_gen"
	raw := map[string]json.RawMessage{"id": json.RawMessage(`"r1"`)}
	lr := p.strip(raw, time.Unix(0, 0).UTC())
	if lr.RecordAttributes[useCaseLabelKey] != "data_gen" {
		t.Fatalf("record attr missing or wrong: RecordAttributes=%v", lr.RecordAttributes)
	}
	if v := lr.IndexedAttributes[useCaseLabelKey]; v != "" {
		t.Fatalf("use-case must be record-tier, not indexed: IndexedAttributes[%s]=%q", useCaseLabelKey, v)
	}
}

// TestLogsExportAPIKeyIDsFilterBody drives a fanned logsExportLoop against fakeExport and asserts:
//  1. The recorded POST /logs/exports body's filters.api_key_ids == ["uuid-a"] (the use-case filter
//     is wired into the create request body, confirmed accepted by the 2026-06-24 live probe).
//  2. All downloaded records carry RecordAttributes["api_key_use_case"]=="data_gen" (record-tier stamp).
func TestLogsExportAPIKeyIDsFilterBody(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	f := newFakeExport(t, 3, func(page int) string { return nLines(3, "gpt-5") })

	cfg := logsCfgWithUseCases(f.srv,
		map[string]string{"window": "1h", "settle": "10m"},
		[]config.APIKeyUseCase{{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}}},
	)
	src, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Loops()) != 1 {
		t.Fatalf("want 1 fan-out instance (one use-case), got %d", len(src.Loops()))
	}
	l := src.Loops()[0].(*logsExportLoop)
	l.now = func() time.Time { return now }

	logs, _ := drive(t, l, model.Watermark{Time: now.Add(-time.Hour)}, 10)

	// Assert 1: filters.api_key_ids in the create body == ["uuid-a"].
	if len(f.createBodies) == 0 {
		t.Fatal("no create request captured")
	}
	filtersRaw, ok := f.createBodies[0]["filters"]
	if !ok {
		t.Fatal("create body missing filters key")
	}
	var filt struct {
		APIKeyIDs []string `json:"api_key_ids"`
	}
	if err := json.Unmarshal(filtersRaw, &filt); err != nil {
		t.Fatalf("unmarshal filters: %v", err)
	}
	if len(filt.APIKeyIDs) != 1 || filt.APIKeyIDs[0] != "uuid-a" {
		t.Fatalf("filters.api_key_ids=%v want [uuid-a]", filt.APIKeyIDs)
	}

	// Assert 2: all records carry api_key_use_case="data_gen" as a record attr (not indexed).
	if len(logs) != 3 {
		t.Fatalf("emitted %d logs, want 3", len(logs))
	}
	for i, lr := range logs {
		got := lr.RecordAttributes[useCaseLabelKey]
		if got != "data_gen" {
			t.Fatalf("record[%d] RecordAttributes[%s]=%q want data_gen", i, useCaseLabelKey, got)
		}
		if v := lr.IndexedAttributes[useCaseLabelKey]; v != "" {
			t.Fatalf("record[%d] use-case must be record-tier, not indexed: %v", i, lr.IndexedAttributes)
		}
	}
}
