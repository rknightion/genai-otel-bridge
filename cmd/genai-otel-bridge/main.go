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

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"go.opentelemetry.io/otel"
	corev1client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rknightion/genai-otel-bridge/internal/app"
	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	cpcm "github.com/rknightion/genai-otel-bridge/internal/checkpoint/configmap"
	cpddb "github.com/rknightion/genai-otel-bridge/internal/checkpoint/dynamodb"
	cpfile "github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/cleanup"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	ddbcoord "github.com/rknightion/genai-otel-bridge/internal/coordinate/dynamodb"
	leasecoord "github.com/rknightion/genai-otel-bridge/internal/coordinate/lease"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/version"
)

// App-created HA state objects. These names are fixed (single-instance chart, see
// deploy/helm/templates/_helpers.tpl) and must match the chart RBAC resourceNames. Defined here as
// the single source of truth shared by buildHA (which creates them) and the -cleanup path (which
// deletes them on uninstall).
const (
	leaseName        = "genai-otel-bridge-leader"
	checkpointCMName = "genai-otel-bridge-checkpoints"
)

func main() {
	cfgPath := flag.String("config", "/etc/genai-otel-bridge/config.yaml", "config file")
	healthAddr := flag.String("health-addr", ":8080", "health endpoint addr")
	ns := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "k8s namespace for lease/configmap")
	identity := flag.String("identity", os.Getenv("POD_NAME"), "leader-election identity")
	memLimit := flag.Int64("container-mem-bytes", 0, "container memory limit for GOMEMLIMIT")
	cpFile := flag.String("checkpoint-file", "/var/lib/genai-otel-bridge/checkpoints.yaml", "path for the file checkpoint store (only used when ha.checkpoint=file)")
	cleanupMode := flag.Bool("cleanup", false, "delete the app-created lease + checkpoint ConfigMap, then exit (the chart's post-delete uninstall hook)")
	cleanupRetainCheckpoint := flag.Bool("cleanup-retain-checkpoint", false, "with -cleanup: keep the checkpoint ConfigMap (only remove the lease) so a reinstall resumes the watermark")
	validateConfigMode := flag.Bool("validate-config", false, "load + validate the -config file (placeholdering unset ${ENV} refs so secrets aren't needed), print the result, and exit 0/1")
	healthCheckMode := flag.Bool("healthcheck", false, "probe the local /healthz derived from -health-addr (a 0.0.0.0/[::] bind, or a bare port, is dialed via 127.0.0.1) and exit 0/1 (ECS container health check; distroless has no shell for curl)")
	versionMode := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	// [#91] Version path: print the ldflags-stamped build version and exit. Importing internal/version
	// (here + in the self-obs resource wiring below) is also what makes the `-X .../version.Version=…`
	// stamp actually LINK into the binary — without a real import the linker drops it and it stays "dev".
	if *versionMode {
		fmt.Println(version.String())
		return
	}

	// Config-validation path: load + schema/semantic-check the -config file and exit. No wiring, no
	// secrets required (unset ${ENV} refs get placeholders). For pre-deploy / CI overlay validation.
	if *validateConfigMode {
		if err := app.ValidateConfigFile(*cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "validate-config: FAIL: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("validate-config: OK (%s)\n", *cfgPath)
		return
	}

	// Uninstall cleanup path (chart post-delete hook). Self-contained: build a client and delete the
	// runtime-created HA objects helm can't track, then exit — no config/emit/coordinator wiring.
	if *cleanupMode {
		runCleanup(*ns, *cleanupRetainCheckpoint)
		return
	}

	// Health-check probe path (ECS container health check). Pure HTTP probe of the local /healthz +
	// exit 0/1 — no config/wiring. distroless has no shell, so ECS runs the binary itself.
	if *healthCheckMode {
		// Not an SSRF risk: the probe target derives from the operator's own -health-addr flag and is
		// rewritten to loopback (127.0.0.1) by localHealthURL — no untrusted input, no trust boundary
		// crossed. Real outbound traffic (vendor/OTLP) goes through httpx's SSRF egress guard, not this
		// path. (Snyk tooling/.snyk suppressions were dropped from the fleet 2026-07-03 — this is a
		// design-level rationale, not a scanner suppression.)
		os.Exit(healthCheckCode(localHealthURL(*healthAddr)))
	}

	// ECS identity fallback: with no -identity/$POD_NAME (ECS has no downward API for the task id), read
	// the Task ARN from the task-metadata endpoint. Must run before *identity is consumed below (logger
	// correlation, self-obs instance, buildHA lock identity). An explicit -identity/env still wins.
	if *identity == "" {
		if id := resolveECSIdentity(os.Getenv("ECS_CONTAINER_METADATA_URI_V4")); id != "" {
			*identity = id
		}
	}

	selfobs.SetMemoryLimit(0.9, *memLimit)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Config delivery: a file at -config, OR (ECS/Fargate, no file mount) the GENAI_OTEL_BRIDGE_CONFIG
	// env var, parsed in-memory (no temp file, so a read-only root filesystem is fine). Either path
	// resolves ${ENV}/file: secret refs at load.
	var cfg *config.Config
	var err error
	if inline := os.Getenv("GENAI_OTEL_BRIDGE_CONFIG"); inline != "" {
		cfg, err = config.LoadBytes([]byte(inline))
	} else {
		cfg, err = config.Load(*cfgPath)
	}
	if err != nil {
		fatal("load config", err)
	}

	// [final-review] All-leader double-emit guard (defence-in-depth with the chart-render guard):
	// ha.coordinator=none disables leader election, so with >1 replica EVERY pod polls + emits the same
	// series (3× source load, duplicate-timestamp churn). The replica count isn't config — the chart
	// injects it via GENAI_OTEL_BRIDGE_REPLICAS. Unset/unparseable ⇒ skip (a raw run without the env can't know it).
	if cfg.HA.Coordinator == "none" {
		if n, perr := strconv.Atoi(os.Getenv("GENAI_OTEL_BRIDGE_REPLICAS")); perr == nil && n > 1 {
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
		"version", version.String(), // [#91] which build is this pod running — observable in logs, not just the image tag
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
		Instance: *identity,        // [CP-H8] POD_NAME → per-replica self-metric identity
		Version:  version.String(), // [#91] service.version on the self resource → correlate a regression to a build
		Interval: selfInterval,
		MaxDPM:   cfg.Governance.MaxDPM,
	})
	if err != nil {
		fatal("selfobs", err)
	}
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

	// Opt-in self-APM tracing (default-off). Like profiling it observes our OWN pipeline, not the data
	// plane; built before the coordinator so spans cover leader work. Traces ride the SAME self endpoint
	// (selfEP) as self-metrics → the same gateway into Tempo (no separate channel). Installed as the OTel
	// GLOBAL provider so schedule's spans light up; disabled ⇒ the global stays the no-op tracer. Start
	// failure is fatal (operationally honest: an operator who enabled tracing must not run silently un-traced).
	tpShutdown := func(context.Context) error { return nil } // no-op unless tracing is enabled
	if cfg.Selfobs.Tracing.Enabled {
		tp, shutdown, err := selfobs.NewTracerProvider(ctx, selfobs.TracingConfig{
			Endpoint: selfEP.Endpoint, InstanceID: selfEP.InstanceID, Token: selfEP.Token,
			ServiceNamespace: cfg.Identity.ServiceNamespace + "-meta", Environment: cfg.Identity.DeploymentEnvironment,
			Instance: *identity,        // POD_NAME — per-replica
			Version:  version.String(), // [#91] service.version on the self trace resource
		})
		if err != nil {
			fatal("selfobs tracing", err)
		}
		otel.SetTracerProvider(tp)
		tpShutdown = shutdown
	}

	partialRejectLim := logging.NewLimiter(time.Minute) // [#80] rate-limit the partial-success WARN (per-plane)
	em := otlp.New(otlp.Config{
		Endpoint: cfg.Emit.Telemetry.OTLP.Endpoint, InstanceID: cfg.Emit.Telemetry.OTLP.InstanceID,
		Token: cfg.Emit.Telemetry.OTLP.Token, MaxBytes: cfg.Queue.MaxBatchBytes,
		// Single source of truth (config.IdentityConfig.ProductIdentity) — the composition root counts the
		// same keys against governance.max_stream_label_keys (Loki stream-label budget), so they can't drift.
		Identity: cfg.Identity.ProductIdentity(),
		// [#60] Emit-leg latency into selfobs (the emit client is outside the httpx observer chokepoint).
		Observer: func(plane string, statusCode int, err error, d time.Duration) {
			metrics.ObserveEmitRequest(plane, statusCode, err, d)
		},
		// [#80] A 200 + partial_success response silently drops N items past the reject taxonomy. Count
		// them into an alertable self-metric AND emit a rate-limited WARN (a persistent partial-reject
		// would otherwise log every emit) — the loop-agnostic emitter owns neither, so it calls back here.
		OnPartialReject: func(plane string, rejected int64, msg string) {
			metrics.ObserveEmitPartialReject(plane, rejected)
			if partialRejectLim.Allow("partial:" + plane) {
				slog.Warn("OTLP gateway rejected part of an emit batch via a 200 partial_success response; the rejected data was NOT delivered (alert on genai_otel_bridge_emit_partial_success_rejected_total)",
					"plane", plane, "rejected", rejected, "gateway_message", msg)
			}
		},
	})

	cp, coord := buildHA(ctx, cfg, *ns, *identity, *cpFile)

	// [CP-C5] /healthz liveness threshold = the worst LEGITIMATE gap between two tick-ATTEMPT beats, so a
	// leader blocked in an intended emit-retry/backpressure, or a degraded loop on its slow backoff, is
	// NOT killed — but a genuinely wedged scheduler (no beat at all) IS. See livenessThreshold.
	health := selfobs.NewHealth(livenessThreshold(cfg))
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
	gracefulShutdown(stop, mpShutdown, tpShutdown, profStop)
	slog.Info("genai-otel-bridge stopped")
}

