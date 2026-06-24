// SPDX-License-Identifier: AGPL-3.0-only

package version

import "testing"

func TestString(t *testing.T) {
	if String() == "" {
		t.Fatal("version.String() must not be empty")
	}
}
