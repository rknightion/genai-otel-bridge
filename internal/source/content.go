// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"slices"
	"strings"
)

// AbsoluteNeverDenyKeys are the message-body and injected-PII field keys that must NEVER egress and must
// NEVER be opt-in-able — the hard FLOOR of the content denylist. They are shared by the composition root
// (the guard's DenyFieldKeys backstop, internal/app) and every source's content opt-in validation (e.g.
// langsmith runs `extra_record_fields`) so the two layers CANNOT drift out of sync. The composition root
// MAY add further "gray" fields to the denylist as defence-in-depth (those are subtractable when an
// operator explicitly opts one in); these floor keys are never subtractable, never opt-in-able.
//
// Names cover the OTel GenAI content attrs, the OpenInference value attrs, the generic gateway/eval body
// fields (request/response/inputs/outputs/messages), the direct LLM-content previews
// (`inputs_preview`/`outputs_preview` — truncated renderings of the actual prompt/response, which ARE
// prompt/response content and so belong on the floor, not the subtractable gray tier — #95), and the two
// fields the Portkey logs-export PoC proved are injected regardless of requested_data (`metadata` =
// customer PII, `portkeyHeaders` = gateway config). Vendor-specific content fields (e.g. LangSmith
// `inputs_s3_urls`) are added on top by the owning source's own hard-denied set, not here (this stays
// vendor-neutral).
func AbsoluteNeverDenyKeys() []string {
	return []string{
		"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages", "gen_ai.output.messages",
		"gen_ai.system_instructions", "input.value", "output.value",
		"request", "response", "inputs", "outputs", "messages",
		"inputs_preview", "outputs_preview",
		"metadata", "portkeyHeaders",
	}
}

// genAIContentPrefixes are the OTel GenAI content-attribute NAMESPACES whose flattened/indexed variants
// (e.g. `gen_ai.prompt.0.content`, `gen_ai.completion.1.content`) the exact-match floor list above cannot
// catch. The repo's own docs describe the floor as covering `gen_ai.prompt*` / `gen_ai.completion*`
// (source/CLAUDE.md, followup.md) — i.e. PREFIX coverage — so IsContentFloorKey honours that contract
// (#97). Scoped to the known content namespaces (not a blanket `gen_ai.` prefix) so content-free
// operational GenAI semconv attrs (`gen_ai.request.*`, `gen_ai.usage.*`) are not over-blocked.
var genAIContentPrefixes = []string{
	"gen_ai.prompt", "gen_ai.completion",
	"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
}

// IsContentFloorKey reports whether k is a never-subtractable content-floor key — an exact match of
// AbsoluteNeverDenyKeys OR a prefix variant of a gen_ai content namespace (the flattened/indexed content
// forms). This is the SINGLE matching helper both defence layers should use (the guard deny check here,
// and each source's opt-in validation) so exact-vs-prefix behaviour cannot drift between them (#97).
func IsContentFloorKey(k string) bool {
	if slices.Contains(AbsoluteNeverDenyKeys(), k) {
		return true
	}
	for _, p := range genAIContentPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
