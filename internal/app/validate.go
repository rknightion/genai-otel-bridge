// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/langsmith"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

// knownSourceTypes builds the source-type registry exactly as Build does, so config validation matches
// what the running binary would accept.
func knownSourceTypes() map[string]struct{} {
	reg := source.NewRegistry()
	portkey.Register(reg)
	langsmith.Register(reg)
	return reg.Known()
}

var validateEnvRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ValidateConfigFile loads and validates a config file the same way the binary does (schema/known-fields
// + secret substitution + semantic Validate against the registered source types) WITHOUT requiring real
// secrets: any `${ENV}` ref that is unset gets a placeholder so an overlay full of injected refs can be
// checked in CI. Endpoints/URLs get an https placeholder to satisfy the endpoint gate; everything else
// gets a non-empty token. Returns nil if the config is valid.
func ValidateConfigFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Placeholder any unset ${VAR} (scan raw text — refs can appear anywhere, even in comments, which
	// config.Load also resolves). Never clobber a real value the caller has set.
	for _, m := range validateEnvRefRe.FindAllStringSubmatch(string(raw), -1) {
		name := m[1]
		if os.Getenv(name) != "" {
			continue
		}
		val := "x"
		if strings.Contains(name, "ENDPOINT") || strings.Contains(name, "URL") {
			val = "https://placeholder.example"
		}
		// #nosec G104 — Setenv on a validated key name; failure is non-fatal (Load will report the unset ref).
		_ = os.Setenv(name, val)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(knownSourceTypes()); err != nil {
		return err
	}
	return nil
}
