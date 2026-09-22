// SPDX-License-Identifier: Apache-2.0

package version

import "testing"

func TestString(t *testing.T) {
	if String() == "" {
		t.Fatal("version.String() must not be empty")
	}
}
