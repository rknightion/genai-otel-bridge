// SPDX-License-Identifier: AGPL-3.0-only

package portkey

// AllowedLabelKeys returns the content-free label / indexed-attribute keys this source emits, for the
// composition root to allow-list in the cardinality guard (the guard is default-deny — an un-listed key
// drops the sample/record). Keeping the keys HERE, not hardcoded in internal/app, honours the decoupling
// hard rule: no vendor label keys live in core code or defaults.
//   - quantile: analytics latency percentile gauges ({quantile} label)
//   - token_type: analytics tokens gauge split (total/input/output — prompt vs completion units)
//   - ai_model / metadata_key / metadata_value: groups window-total snapshot gauges
//   - ai_org / ai_model / response_status_code: logs_export indexed attrs (Loki stream-label candidates)
//   - prompt: groups per-prompt request dimension (groups/prompt; default-on, opt out via emit_prompts:false)
//
// (ai_org replaced the dead `ai_provider` — confirmed against the live instance 2026-06-21, followup §9.)
// All are content-free operational identifiers; none is a message body or injected PII.
//   - api_key_use_case: per-api-key use-case name (config-mapped; metrics label)
func AllowedLabelKeys() []string {
	return []string{"quantile", "token_type", "ai_model", "ai_org", "response_status_code", "metadata_key", "metadata_value", "prompt", useCaseLabelKey}
}
