// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/grafana/pyroscope-go"
)

// ProfilingConfig is the resolved (validated, defaults-applied) profiling configuration. It is
// selfobs-owned and decoupled from internal/config — main.go maps the YAML config into it, exactly
// as it does for ProviderConfig (H4 self identity travels in the fields below).
type ProfilingConfig struct {
	Enabled  bool
	Mode     string // "pull" | "push" (empty treated as pull)
	PullAddr string // pull mode listener, e.g. ":6060"

	PushEndpoint   string
	PushInstanceID string
	PushToken      string

	ServiceNamespace      string // already includes the "-meta" suffix
	DeploymentEnvironment string
	Instance              string // POD_NAME — per-replica identity
}

// StartProfiling starts continuous profiling per cfg and returns a stop func (always non-nil; a
// no-op when disabled) plus an error. Disabled ⇒ pure no-op (no listener, no agent, no global
// state touched). On any start failure it returns a no-op stop AND the error, so main can fatal:
// an operator who enabled profiling must not run silently un-profiled (operationally honest).
func StartProfiling(cfg ProfilingConfig) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !cfg.Enabled {
		return noop, nil
	}
	switch cfg.Mode {
	case "push":
		p, err := pyroscope.Start(buildPyroscopeConfig(cfg))
		if err != nil {
			return noop, fmt.Errorf("selfobs profiling: start pyroscope: %w", err)
		}
		return func(context.Context) error { return p.Stop() }, nil
	default: // "pull" or "" (defaulted in config.Load, but be robust)
		// [#72] The pull listener serves UNAUTHENTICATED pprof (heap/goroutine dumps) on the
		// operator-chosen pull.addr (default :6060, all interfaces — NOT the health port). Exposure is
		// bounded by deployment: bind it to loopback, or restrict :6060 with a NetworkPolicy so only the
		// scraper reaches it. See internal/selfobs/CLAUDE.md.
		ln, err := net.Listen("tcp", cfg.PullAddr)
		if err != nil {
			return noop, fmt.Errorf("selfobs profiling: listen %q: %w", cfg.PullAddr, err)
		}
		return servePprof(ln), nil
	}
}

// servePprof registers the stdlib pprof handlers on a PRIVATE mux (never DefaultServeMux — avoid
// mutating global state and accidentally exposing pprof on the health server) and serves the given
// listener. Returns the server's Shutdown as the stop func.
//
// pprof.Index, registered on the "/debug/pprof/" PREFIX, dispatches the named runtime profiles
// (/heap, /goroutine, /allocs, ...) itself — they are NOT separate handlers. cmdline/profile/
// symbol/trace have dedicated handlers. NOTE: /debug/pprof/mutex and /block exist via Index but
// return EMPTY unless runtime.SetMutexProfileFraction/SetBlockProfileRate are enabled (we don't —
// CPU/heap/goroutine cover the self-APM need).
func servePprof(ln net.Listener) func(context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// [#72] ReadHeaderTimeout bounds slow-header (Slowloris) clients that would otherwise pin a
	// goroutine/FD open indefinitely while dribbling request headers (also satisfies gosec G114). No
	// Read/WriteTimeout: pprof CPU/trace profiles are long-lived streaming responses a whole-request
	// deadline would truncate.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func(ctx context.Context) error { return shutdownServer(ctx, srv) }
}

// shutdownServer drains srv gracefully within ctx's deadline, then FORCE-closes if the drain does not
// finish in time. [#129] A pull-mode pprof request can hold a connection for its whole duration
// (/debug/pprof/profile?seconds=N streams for N seconds), so a plain srv.Shutdown under an unbounded
// context blocks process exit behind that request until the orchestrator's SIGKILL — which drops the
// final self-metrics flush. Bounding the drain and falling back to srv.Close() keeps shutdown (and thus
// process exit) bounded regardless of an in-flight long profile pull. A clean drain (no active request,
// e.g. the ephemeral-listener test) returns nil before the deadline and never force-closes.
func shutdownServer(ctx context.Context, srv *http.Server) error {
	err := srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		_ = srv.Close() // force-close hung connections so the caller's shutdown budget is respected
	}
	return err
}

// buildPyroscopeConfig maps our resolved config into a pyroscope.Config. Pure (no I/O) so it is
// unit-tested directly without a live network. ApplicationName is the fixed product name; the
// distinct self identity (H4) travels in Tags (the -meta namespace + env + per-replica instance).
func buildPyroscopeConfig(cfg ProfilingConfig) pyroscope.Config {
	return pyroscope.Config{
		ApplicationName:   "genai-otel-bridge",
		ServerAddress:     cfg.PushEndpoint,
		BasicAuthUser:     cfg.PushInstanceID,
		BasicAuthPassword: cfg.PushToken,
		Tags: map[string]string{
			"service_namespace":           cfg.ServiceNamespace,
			"deployment_environment_name": cfg.DeploymentEnvironment,
			"service_instance_id":         cfg.Instance,
		},
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
		},
	}
}
