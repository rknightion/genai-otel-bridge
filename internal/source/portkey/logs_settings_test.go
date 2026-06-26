// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// baseLogsSettings is a VALID minimal settings map (the two safety-required keys + workspace_id set).
func baseLogsSettings() map[string]string {
	return map[string]string{
		"workspace_id":           "ws-test",
		"signed_url_allow_hosts": "signed-url-host.example.com",
	}
}

// TestLogsSettingsMetadataFields: the two metadata-lift knobs parse into the settings and a hard-denied
// content sub-key is rejected fail-fast (can't smuggle a body out via metadata extraction).
func TestLogsSettingsMetadataFields(t *testing.T) {
	ls := defaultLogsSettings()
	s := baseLogsSettings()
	s["metadata_record_fields"] = "correlation_id,session_ref"
	s["metadata_trace_id_field"] = "correlation_id"
	if err := applyLogsSettings(&ls, s); err != nil {
		t.Fatalf("valid metadata settings rejected: %v", err)
	}
	if !slices.Equal(ls.metadataRecordFields, []string{"correlation_id", "session_ref"}) {
		t.Fatalf("metadata_record_fields=%v", ls.metadataRecordFields)
	}
	if ls.metadataTraceIDField != "correlation_id" {
		t.Fatalf("metadata_trace_id_field=%q", ls.metadataTraceIDField)
	}

	// A hard-denied content field must be rejected in either knob.
	ls = defaultLogsSettings()
	bad := baseLogsSettings()
	bad["metadata_record_fields"] = "prompt"
	if err := applyLogsSettings(&ls, bad); err == nil {
		t.Fatal("expected metadata_record_fields=prompt (hard-denied) to be rejected")
	}
	ls = defaultLogsSettings()
	bad2 := baseLogsSettings()
	bad2["metadata_trace_id_field"] = "messages"
	if err := applyLogsSettings(&ls, bad2); err == nil {
		t.Fatal("expected metadata_trace_id_field=messages (hard-denied) to be rejected")
	}
}

// TestLogsSettingsTraceIDField: the top-level trace_id_field knob (Portkey-native trace_id path) parses,
// rejects csv (single key only), rejects a hard-denied content field, and is mutually exclusive with
// metadata_trace_id_field (both map to the OTLP trace_id — set exactly one).
func TestLogsSettingsTraceIDField(t *testing.T) {
	ls := defaultLogsSettings()
	s := baseLogsSettings()
	s["trace_id_field"] = "trace_id"
	if err := applyLogsSettings(&ls, s); err != nil {
		t.Fatalf("valid trace_id_field rejected: %v", err)
	}
	if ls.traceIDField != "trace_id" {
		t.Fatalf("trace_id_field=%q", ls.traceIDField)
	}

	// csv rejected — it names a SINGLE field (use metadata_record_fields for multiple lifts).
	ls = defaultLogsSettings()
	bad := baseLogsSettings()
	bad["trace_id_field"] = "trace_id,correlation_id"
	if err := applyLogsSettings(&ls, bad); err == nil {
		t.Fatal("expected csv trace_id_field to be rejected")
	}

	// hard-denied content field rejected.
	ls = defaultLogsSettings()
	bad2 := baseLogsSettings()
	bad2["trace_id_field"] = "messages"
	if err := applyLogsSettings(&ls, bad2); err == nil {
		t.Fatal("expected hard-denied trace_id_field to be rejected")
	}

	// mutually exclusive with metadata_trace_id_field (one OTLP trace_id, one source).
	ls = defaultLogsSettings()
	bad3 := baseLogsSettings()
	bad3["trace_id_field"] = "trace_id"
	bad3["metadata_trace_id_field"] = "correlation_id"
	if err := applyLogsSettings(&ls, bad3); err == nil {
		t.Fatal("expected trace_id_field + metadata_trace_id_field (both set) to be rejected")
	}
}

func TestLogsSettingsDefaults(t *testing.T) {
	ls := defaultLogsSettings()
	if err := applyLogsSettings(&ls, baseLogsSettings()); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	if ls.window != time.Hour || ls.settle != 10*time.Minute {
		t.Fatalf("window/settle defaults wrong: %s/%s", ls.window, ls.settle)
	}
	if ls.pageSize != portkeyPageSizeMax || ls.chunkMaxRecords != 5000 {
		t.Fatalf("page_size/chunk defaults wrong: %d/%d", ls.pageSize, ls.chunkMaxRecords)
	}
	// requested_data defaults to the content-free strip allow-list — and must itself be content-free.
	for _, f := range ls.requestedData {
		if contentRequestKeys[f] {
			t.Fatalf("default requested_data contains content field %q", f)
		}
	}
}

func TestLogsSettingsRequiresSafetyKeys(t *testing.T) {
	cases := map[string]map[string]string{
		"missing workspace_id":           {"signed_url_allow_hosts": "h"},
		"missing signed_url_allow_hosts": {"workspace_id": "ws"},
		"empty signed_url_allow_hosts":   {"workspace_id": "ws", "signed_url_allow_hosts": ""},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			ls := defaultLogsSettings()
			if err := applyLogsSettings(&ls, s); err == nil {
				t.Fatalf("%s: expected a fail-fast error, got nil", name)
			}
		})
	}
}