// shutdownStepTimeout bounds each deferred-shutdown step. [#129] Budgeted well under the orchestrator
// grace period (terminationGracePeriodSeconds, ~300s) so all steps run even if one hangs. A package var
// so tests can shrink it.
var shutdownStepTimeout = 5 * time.Second

// gracefulShutdown runs the process's shutdown funcs in flush-FIRST order, each under its own deadline.
// [#129] Three fixes to the old LIFO `defer …(context.Background())` scheme:
//  1. Signal disposition is reset FIRST (stop()), so a SECOND SIGTERM/SIGINT arriving during a hung
//     shutdown takes the default kill action instead of being delivered to a channel nobody reads —
//     previously the reset was the last-registered defer, so it ran only after everything else.
//  2. The self-metrics flush (mpShutdown) runs BEFORE the pprof drain (profStop), so a hung pprof
//     server drain (an in-flight /debug/pprof/profile?seconds=N pull) can't block the final flush —
//     previously profStop, registered later, ran first under LIFO.
//  3. Every step is deadline-bounded, so no single stop can block process exit until SIGKILL.
func gracefulShutdown(stop func(), mpShutdown, tpShutdown, profStop func(context.Context) error) {
	stop()
	shutdownStep("selfobs metrics flush", mpShutdown)
	shutdownStep("selfobs tracing", tpShutdown)
	shutdownStep("selfobs profiling", profStop)
}

