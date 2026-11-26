// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/langsmith"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// knownSourceTypes builds the source-type registry exactly as Build does, so config validation matches
// what the running binary would accept.
func knownSourceTypes() map[string]struct{} {
	reg := source.NewRegistry()
	portkey.Register(reg)
	langsmith.Register(reg)
	return reg.Known()
}

// ValidateConfig runs the same semantic validation as Build (cfg.Validate against the registered source
// types, including the cleartext-credential gate for every OTLP endpoint). cmd calls it before it builds
// any exporter, so a rejected config never sends a request; Build still validates on its own. It lives
// here, not in app.go, to keep the composition root untouched.
func ValidateConfig(cfg *config.Config) error {
	if err := cfg.Validate(knownSourceTypes()); err != nil {
		return fmt.Errorf("config invalid: %w", err)
	}
	return nil
}

var validateEnvRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ValidateConfigFile loads and validates a config file the same way the binary does (schema/known-fields
// + secret substitution + semantic Validate against the registered source types) WITHOUT requiring real
// secrets: any `${ENV}` ref that is unset gets a placeholder so an overlay full of injected refs can be
// checked in CI. Endpoints/URLs get an https placeholder to satisfy the endpoint gate; everything else
// gets a non-empty token. After Validate it also runs app.Build with inert dependencies (no-op
// checkpoint, emitter and coordinator; no network), so every check the source constructors and Build
// make at startup (unknown loop or graph, bootstrap_lookback, content-floor keys, stream-label budget,
// series ownership, no enabled loops) fails here too. Only a file is checked, not the inline
// GENAI_OTEL_BRIDGE_CONFIG form. For a valid config it returns the resolved plan's table and hash
// (tenancy.Plan.Table and Hash; the table holds no secret) and a nil error; otherwise only the error.
// The hash of a config with an unset ${ENV} ref covers its placeholder, so it matches a running
// leader's only when CI has the same environment.
func ValidateConfigFile(path string) (table, hash string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", path, err)
	}
	// Placeholder any unset ${VAR} (scan raw text — refs can appear anywhere, even in comments, which
	// config.Load also resolves). Never clobber a real value the caller has set. [#112] Classify a var
	// by the KEY it is assigned to, not just its name: an endpoint/url/base_url field parameterised by a
	// generically-named env ref (e.g. `endpoint: ${OTLP_GATEWAY}`) still needs an https placeholder or
	// the endpoint gate false-FAILs a valid overlay. (Duration/numeric fields parameterised by an unset
	// ref still can't be meaningfully validated — the "x" placeholder fails ParseDuration; set those in
	// CI env or don't env-parameterise them. The failure direction stays safe: false-FAIL, never
	// false-PASS. Documented on this function.)
	urlKey := regexp.MustCompile(`(?m)^\s*(?:-\s*)?([A-Za-z0-9_]+)\s*:\s*"?\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	urlVars := map[string]bool{}
	for _, m := range urlKey.FindAllStringSubmatch(string(raw), -1) {
		if k := strings.ToLower(m[1]); k == "endpoint" || k == "url" || k == "base_url" {
			urlVars[m[2]] = true
		}
	}
	for _, m := range validateEnvRefRe.FindAllStringSubmatch(string(raw), -1) {
		name := m[1]
		if os.Getenv(name) != "" {
			continue
		}
		val := "x"
		if urlVars[name] || strings.Contains(name, "ENDPOINT") || strings.Contains(name, "URL") || strings.Contains(name, "GATEWAY") {
			val = "https://placeholder.example"
		}
		// #nosec G104 — Setenv on a validated key name; failure is non-fatal (Load will report the unset ref).
		_ = os.Setenv(name, val)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", "", err
	}
	if err := cfg.Validate(knownSourceTypes()); err != nil {
		return "", "", err
	}
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		return "", "", err
	}
	// Constructors only build clients; the first network call is the lazy first Collect, which never runs.
	if _, err := Build(context.Background(), cfg, inertCheckpointer{}, coordinate.Noop{}, inertEmitterFactory, noopTelemetryFactory, Options{}); err != nil {
		return "", "", err
	}
	return plan.Table(), plan.Hash(), nil
}

// inertCheckpointer and inertEmitter let ValidateConfigFile run Build without any store or backend.
type inertCheckpointer struct{}

func (inertCheckpointer) Load(context.Context, model.CheckpointKey) (model.Watermark, error) {
	return model.Watermark{}, nil
}

func (inertCheckpointer) Save(context.Context, model.CheckpointKey, model.Watermark) error {
	return nil
}

type inertEmitter struct{}

func (inertEmitter) Emit(context.Context, model.Batch) error { return nil }

// inertEmitterFactory and noopTelemetryFactory are the factories ValidateConfigFile hands Build: no
// emitter reaches a backend and no self-telemetry is recorded.
func inertEmitterFactory(EmitterSpec, PipelineTelemetry) emit.Emitter { return inertEmitter{} }

func noopTelemetryFactory(string, string) PipelineTelemetry { return noopTelemetry{} }
