// SPDX-License-Identifier: Apache-2.0

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