func shutdownStep(what string, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownStepTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		slog.Warn("shutdown step did not complete cleanly", "step", what, "err", err)
	}
}

// buildHA constructs the checkpoint store + coordinator from cfg.HA. This is the ONLY place that knows
// about a specific HA backend: K8s (lease/configmap, via an in-cluster client) or AWS (dynamodb, via the
// SDK default credential chain → the ECS task role). No DynamoDB API call is made here — construction is
// lazy; cfg.Validate (inside app.Build) runs before app.Run, so an empty/invalid table is rejected first.
func buildHA(ctx context.Context, cfg *config.Config, ns, identity, cpFile string) (checkpoint.Checkpointer, coordinate.Coordinator) {
	needK8s := cfg.HA.Coordinator == "lease" || cfg.HA.Checkpoint == "configmap"
	needDynamo := cfg.HA.Coordinator == "dynamodb" || cfg.HA.Checkpoint == "dynamodb"

	var cs corev1client.Interface
	if needK8s {
		rc, err := rest.InClusterConfig()
		if err != nil {
			fatal("in-cluster k8s config", err)
		}
		if cs, err = corev1client.NewForConfig(rc); err != nil {
			fatal("k8s client", err)
		}
	}

	var ddb *awsddb.Client
	if needDynamo {
		var loadOpts []func(*awscfg.LoadOptions) error
		if r := cfg.HA.DynamoDB.Region; r != "" {
			loadOpts = append(loadOpts, awscfg.WithRegion(r))
		}
		acfg, err := awscfg.LoadDefaultConfig(ctx, loadOpts...)
		if err != nil {
			fatal("aws config", err)
		}
		// [#30] Bound every DynamoDB request with an overall HTTP client timeout. aws-sdk-go-v2's default
		// BuildableClient has clientTimeout=0 (no per-request deadline), so a silent/blackholed connection
		// hangs indefinitely — which is exactly what leaves the coordinator's renew stalled past its
		// renew_deadline. The coordinator's out-of-band watchdog (dynamodb.go lead) is the primary defence;
		// this is defence-in-depth that also bounds acquire and the checkpoint Get/Put path. Sized to the
		// renew deadline (default 10s) so a single stalled request fails within one renew cycle.
		clientTimeout := time.Duration(cfg.HA.DynamoDB.RenewDeadline)
		if clientTimeout <= 0 {
			clientTimeout = 10 * time.Second
		}
		ddb = awsddb.NewFromConfig(acfg, func(o *awsddb.Options) {
			o.HTTPClient = awshttp.NewBuildableClient().WithTimeout(clientTimeout)
			if ep := cfg.HA.DynamoDB.Endpoint; ep != "" {
				o.BaseEndpoint = aws.String(ep)
			}
		})
	}

	var cp checkpoint.Checkpointer
	switch cfg.HA.Checkpoint {
	case "configmap":
		cp = cpcm.New(cs, ns, checkpointCMName)
	case "dynamodb":
		cp = cpddb.New(ddb, cfg.HA.DynamoDB.Table, cfg.HA.DynamoDB.KeyPrefix+"ckpt#")
	default: // file
		f, err := cpfile.New(cpFile, false)
		if err != nil {
			fatal("file checkpoint", err)
		}
		cp = f
	}

	// [#87] Fail fast when leader election is enabled but the replica identity is empty. An empty
	// identity is silently unsafe (client-go refuses an empty Lock identity → crash-loop; the DynamoDB
	// path collides self-telemetry across replicas). Better a loud startup exit than a blind run.
	if err := coordinate.RequireIdentity(cfg.HA.Coordinator, identity); err != nil {
		fatal("ha identity", err)
	}

	var coord coordinate.Coordinator = coordinate.Noop{}
	switch cfg.HA.Coordinator {
	case "lease":
		coord = leasecoord.New(cs, ns, leaseName, identity, 15*time.Second, 10*time.Second, 2*time.Second)
	case "dynamodb":
		d := cfg.HA.DynamoDB
		coord = ddbcoord.New(ddb, d.Table, d.KeyPrefix+"lock#"+d.LockName, identity,
			time.Duration(d.LeaseDuration), time.Duration(d.RenewDeadline), time.Duration(d.RetryPeriod))
	default: // "none" (or, pre-validation, any other value) → single-replica Noop
		// [#45] A durable, shared checkpoint that a prior HA (lease/dynamodb) deployment advanced to
		// epoch ≥ 2 permanently fences Noop's writes (Noop stamps epoch 1), spinning the loop re-emitting
		// the same window forever until the checkpoint objects are manually deleted. We cannot read the
		// stored epoch here (checkpoint keys aren't known until the source graph is built), so warn loudly
		// rather than fail. app.Build then completes the auto-heal: it reads the max stored epoch across the
		// built loops and re-constructs the Noop via NoopWithEpoch, so writes aren't fenced after an HA→none
		// downgrade; a fresh single-replica deployment on this backend stays legitimate (effective epoch 1).
		if cfg.HA.Checkpoint == "configmap" || cfg.HA.Checkpoint == "dynamodb" {
			slog.Warn("ha.coordinator=none over a durable checkpoint: if this store was previously advanced by an HA (lease/dynamodb) deployment (epoch ≥ 2), watermark writes will be permanently fenced (checkpoint_fenced) and the loops will re-emit the same window at full cadence. Recovery is manual: delete the checkpoint objects. See internal/coordinate migration note (#45).",
				"checkpoint", cfg.HA.Checkpoint)
		}
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

// livenessThreshold derives the /healthz staleness threshold EXPLICITLY from the real scheduler
// components (not a coincidental constant): max(schedule.DegradedBackoff, slowest ENABLED loop
// cadence) + emit-retry budget + margin. [CP-C5] A leader blocked in an intended emit-retry /
// backpressure window, or a degraded loop on its slow backoff, must not be killed — but a genuinely
// wedged scheduler (no tick-attempt beat) must be. (bucket_settle drives the window_lag staleness
// ALERT, not this heartbeat — beats fire on tick attempts regardless of settle, so it is absent here.)
//
// [#88] Only ENABLED sources/loops contribute — mirroring the config-summary loop and app.Build. A
// disabled/parked slow loop (e.g. a 6h eval loop left in config) must NOT inflate the threshold, or a
// wedged leader would pass liveness for hours instead of minutes before the orchestrator restarts it.
func livenessThreshold(cfg *config.Config) time.Duration {
	const (
		emitRetryBudget = 2 * time.Minute // 3× emit retry (exp backoff ~90s) + checkpoint save + slack
		livenessMargin  = 4 * time.Minute // headroom for scheduling jitter / clock skew
	)
	base := schedule.DegradedBackoff
	for _, sc := range cfg.Sources {
		if !sc.Enabled {
			continue
		}
		for _, lp := range sc.Loops {
			if !lp.Enabled {
				continue
			}
			if c := time.Duration(lp.Cadence); c > base {
				base = c
			}
		}
	}
	return base + emitRetryBudget + livenessMargin
}

func fatal(msg string, err error) { slog.Error(msg, "err", err); os.Exit(1) }
