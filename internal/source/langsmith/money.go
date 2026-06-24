// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

import (
	"bytes"
	"fmt"
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
		m.v, m.set = f, true
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("langsmith: cost number %q: %w", b, err)
	}
	m.v, m.set = f, true
	return nil
}
