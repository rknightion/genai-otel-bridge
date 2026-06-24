// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"slices"
	"testing"
)

func TestAllowedLabelKeysIncludesUseCase(t *testing.T) {
	if !slices.Contains(AllowedLabelKeys(), useCaseLabelKey) {
		t.Fatalf("AllowedLabelKeys missing %q: %v", useCaseLabelKey, AllowedLabelKeys())
	}
}
