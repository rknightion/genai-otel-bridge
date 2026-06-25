// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	corev1client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rknightion/genai-otel-bridge/internal/app"
	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	cpcm "github.com/rknightion/genai-otel-bridge/internal/checkpoint/configmap"
	cpfile "github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/cleanup"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	leasecoord "github.com/rknightion/genai-otel-bridge/internal/coordinate/lease"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/source"
)

// App-created HA state objects. These names are fixed (single-instance chart, see
// deploy/helm/templates/_helpers.tpl) and must match the chart RBAC resourceNames. Defined here as
// the single source of truth shared by buildHA (which creates them) and the -cleanup path (which
// deletes them on uninstall).
const (
	leaseName        = "decant-leader"
	checkpointCMName = "decant-checkpoints"
)

func main() {
	cfgPath := flag.String("config", "/etc/decant/config.yaml", "config file")
	healthAddr := flag.String("health-addr", ":8080", "health endpoint addr")
	ns := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "k8s namespace for lease/configmap")
	identity := flag.String("identity", os.Getenv("POD_NAME"), "leader-election identity")
	memLimit := flag.Int64("container-mem-bytes", 0, "container memory limit for GOMEMLIMIT")
	cpFile := flag.String("checkpoint-file", "/var/lib/decant/checkpoints.yaml", "path for the file checkpoint store (only used when ha.checkpoint=file)")
	cleanupMode := flag.Bool("cleanup", false, "delete the app-created lease + checkpoint ConfigMap, then exit (the chart's post-delete uninstall hook)")
	cleanupRetainCheckpoint := flag.Bool("cleanup-retain-checkpoint", false, "with -cleanup: keep the checkpoint ConfigMap (only remove the lease) so a reinstall resumes the watermark")
	flag.Parse()

	// Uninstall cleanup path (chart post-delete hook). Self-contained: build a client and delete the
	// runtime-created HA objects helm can't track, then exit — no config/emit/coordinator wiring.
	if *cleanupMode {
		runCleanup(*ns, *cleanupRetainCheckpoint)
		return
	}

	selfobs.SetMemoryLimit(0.9, *memLimit)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config", err)
	}

	// [final-review] All-leader double-emit guard (defence-in-depth with the chart-render guard):
	// ha.coordinator=none disables leader election, so with >1 replica EVERY pod polls + emits the same
	// series (3× source load, duplicate-timestamp churn). The replica count isn't config — the chart
	// injects it via DECANT_REPLICAS. Unset/unparseable ⇒ skip (a raw run without the env can't know it).
	if cfg.HA.Coordinator == "none" {
		if n, perr := strconv.Atoi(os.Getenv("DECANT_REPLICAS")); perr == nil && n > 1 {
			fatal("config", fmt.Errorf("ha.coordinator=none with replicas=%d: all replicas would be leaders and double-emit — use ha.coordinator=lease for multi-replica HA, or replicas: 1", n))
		}
	}

	// Operational logs → STDOUT (logfmt by default), scraped by the k8s-monitoring collector → Loki;
	// NOT pushed via OTLP. Set as the process default so every slog call downstream is structured.
	logHandler, err := logging.NewHandler(cfg.Log.Format, cfg.Log.Level, os.Stdout)
	if err != nil {
		fatal("log handler", err)
	}
	logger := slog.New(logHandler)
	if *identity != "" {
		logger = logger.With("instance", *identity) // correlate stdout logs with the per-replica metric identity
	}
	slog.SetDefault(logger)

	// Config-loaded summary: a one-line "what am I running" signal at startup (Info, so it shows at the
	// default level). Endpoint HOST only — never the token/instance_id (separate credential fields, not in
	// the URL). Gives operators a positive confirmation the process came up with the expected config.
	telemetryHost := cfg.Emit.Telemetry.OTLP.Endpoint
	if u, perr := url.Parse(cfg.Emit.Telemetry.OTLP.Endpoint); perr == nil && u.Host != "" {
		telemetryHost = u.Host
	}
	enabledSources, enabledLoops := 0, 0
	for _, sc := range cfg.Sources {
		if !sc.Enabled {
			continue
		}
		enabledSources++
		for _, lp := range sc.Loops {
			if lp.Enabled {
				enabledLoops++
			}
		}
	}
	slog.Info("config loaded",
		"sources_enabled", enabledSources, "loops_enabled", enabledLoops,
		"telemetry_endpoint", telemetryHost, "self_endpoint", cfg.Emit.Self != nil,
		"profiling", cfg.Selfobs.Profiling.Enabled, "tracing", cfg.Selfobs.Tracing.Enabled,
		"log_level", cfg.Log.Level)

	// Self-observability provider (distinct identity, H4). Falls back to the telemetry endpoint.
	// NOTE [CP-M7]: this and the product emitter below are CONSTRUCTED here but do not emit until
	// app.Run; cfg.Validate (inside app.Build, further down) runs the cleartext-credential gate before
	// any network emit, so emit-bearing wiring must stay after that gate — don't reorder an actual emit
	// above app.Build.
	selfEP := cfg.Emit.Telemetry.OTLP
	var selfInterval time.Duration // 0 ⇒ provider default (60s, 1DPM)
	if cfg.Emit.Self != nil {
		selfEP = cfg.Emit.Self.OTLP
		selfInterval = time.Duration(cfg.Emit.Self.MetricInterval)
	}
	mp, mpShutdown, err := selfobs.NewProvider(ctx, selfobs.ProviderConfig{
		Endpoint: selfEP.Endpoint, InstanceID: selfEP.InstanceID, Token: selfEP.Token,
		ServiceNamespace: cfg.Identity.ServiceNamespace + "-meta", Environment: cfg.Identity.DeploymentEnvironment,
		Instance: *identity, // [CP-H8] POD_NAME → per-replica self-metric identity
		Interval: selfInterval,
		MaxDPM:   cfg.Governance.MaxDPM,
	})
	if err != nil {
		fatal("selfobs", err)
	}
	defer func() { _ = mpShutdown(context.Background()) }()
	metrics, err := selfobs.NewMetrics(mp)
	if err != nil {
		fatal("selfobs metrics", err)
	}

	// Opt-in self-profiling (default-off). Started before the coordinator so it runs on leader AND
	// standby — it observes the process, not the data plane. Failure to start is fatal: an operator
	// who enabled profiling must not run silently un-profiled (operationally honest).
	profStop, err := selfobs.StartProfiling(selfobs.ProfilingConfig{
		Enabled:               cfg.Selfobs.Profiling.Enabled,
		Mode:                  cfg.Selfobs.Profiling.Mode,
		PullAddr:              cfg.Selfobs.Profiling.Pull.Addr,
		PushEndpoint:          cfg.Selfobs.Profiling.Push.Endpoint,
		PushInstanceID:        cfg.Selfobs.Profiling.Push.InstanceID,
		PushToken:             cfg.Selfobs.Profiling.Push.Token,
		ServiceNamespace:      cfg.Identity.ServiceNamespace + "-meta", // [H4] distinct self identity
		DeploymentEnvironment: cfg.Identity.DeploymentEnvironment,
		Instance:              *identity, // POD_NAME — per-replica
	})
	if err != nil {
		fatal("selfobs profiling", err)
	}
	defer func() { _ = profStop(context.Background()) }()

	// Opt-in self-APM tracing (default-off). Like profiling it observes our OWN pipeline, not the data
	// plane; built before the coordinator so spans cover leader work. Traces ride the SAME self endpoint
	// (selfEP) as self-metrics → the same gateway into Tempo (no separate channel). Installed as the OTel
	// GLOBAL provider so schedule's spans light up; disabled ⇒ the global stays the no-op tracer. Start
	// failure is fatal (operationally honest: an operator who enabled tracing must not run silently un-traced).
	if cfg.Selfobs.Tracing.Enabled {
		tp, tpShutdown, err := selfobs.NewTracerProvider(ctx, selfobs.TracingConfig{
			Endpoint: selfEP.Endpoint, InstanceID: selfEP.InstanceID, Token: selfEP.Token,
			ServiceNamespace: cfg.Identity.ServiceNamespace + "-meta", Environment: cfg.Identity.DeploymentEnvironment,
			Instance: *identity, // POD_NAME — per-replica
		})
		if err != nil {
			fatal("selfobs tracing", err)
		}
		otel.SetTracerProvider(tp)
		defer func() { _ = tpShutdown(context.Background()) }()
	}

	em := otlp.New(otlp.Config{
		Endpoint: cfg.Emit.Telemetry.OTLP.Endpoint, InstanceID: cfg.Emit.Telemetry.OTLP.InstanceID,
		Token: cfg.Emit.Telemetry.OTLP.Token, MaxBytes: cfg.Queue.MaxBatchBytes,
		// Single source of truth (config.IdentityConfig.ProductIdentity) — the composition root counts the
		// same keys against governance.max_stream_label_keys (Loki stream-label budget), so they can't drift.
		Identity: cfg.Identity.ProductIdentity(),
	})

	cp, coord := buildHA(cfg, *ns, *identity, *cpFile)

	// [CP-C5] /healthz liveness threshold = the worst LEGITIMATE gap between two tick-ATTEMPT beats, so a
	// leader blocked in an intended emit-retry/backpressure, or a degraded loop on its slow backoff, is
	// NOT killed — but a genuinely wedged scheduler (no beat at all) IS. Derived EXPLICITLY from the real
	// components (not a coincidental constant): max(DegradedBackoff, slowest enabled cadence) +
	// emit-retry budget + margin. (bucket_settle drives the window_lag staleness ALERT, not this
	// heartbeat — beats fire on tick attempts regardless of settle, so it is intentionally absent here.)
	const (
		emitRetryBudget = 2 * time.Minute // 3× emit retry (exp backoff ~90s) + checkpoint save + slack
		livenessMargin  = 4 * time.Minute // headroom for scheduling jitter / clock skew
	)
	base := schedule.DegradedBackoff
	for _, sc := range cfg.Sources {
		for _, lp := range sc.Loops {
			if c := time.Duration(lp.Cadence); c > base {
				base = c
			}
		}
	}
	threshold := base + emitRetryBudget + livenessMargin
	health := selfobs.NewHealth(threshold)
	// Wire the upstream-request self-obs histogram into every source's HTTP client (decoupled: httpx
	// emits a RequestInfo, selfobs records it; neither imports the other).
	deps := source.Deps{UpstreamObserver: func(i httpx.RequestInfo) {
		metrics.ObserveUpstreamRequest(i.Target, i.Method, i.StatusCode, i.Err, i.Duration)
	}}
	a, err := app.Build(ctx, cfg, cp, coord, em, metrics, deps)
	if err != nil {
		fatal("build", err)
	}
	if err := a.Run(ctx, health.Handler(), *healthAddr, health.MarkReady, health.Beat, health.SetLeader); err != nil && ctx.Err() == nil {
		fatal("run", err)
	}
	slog.Info("decant stopped")
}