func TestLogsSettingsRejectsContentInRequestedData(t *testing.T) {
	for _, bad := range []string{"prompt", "request", "response", "metadata", "portkeyHeaders", "messages"} {
		s := baseLogsSettings()
		s["requested_data"] = "id,trace_id," + bad
		ls := defaultLogsSettings()
		err := applyLogsSettings(&ls, s)
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("requested_data with content field %q must be rejected, got err=%v", bad, err)
		}
	}
}

// TestLogsSettingsExtraRecordFields: the operator can opt extra content-free fields into the strip's
// RECORD allow-list; opting in a hard-denied content/PII field (the shared floor + the Portkey content
// fields incl. bare "prompt") is rejected fail-fast.
func TestLogsSettingsExtraRecordFields(t *testing.T) {
	s := baseLogsSettings()
	s["extra_record_fields"] = "cache_status, mode "
	ls := defaultLogsSettings()
	if err := applyLogsSettings(&ls, s); err != nil {
		t.Fatal(err)
	}
	if len(ls.extraRecordFields) != 2 || ls.extraRecordFields[0] != "cache_status" || ls.extraRecordFields[1] != "mode" {
		t.Fatalf("extra_record_fields parse wrong: %v", ls.extraRecordFields)
	}
	banned := append(source.AbsoluteNeverDenyKeys(), "prompt")
	for _, b := range banned {
		bs := baseLogsSettings()
		bs["extra_record_fields"] = "cache_status," + b
		l := defaultLogsSettings()
		if err := applyLogsSettings(&l, bs); err == nil {
			t.Errorf("opting in hard-denied content field %q must fail", b)
		}
	}
}

// TestLogsSettingsExtraIndexedFields: the per-loop indexed-tier opt-in parses a csv and rejects a
// hard-denied content/PII field exactly as extra_record_fields does (a body must never become a label).
func TestLogsSettingsExtraIndexedFields(t *testing.T) {
	s := baseLogsSettings()
	s["extra_indexed_fields"] = "cache_status, mode "
	ls := defaultLogsSettings()
	if err := applyLogsSettings(&ls, s); err != nil {
		t.Fatal(err)
	}
	if len(ls.extraIndexedFields) != 2 || ls.extraIndexedFields[0] != "cache_status" || ls.extraIndexedFields[1] != "mode" {
		t.Fatalf("extra_indexed_fields parse wrong: %v", ls.extraIndexedFields)
	}
	banned := append(source.AbsoluteNeverDenyKeys(), "prompt")
	for _, b := range banned {
		bs := baseLogsSettings()
		bs["extra_indexed_fields"] = "cache_status," + b
		l := defaultLogsSettings()
		if err := applyLogsSettings(&l, bs); err == nil {
			t.Errorf("opting hard-denied %q into extra_indexed_fields must fail", b)
		}
	}
}

// TestHardDeniedLogFieldsCoversFloor: the opt-in rejection set must include the entire shared content
// floor so it can't drift from the app guard backstop (mirror of the langsmith drift guard).
func TestHardDeniedLogFieldsCoversFloor(t *testing.T) {
	for _, k := range source.AbsoluteNeverDenyKeys() {
		if !hardDeniedLogFields[k] {
			t.Errorf("floor key %q missing from hardDeniedLogFields", k)
		}
	}
}

func TestLogsSettingsRejectsOverlargePageSize(t *testing.T) {
	s := baseLogsSettings()
	s["page_size"] = "50001"
	ls := defaultLogsSettings()
	if err := applyLogsSettings(&ls, s); err == nil {
		t.Fatal("page_size > 50000 (Portkey max) must be rejected")
	}
}

func TestLogsSettingsParsesOverrides(t *testing.T) {
	s := baseLogsSettings()
	s["window"] = "30m"
	s["settle"] = "5m"
	s["page_size"] = "10000"
	s["chunk_max_records"] = "1000"
	s["max_pages_per_window"] = "5"
	s["job_poll_timeout"] = "2m"
	s["requested_data"] = "id, trace_id, ai_model"
	s["signed_url_allow_hosts"] = "a.example.com, b.example.com"
	ls := defaultLogsSettings()
	if err := applyLogsSettings(&ls, s); err != nil {
		t.Fatal(err)
	}
	if ls.window != 30*time.Minute || ls.settle != 5*time.Minute || ls.pageSize != 10000 ||
		ls.chunkMaxRecords != 1000 || ls.maxPagesPerWindow != 5 || ls.jobPollTimeout != 2*time.Minute {
		t.Fatalf("overrides not applied: %+v", ls)
	}
	if len(ls.requestedData) != 3 || len(ls.signedURLAllowHosts) != 2 {
		t.Fatalf("csv parse wrong: requested=%v hosts=%v", ls.requestedData, ls.signedURLAllowHosts)
	}
}

// TestLogsExportIndexedKeys: IndexedKeys() returns the base content-free indexed set ∪
// extra_indexed_fields, sorted — the set the composition root budgets against the Loki
// max_label_names_per_series limit. Satisfies source.IndexedKeyDeclarer.
func TestLogsExportIndexedKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	l := mkLogsLoop(t, logsCfg(srv, map[string]string{"window": "1h", "settle": "10m", "extra_indexed_fields": "cache_status, mode"}), time.Now().UTC())
	got := l.IndexedKeys()
	want := []string{"ai_model", "ai_org", "cache_status", "mode", "response_status_code"} // base 3 ∪ 2 extras, sorted
	if !slices.Equal(got, want) {
		t.Fatalf("IndexedKeys = %v, want %v", got, want)
	}
}
