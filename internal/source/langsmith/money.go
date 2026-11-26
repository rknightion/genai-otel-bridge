// SPDX-License-Identifier: Apache-2.0

package langsmith

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// money decodes a cost field that LangSmith returns inconsistently across versions: a JSON number
// (0.13.5) OR a quoted decimal string (0.16.5 spec). `null` and `""` mean "no value". `set`
// distinguishes a real value (including 0) from absent, so an unset cost is skipped rather than
// emitted as a misleading 0.
type money struct {
	v   float64
	set bool
}

func (m *money) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil // unset
	}
	if b[0] == '"' { // quoted string form
		unq, err := strconv.Unquote(string(b))
		if err != nil {
			return fmt.Errorf("langsmith: cost string: %w", err)
		}
		unq = strings.TrimSpace(unq)
		if unq == "" {
			return nil // empty ⇒ unset
		}
		f, err := strconv.ParseFloat(unq, 64)
		if err != nil {
			return fmt.Errorf("langsmith: cost %q: %w", unq, err)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("langsmith: cost %q is not finite", unq)
		}
		m.v, m.set = f, true
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("langsmith: cost number %q: %w", b, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("langsmith: cost %q is not finite", b)
	}
	m.v, m.set = f, true
	return nil
}