func buildHA(cfg *config.Config, ns, identity, cpFile string) (checkpoint.Checkpointer, coordinate.Coordinator) {
	needK8s := cfg.HA.Coordinator == "lease" || cfg.HA.Checkpoint == "configmap"
	var cs corev1client.Interface
	if needK8s {
		rc, err := rest.InClusterConfig()
		if err != nil {
			fatal("in-cluster k8s config", err)
		}
		cs, err = corev1client.NewForConfig(rc)
		if err != nil {
			fatal("k8s client", err)
		}
	}
	var cp checkpoint.Checkpointer
	if cfg.HA.Checkpoint == "configmap" {
		cp = cpcm.New(cs, ns, checkpointCMName)
	} else {
		f, err := cpfile.New(cpFile, false)
		if err != nil {
			fatal("file checkpoint", err)
		}
		cp = f
	}
	var coord coordinate.Coordinator = coordinate.Noop{}
	if cfg.HA.Coordinator == "lease" {
		coord = leasecoord.New(cs, ns, leaseName, identity, 15*time.Second, 10*time.Second, 2*time.Second)
	}
	return cp, coord
}

// runCleanup deletes the runtime-created HA objects (lease + checkpoint ConfigMap) and exits. Invoked
// by the chart's post-delete hook so `helm uninstall` leaves no orphans. Self-contained: it needs
// only an in-cluster client and the namespace (no config load, no emit/coordinator wiring).
func runCleanup(ns string, retainCheckpoint bool) {
	if ns == "" {
		fatal("cleanup", fmt.Errorf("namespace is required (-namespace or POD_NAMESPACE)"))
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		fatal("cleanup: in-cluster k8s config", err)
	}
	cs, err := corev1client.NewForConfig(rc)
	if err != nil {
		fatal("cleanup: k8s client", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cleanup.Run(ctx, cs, ns, leaseName, checkpointCMName, retainCheckpoint); err != nil {
		fatal("cleanup", err)
	}
	slog.Info("cleanup complete", "namespace", ns, "lease", leaseName, "checkpoint_retained", retainCheckpoint)
}

func fatal(msg string, err error) { slog.Error(msg, "err", err); os.Exit(1) }
