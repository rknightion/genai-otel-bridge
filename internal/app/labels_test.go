// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/langsmith"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

// TestLabelAllowListUnionMatchesHistorical is the decoupling-refactor regression guard: moving the guard
// allow-list out of a hardcoded app.go slice and into per-vendor AllowedLabelKeys() must union to EXACTLY
// the expected set — no key dropped (would silently drop a metric/log) or added unintentionally.
// (trace_tier was intentionally dropped from the langsmith runs indexed set — NULL at scale, reclaims a
// Loki stream-label slot; see followup.md — so it is no longer in the union.)
// (token_type added 2026-06-23 — analytics tokens prompt/completion split, see portkey derive.go.)
// (retention_tier added 2026-07-02 — langsmith usage/platform-cost loop trace-retention tier, see langsmith usage_derive.go.)
func TestLabelAllowListUnionMatchesHistorical(t *testing.T) {
	historical := []string{
		"quantile", "token_type", "session", "feedback_key", "ai_model", "ai_org", "response_status_code",
		"metadata_key", "metadata_value", "run_type", "status", "prompt", "api_key_use_case", "retention_tier",
	}
	union := dedupe(append(append([]string{}, portkey.AllowedLabelKeys()...), langsmith.AllowedLabelKeys()...))
	slices.Sort(union)
	slices.Sort(historical)
	if !slices.Equal(union, historical) {
		t.Fatalf("vendor label-key union changed:\n got=%v\nwant=%v", union, historical)
	}
}

// TestBuildRejectsFloorKeyInAllowLabelKeys: opting a content-floor key (message body / injected PII) into
// governance.allow_label_keys must fail fast at Build — the guard would otherwise silently deny it (the
// floor wins), which is exactly the silent no-op the "operationally honest" rule forbids.
func TestBuildRejectsFloorKeyInAllowLabelKeys(t *testing.T) {
	cfg := minimalConfig("http://127.0.0.1:1")
	cfg.Governance.AllowLabelKeys = []string{"messages"} // a floor key (AbsoluteNeverDenyKeys)
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	_, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err == nil || !strings.Contains(err.Error(), "content-floor") {
		t.Fatalf("want a content-floor rejection error, got %v", err)
	}
}

// TestBuildAcceptsContentFreeAllowLabelKey: a content-free extra key is accepted (the additive opt-in).
func TestBuildAcceptsContentFreeAllowLabelKey(t *testing.T) {
	cfg := minimalConfig("http://127.0.0.1:1")
	cfg.Governance.AllowLabelKeys = []string{"deployment_env"} // content-free, not a floor key
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if _, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{}); err != nil {
		t.Fatalf("content-free allow_label_keys opt-in must be accepted, got %v", err)
	}
}

// TestBuildRejectsFloorKeyInExtraIndexedFields: a per-loop settings.extra_indexed_fields naming a
// content-floor key is rejected fail-fast at the composition root (the auto-allow-list backstop), not
// silently dropped. Mirrors the governance.allow_label_keys floor check.
func TestBuildRejectsFloorKeyInExtraIndexedFields(t *testing.T) {
	cfg := minimalConfig("http://127.0.0.1:1")
	lp := cfg.Sources[0].Loops["analytics"]
	if lp.Settings == nil {
		lp.Settings = map[string]string{}
	}
	lp.Settings["extra_indexed_fields"] = "messages" // a content-floor key
	cfg.Sources[0].Loops["analytics"] = lp
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	_, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err == nil || !strings.Contains(err.Error(), "content-floor") {
		t.Fatalf("want a content-floor rejection for extra_indexed_fields, got %v", err)
	}
}

// TestBuildRejectsContentFloorPrefixInExtraIndexedFields (#97): the composition-root floor check matches
// by gen_ai content-namespace PREFIX (source.IsContentFloorKey), so a FLATTENED content attr like
// gen_ai.prompt.0.content — which exact-matches no floor key — is still rejected fail-fast, honouring the
// documented gen_ai.prompt* prefix contract.
func TestBuildRejectsContentFloorPrefixInExtraIndexedFields(t *testing.T) {
	cfg := minimalConfig("http://127.0.0.1:1")
	lp := cfg.Sources[0].Loops["analytics"]
	if lp.Settings == nil {
		lp.Settings = map[string]string{}
	}
	lp.Settings["extra_indexed_fields"] = "gen_ai.prompt.0.content" // flattened content attr — prefix floor
	cfg.Sources[0].Loops["analytics"] = lp
	cp, _ := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	_, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, noopEmitter{}, schedule.NoopMetrics{}, source.Deps{})
	if err == nil || !strings.Contains(err.Error(), "content-floor") {
		t.Fatalf("want a content-floor rejection for the gen_ai prefix variant, got %v", err)
	}
}

// TestGrayFieldPromotedNotSimultaneouslyDenied (#51): a gray backstop key promoted via extra_indexed_fields
// must be BOTH auto-allow-listed AND absent from the effective guard denylist — never both allowed and
// denied (deny beats allow ⇒ silent record drop). Also covers the metadata_record_fields knob.
func TestGrayFieldPromotedNotSimultaneouslyDenied(t *testing.T) {
	cfg := &config.Config{Sources: []config.SourceConfig{{
		Enabled: true,
		Loops: map[string]config.LoopConfig{
			"runs": {Enabled: true, Settings: map[string]string{
				"extra_indexed_fields":   "name",  // gray, promoted to the indexed tier
				"metadata_record_fields": "error", // gray, lifted to the record tier
			}},
		},
	}}}
	deny := contentDenylist(optedInContentFields(cfg))
	if slices.Contains(deny, "name") {
		t.Error("gray key promoted via extra_indexed_fields must be released from the effective denylist (#51)")
	}
	if slices.Contains(deny, "error") {
		t.Error("gray key promoted via metadata_record_fields must be released from the effective denylist (#51)")
	}
	if !slices.Contains(optedInIndexedFields(cfg), "name") {
		t.Error("extra_indexed_fields key must be auto-allow-listed as an indexed attr (#51)")
	}
}
