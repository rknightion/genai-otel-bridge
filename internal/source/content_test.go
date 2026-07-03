// SPDX-License-Identifier: AGPL-3.0-only

package source

import (
	"slices"
	"testing"
)

// TestContentPreviewsAreFloorKeys (#95): inputs_preview/outputs_preview are truncated renderings of the
// actual prompt/response (LLM content), so they must be on the never-subtractable content FLOOR — never
// opt-in-able. Every source's hard-denied opt-in set is built from AbsoluteNeverDenyKeys, so putting them
// here makes extra_record_fields opt-in of a preview fail fast at config load across all sources.
func TestContentPreviewsAreFloorKeys(t *testing.T) {
	for _, k := range []string{"inputs_preview", "outputs_preview"} {
		if !slices.Contains(AbsoluteNeverDenyKeys(), k) {
			t.Errorf("%q must be a never-subtractable content floor key (#95)", k)
		}
		if !IsContentFloorKey(k) {
			t.Errorf("IsContentFloorKey(%q) = false, want true (#95)", k)
		}
	}
}

// TestIsContentFloorKeyPrefix (#97): the floor covers gen_ai content namespaces by PREFIX, so flattened/
// indexed content attrs (gen_ai.prompt.0.content, gen_ai.completion.1.content) that exact-match nothing
// are still floor keys — matching the repo's own gen_ai.prompt* / gen_ai.completion* documented contract.
// Content-free operational gen_ai semconv attrs (gen_ai.request.*, gen_ai.usage.*) are NOT over-blocked.
func TestIsContentFloorKeyPrefix(t *testing.T) {
	floor := []string{
		"gen_ai.prompt", "gen_ai.prompt.0.content", "gen_ai.completion.1.content",
		"gen_ai.input.messages.0.content", "gen_ai.system_instructions",
		"inputs", "metadata", "portkeyHeaders",
	}
	for _, k := range floor {
		if !IsContentFloorKey(k) {
			t.Errorf("IsContentFloorKey(%q) = false, want true", k)
		}
	}
	notFloor := []string{
		"gen_ai.request.model", "gen_ai.usage.input_tokens", "gen_ai.response.id",
		"ai_model", "ai_org", "prompt", "run_type", "status", "quantile",
	}
	for _, k := range notFloor {
		if IsContentFloorKey(k) {
			t.Errorf("IsContentFloorKey(%q) = true, want false (content-free operational key over-blocked)", k)
		}
	}
}
