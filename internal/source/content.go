// SPDX-License-Identifier: AGPL-3.0-only

package source

// AbsoluteNeverDenyKeys are the message-body and injected-PII field keys that must NEVER egress and must
// NEVER be opt-in-able — the hard FLOOR of the content denylist. They are shared by the composition root
// (the guard's DenyFieldKeys backstop, internal/app) and every source's content opt-in validation (e.g.
// langsmith runs `extra_record_fields`) so the two layers CANNOT drift out of sync. The composition root
// MAY add further "gray" fields to the denylist as defence-in-depth (those are subtractable when an
// operator explicitly opts one in); these floor keys are never subtractable, never opt-in-able.
//
// Names cover the OTel GenAI content attrs, the OpenInference value attrs, the generic gateway/eval body
// fields (request/response/inputs/outputs/messages), and the two fields the Portkey logs-export PoC
// proved are injected regardless of requested_data (`metadata` = customer PII, `portkeyHeaders` = gateway
// config). Vendor-specific content fields (e.g. LangSmith `inputs_s3_urls`) are added on top by the
// owning source's own hard-denied set, not here (this stays vendor-neutral).
func AbsoluteNeverDenyKeys() []string {
	return []string{
		"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages", "gen_ai.output.messages",
		"gen_ai.system_instructions", "input.value", "output.value",
		"request", "response", "inputs", "outputs", "messages",
		"metadata", "portkeyHeaders",
	}
}
