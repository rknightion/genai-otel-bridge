// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"slices"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

func buildRunsSettings(s map[string]string) (runsSettings, error) {
	rs := defaultRunsSettings()
	err := applyRunsSettings(&rs, s)
	return rs, err
}

func TestApplyRunsSettings(t *testing.T) {
	// scope: neither session_ids nor session_filter → fail (never firehose all projects)
	if _, err := buildRunsSettings(map[string]string{}); err == nil {
		t.Fatal("empty scope (no session_ids/session_filter) must fail")
	}
	// session_filter alone is a valid scope (auto-discovery)
	if _, err := buildRunsSettings(map[string]string{"session_filter": `eq(name, "x")`}); err != nil {
		t.Fatalf("session_filter alone must be valid: %v", err)
	}
	// session_ids alone is valid (static)
	if _, err := buildRunsSettings(map[string]string{"session_ids": "a,b"}); err != nil {
		t.Fatalf("session_ids alone must be valid: %v", err)
	}
	// page_size > 100 rejected (API max)
	if _, err := buildRunsSettings(map[string]string{"session_ids": "s1", "page_size": "500"}); err == nil {
		t.Fatal("page_size>100 must fail")
	}
	// settle >= window inverts the window
	if _, err := buildRunsSettings(map[string]string{"session_ids": "s1", "window": "10m", "settle": "10m"}); err == nil {
		t.Fatal("settle>=window must fail")
	}
	// happy overlay
	rs, err := buildRunsSettings(map[string]string{
		"session_ids": "s1, s2", "session_filter": `eq(name,"x")`, "window": "30m", "settle": "5m",
		"max_backfill": "12h", "page_size": "50", "max_pages_per_window": "10", "max_sessions": "20",
		"session_refresh": "2m", "max_response_bytes": "1048576", "root_only": "true", "run_type": "llm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.sessionIDs) != 2 || rs.sessionFilter == "" || rs.window != 30*time.Minute || rs.pageSize != 50 ||
		rs.maxSessions != 20 || rs.sessionRefresh != 2*time.Minute || rs.maxResponseBytes != 1048576 ||
		!rs.rootOnly || rs.runType != "llm" {
		t.Fatalf("overlay wrong: %+v", rs)
	}
	// unknown key warns, does not fail
	if _, err := buildRunsSettings(map[string]string{"session_ids": "s1", "bogus": "x"}); err != nil {
		t.Fatalf("unknown key must not fail: %v", err)
	}
}

// TestRunsExtraRecordFields: the operator can opt extra content-free fields INTO the record allow-list
// (default-on strip stays content-free; opt-in layers on). Opting in a hard-denied message-body field
// is rejected at config time — the composition-root guard denylist would otherwise drop the WHOLE record
// silently (okLog), so fail-fast loud instead of emitting nothing.
func TestRunsExtraRecordFields(t *testing.T) {
	rs, err := buildRunsSettings(map[string]string{
		"session_ids": "s1", "extra_record_fields": "tags, child_run_ids , app_path",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.extraRecordFields) != 3 || rs.extraRecordFields[0] != "tags" ||
		rs.extraRecordFields[1] != "child_run_ids" || rs.extraRecordFields[2] != "app_path" {
		t.Fatalf("extra_record_fields parse wrong: %v", rs.extraRecordFields)
	}
	// Every shared content-floor key (message bodies + injected PII) must be rejected — the fail-fast
	// mirror cannot drift from the app guard backstop (review HIGH-2). Plus langsmith raw-blob S3 pointers.
	banned := append(source.AbsoluteNeverDenyKeys(), "inputs_s3_urls", "outputs_s3_urls")
	for _, b := range banned {
		if _, err := buildRunsSettings(map[string]string{
			"session_ids": "s1", "extra_record_fields": "tags," + b,
		}); err == nil {
			t.Errorf("opting in hard-denied content field %q must fail", b)
		}
	}
}

// TestRunsExtraIndexedFields: the per-loop indexed-tier opt-in parses a csv and rejects a hard-denied
// content/floor field exactly as extra_record_fields does (a body must never become a stream label).
func TestRunsExtraIndexedFields(t *testing.T) {
	rs, err := buildRunsSettings(map[string]string{
		"session_ids": "s1", "extra_indexed_fields": "app_path, run_type",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.extraIndexedFields) != 2 || rs.extraIndexedFields[0] != "app_path" || rs.extraIndexedFields[1] != "run_type" {
		t.Fatalf("extra_indexed_fields parse wrong: %v", rs.extraIndexedFields)
	}
	banned := append(source.AbsoluteNeverDenyKeys(), "inputs_s3_urls", "outputs_s3_urls")
	for _, b := range banned {
		if _, err := buildRunsSettings(map[string]string{
			"session_ids": "s1", "extra_indexed_fields": "app_path," + b,
		}); err == nil {
			t.Errorf("opting hard-denied %q into extra_indexed_fields must fail", b)
		}
	}
}

// TestRunsRejectsUnknownSelectField (#65): an extra_record_fields/extra_indexed_fields opt-in value that
// is not in the known LangSmith 0.13.5 select enum is rejected at config-load time — otherwise it 422s the
// WHOLE runs/query at runtime (a one-character typo → total loop outage). A valid enum value still passes.
func TestRunsRejectsUnknownSelectField(t *testing.T) {
	// A hyphen typo of a real field ("app_path" → "app-path") is not in the enum → fail fast.
	if _, err := buildRunsSettings(map[string]string{
		"session_ids": "s1", "extra_record_fields": "tags,app-path",
	}); err == nil {
		t.Fatal("a typo'd extra_record_fields value outside the select enum must be rejected (would 422 every query)")
	}
	// Same for the indexed-tier opt-in.
	if _, err := buildRunsSettings(map[string]string{
		"session_ids": "s1", "extra_indexed_fields": "not_a_real_select_field",
	}); err == nil {
		t.Fatal("a typo'd extra_indexed_fields value outside the select enum must be rejected")
	}
	// A genuine enum value still passes (no false positive on a valid opt-in).
	if _, err := buildRunsSettings(map[string]string{
		"session_ids": "s1", "extra_record_fields": "reference_example_id", "extra_indexed_fields": "app_path",
	}); err != nil {
		t.Fatalf("a valid select-enum opt-in must pass: %v", err)
	}
}

// TestValidLangsmithSelectEnumIsSingleSource (#65): the production validator and the projection guard test
// share ONE enum var (promoted out of the test file), so they cannot drift.
func TestValidLangsmithSelectEnumIsSingleSource(t *testing.T) {
	for _, f := range defaultRunsFieldPolicy().selectKeys() {
		if !validLangsmithSelectEnum[f] {
			t.Fatalf("default select field %q missing from validLangsmithSelectEnum", f)
		}
	}
}

// TestHardDeniedRunsFieldsCoversFloor guards against drift: every key the shared content floor denies
// must also be rejected by the runs opt-in validation (else opting one in passes config then the guard
// silently drops the whole record — review HIGH-2).
func TestHardDeniedRunsFieldsCoversFloor(t *testing.T) {
	for _, k := range source.AbsoluteNeverDenyKeys() {
		if !hardDeniedRunsFields[k] {
			t.Errorf("floor key %q is not in hardDeniedRunsFields (opt-in mirror drifted from the guard backstop)", k)
		}
	}
}

func TestNewRunsLoopExtraRecordFields(t *testing.T) {
	lp, err := newRunsLoop(
		config.SourceConfig{BaseURL: "https://x", SourceInstance: "ls", Auth: config.AuthConfig{Header: "x-api-key", Value: "k"}},
		config.LoopConfig{Enabled: true, Cadence: config.Duration(time.Minute), Settings: map[string]string{
			"session_filter": `eq(name,"x")`, "extra_record_fields": "tags, app_path",
		}},
		source.Deps{}, runsTestClient(t))
	if err != nil {
		t.Fatal(err)
	}
	// extras layer onto the content-free default record allow-list (defaults preserved)
	if !lp.policy.record["tags"] || !lp.policy.record["app_path"] {
		t.Fatalf("extra record fields not added to policy: %v", lp.policy.record)
	}
	if !lp.policy.record["id"] || !lp.policy.indexed["run_type"] {
		t.Fatalf("default content-free policy fields lost: %v / %v", lp.policy.record, lp.policy.indexed)
	}
	// selectFields mirrors the (extended) allow-list so opted-in fields are actually requested
	if !slices.Contains(lp.selectFields, "tags") || !slices.Contains(lp.selectFields, "app_path") || !slices.Contains(lp.selectFields, "run_type") {
		t.Fatalf("selectFields must mirror the extended allow-list: %v", lp.selectFields)
	}
}

func TestNewRunsLoopRejectsNonZeroWindow(t *testing.T) {
	_, err := newRunsLoop(
		config.SourceConfig{BaseURL: "https://x", Auth: config.AuthConfig{Header: "x-api-key", Value: "k"}},
		config.LoopConfig{Enabled: true, Window: config.Duration(time.Hour), Settings: map[string]string{"session_ids": "s1"}},
		source.Deps{}, runsTestClient(t))
	if err == nil {
		t.Fatal("non-zero LoopConfig.Window must be rejected (snapshot-scheduled)")
	}
}

func TestNewRunsLoopDefaults(t *testing.T) {
	lp, err := newRunsLoop(
		config.SourceConfig{BaseURL: "https://x", SourceInstance: "ls", Auth: config.AuthConfig{Header: "x-api-key", Value: "k"}},
		config.LoopConfig{Enabled: true, Cadence: config.Duration(time.Minute), Settings: map[string]string{"session_filter": `eq(name,"x")`}},
		source.Deps{}, runsTestClient(t))
	if err != nil {
		t.Fatal(err)
	}
	if lp.window != time.Hour || lp.settle != 10*time.Minute || lp.pageSize != runsPageSizeMax ||
		lp.maxResponseBytes != defaultRunsResponseBytes || len(lp.selectFields) == 0 || lp.policy.indexed == nil {
		t.Fatalf("defaults wrong: %+v", lp)
	}
}

// TestRunsIndexedKeys: IndexedKeys() returns the base content-free indexed set ∪ extra_indexed_fields,
// sorted — the set the composition root budgets against the Loki max_label_names_per_series limit.
// Satisfies source.IndexedKeyDeclarer.
func TestRunsIndexedKeys(t *testing.T) {
	lp, err := newRunsLoop(
		config.SourceConfig{BaseURL: "https://x", SourceInstance: "ls", Auth: config.AuthConfig{Header: "x-api-key", Value: "k"}},
		config.LoopConfig{Enabled: true, Cadence: config.Duration(time.Minute), Settings: map[string]string{
			"session_ids": "s1", "extra_indexed_fields": "app_path, reference_dataset_id",
		}},
		source.Deps{}, runsTestClient(t))
	if err != nil {
		t.Fatal(err)
	}
	got := lp.IndexedKeys()
	want := []string{"app_path", "reference_dataset_id", "run_type", "status"} // base 2 ∪ 2 extras, sorted
	if !slices.Equal(got, want) {
		t.Fatalf("IndexedKeys = %v, want %v", got, want)
	}
}
