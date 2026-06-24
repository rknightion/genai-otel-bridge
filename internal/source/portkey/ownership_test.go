// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"testing"
	"time"

	"github.com/grafana-ps/aip-oi/internal/config"
	"github.com/grafana-ps/aip-oi/internal/source"
)

// TestValidateOwnershipWithUseCases is the M7 regression guard for the api_key_use_case feature.
//
// Background: metrics loops (analytics, groups) are SeriesDeclarers. source.ValidateOwnership forbids
// two SeriesDeclarer loops sharing a normalized series name under different checkpoint keys — that
// would produce a duplicate-timestamp storm in Mimir (series identity = name + labels; same name,
// different keys ⇒ two owners advancing the watermark independently). The use-case feature must NOT
// fan out analytics or groups into N distinct-keyed instances.
//
// The correct architecture: analytics and groups each use ONE loop instance with N internal filtered
// passes (one Key per instance). Logs fan out to N instances — safe because logsExportLoop is NOT a
// SeriesDeclarer (it emits OTLP logs, not metric series; distinct cursors carry no duplicate-series risk).
//
// This test builds a source with analytics + groups + logs_export all enabled AND two use-cases, then
// asserts ValidateOwnership passes. A regression (metrics fan-out) would fail ValidateOwnership with
// "series ... owned by both ... and ...".
func TestValidateOwnershipWithUseCases(t *testing.T) {
	cfg := config.SourceConfig{
		Type: "portkey", Enabled: true, BaseURL: "https://api.portkey.ai/v1",
		SourceInstance: "portkey-test",
		Auth:           config.AuthConfig{Header: "x-portkey-api-key", Value: "tok"},
		RateLimit:      config.RateLimitConfig{RPS: 1, Burst: 3},
		APIKeyUseCases: []config.APIKeyUseCase{
			{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
			{Label: "Content Gen", APIKeyIDs: []string{"uuid-b"}},
		},
		Loops: map[string]config.LoopConfig{
			"analytics": {
				Enabled: true, Window: config.Duration(50 * time.Minute),
				Graphs: []string{"requests"},
			},
			"groups": {
				Enabled: true,
			},
			"logs_export": {
				Enabled: true,
				Settings: map[string]string{
					"workspace_id":           "ws-test",
					"signed_url_allow_hosts": "s3.example.com",
				},
			},
		},
	}

	src, err := New(cfg, source.Deps{})
	if err != nil {
		t.Fatalf("New with use-cases failed: %v", err)
	}

	// With 2 use-cases: analytics = 1 loop, groups = 1 loop, logs_export = 2 fan-out loops → 4 total.
	loops := src.Loops()
	if len(loops) != 4 {
		t.Fatalf("want 4 loops (analytics×1 + groups×1 + logs×2), got %d", len(loops))
	}

	// The core assertion: ownership must pass. A metrics fan-out regression would fail here.
	if err := source.ValidateOwnership([]source.Source{src}); err != nil {
		t.Fatalf("ValidateOwnership must pass with api_key_use_cases (M7 regression): %v", err)
	}
}
