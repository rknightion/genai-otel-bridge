// SPDX-License-Identifier: AGPL-3.0-only

// Package logging builds the integrator's own operational-log handler. Logs are written to STDOUT
// (by the caller) and scraped by the k8s-monitoring collector → Loki — they are NOT pushed via OTLP.
// logfmt (slog's stdlib TextHandler) is the default: smaller lines and a cheaper Loki parser than
// JSON for these flat key=value logs (see docs/superpowers/specs/logfmt-spike.md, Path A).
package logging

import (
	"fmt"
	"io"
	"log/slog"
)

// NewHandler returns an slog.Handler for the given format and minimum level. Format: "logfmt" (or "" —
// the default) uses the stdlib TextHandler (logfmt-style key=value, special values quoted); "json" uses
// JSONHandler. Level: "debug"|"info"|"warn"|"error" (or "" — the default, info) sets the floor below
// which records are dropped. Any other format/level is an error so a config typo fails fast rather than
// silently picking a default.
func NewHandler(format, level string, w io.Writer) (slog.Handler, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "", "logfmt":
		return slog.NewTextHandler(w, opts), nil
	case "json":
		return slog.NewJSONHandler(w, opts), nil
	default:
		return nil, fmt.Errorf("logging: unknown format %q (want logfmt|json)", format)
	}
}

// parseLevel maps the config string to an slog.Level; "" ⇒ Info (the operational default — warnings and
// errors are always visible so `kubectl logs` is a useful triage surface).
func parseLevel(level string) (slog.Level, error) {
	switch level {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unknown level %q (want debug|info|warn|error)", level)
	}
}
