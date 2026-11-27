// SPDX-License-Identifier: Apache-2.0

// Package config loads, resolves secret refs in, and validates the YAML config (DESIGN §4.1).
// It is intentionally free of a `source` import (no cycle): unknown-type and series-name
// ownership checks are done by the caller (registry/composition root) at construction.
//
// The Helm chart's default `config:` block (deploy/helm/values.yaml) is GENERATED from the structs
// below — their helm:"..." render tags supply values/env-refs and their Go doc-comments become the
// chart's inline comments. After changing any field, tag, default, or doc-comment, regenerate with
// `just gen` (the go:generate runs from the module root so the relative paths resolve);
// TestHelmGeneratedConfigUpToDate gates against drift.
//
//go:generate sh -c "cd ../.. && go run ./internal/config/gen"
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

const maxWindow = 55 * time.Minute
const minCadence = 10 * time.Second
const jitterFrac = 0.10

// defaultPerMetricCardinalityBudget is applied when governance.per_metric_cardinality_budget is unset
// (0). 0 is never passed through to the guard — there it means "unlimited", which is unsafe as a
// silent default. This is a PER-METRIC cap (distinct label-sets per metric name); total cardinality
// across all metrics is the sum and is far higher.
const defaultPerMetricCardinalityBudget = 10000

// defaultMaxDPM is applied when governance.max_dpm is unset (0). 0 would mean "emit nothing", which is
// nonsense, so unset ⇒ 1 (the Grafana Cloud 1-data-point-per-minute default).
const defaultMaxDPM = 1

// defaultMaxCatchupPerTick is applied when governance.max_catchup_per_tick is unset (0). 1 ⇒ one window
// per cadence (no catch-up acceleration) — the conservative v1 behaviour.
const defaultMaxCatchupPerTick = 1

// DefaultMaxStreamLabelKeys is applied when governance.max_stream_label_keys is unset (0). It is the
// Grafana Cloud Loki `max_label_names_per_series` default — the hard ceiling on stream-label names per
// log series; Loki REJECTS (silently drops) a stream that exceeds it. genai-otel-bridge's product identity resource
// attrs PLUS each logs loop's indexed attrs consume this budget once GS1 promotes the indexed attrs to
// stream labels. The limit is per-tenant overridable by Grafana staff, so an operator whose tenant limit
// was raised sets the knob to match. NOTE: in the in-cluster-Alloy topology, Alloy's enrichment attrs
// (k8s.*/cloud.* — also in the Loki default-promoted set) share this same budget, so size with headroom.
const DefaultMaxStreamLabelKeys = 15

// DynamoDB HA defaults — applied in Load when coordinator|checkpoint == dynamodb (ECS target). These
// are the single source of truth: the ECS config generator's render profile (helmgen.ECSProfile)
// carries the SAME values as literal strings, and the package-config gate TestECSProfileDefaultsMatchLoad
// asserts the two agree, so the generated deploy/ecs/terraform/config.example.yaml can never drift
// from what Load actually applies. (Table/Region/Endpoint/KeyPrefix have no default — required or
// resolved from AWS_REGION/the SDK.)
const (
	defaultDynamoDBLockName      = "genai-otel-bridge-leader"
	defaultDynamoDBLeaseDuration = 15 * time.Second
	defaultDynamoDBRenewDeadline = 10 * time.Second
	defaultDynamoDBRetryPeriod   = 2 * time.Second
)

// Rate-limit defaults — applied per source in Load when rps/burst is unset (0). They mirror the
// RateLimitConfig helm:"default=..." render tags (the chart SSOT). A hand-written or env-injected
// config that omits the rate_limit block must NOT reach rate.NewLimiter with burst 0 — burst 0 makes
// x/time/rate reject every Wait(1), so the loop never collects a single sample (issue #39). RPS and
// Burst default INDEPENDENTLY so `rps: 5` with burst omitted still works.
const (
	defaultRateLimitRPS   = 1
	defaultRateLimitBurst = 3
)

// Queue defaults — applied in Load when max_batches/max_batch_bytes is unset (0). They mirror the
// QueueConfig helm:"default=..." render tags (the chart SSOT). A hand-written or env-injected config
// (e.g. the ECS delivery) that sets only the mandatory emit_workers must NOT run with the zero value:
// max_batch_bytes 0 disables the emitter's proactive over-cap split entirely (only the reactive 413 path
// survives). max_batches is accepted for compatibility and has no effect on buffering (see QueueConfig),
// but it keeps the same default so a rendered config and a hand-written one agree (#114, #372).
const (
	defaultQueueMaxBatches    = 256
	defaultQueueMaxBatchBytes = 1 << 20 // 1 MiB
)

// defaultBucketSettle is applied per loop in Load when bucket_settle is unset (0). It mirrors the
// LoopConfig.BucketSettle helm:"default=10m" render tag (live-measured; 3m was insufficient). 0 is not
// a safe silent default: settle=0 emits a bucket the instant it closes — before late arrivals settle
// it — breaking the emit-once-after-settle correctness leg (issue #52). Inert for aggregate-now loops
// (window==0) which ignore settle; a time-bucketed source still fail-fasts on window==0 in its own New.
const defaultBucketSettle = 10 * time.Minute

// Duration is a time.Duration that yaml.v3 can decode from a human string like "60s" or "50m".
// yaml.v3 does not natively decode duration strings into time.Duration, so we wrap it.
type Duration time.Duration

// UnmarshalYAML decodes a YAML scalar string (e.g. "60s", "50m", "3m") via time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", value.Value, err)
	}
	*d = Duration(dur)
	return nil
}

type Config struct {
	Emit       EmitConfig       `yaml:"emit"`
	Identity   IdentityConfig   `yaml:"identity"`
	HA         HAConfig         `yaml:"ha"`
	Queue      QueueConfig      `yaml:"queue"`
	Governance GovernanceConfig `yaml:"governance"`
	Log        LogConfig        `yaml:"log"` // surfaced so operators can flip log.level (e.g. to debug) at bring-up
	Selfobs    SelfobsConfig    `yaml:"selfobs"`
	Sources    []SourceConfig   `yaml:"sources" helm:"instance"` // chart default carries one example source
	// Tenants is the tenant shape (multi-tenancy design §2): a map keyed by tenant id, each with its own
	// identity, destinations and targets. A config with at least one entry has the tenant shape; then
	// sources must be empty. Every value under it is literal: Load refuses an ${ENV} or file: reference
	// there, and secrets are file names (token_file, value_file) read from the tenant's secrets directory.
	Tenants map[string]TenantConfig `yaml:"tenants" helm:"omit"`
	// Defaults holds, per source type, the loop blocks every target of that type uses (the tenant shape
	// only; a legacy config must not set it).
	Defaults map[string]DefaultsConfig `yaml:"defaults" helm:"omit"`
}

// SelfobsConfig groups the integrator's own-observability options that are not the self-metrics
// endpoint (which lives under emit.self): continuous profiling and self-APM tracing.
type SelfobsConfig struct {
	Profiling ProfilingConfig `yaml:"profiling"`
	Tracing   TracingConfig   `yaml:"tracing"`
}

// TracingConfig configures opt-in, default-off self-APM tracing of the integrator's OWN poll/emit
// pipeline (NOT the Portkey/LangSmith data). When enabled, spans are exported over OTLP to the SAME
// self endpoint/auth/identity as self-metrics (emit.self, falling back to emit.telemetry) — they ride
// the same Grafana Cloud gateway into Tempo, no separate egress channel. Default sampler is
// always-on (our pipeline is low-volume). Disabled ⇒ the OTel global stays the no-op tracer (zero cost).
type TracingConfig struct {
	Enabled bool `yaml:"enabled" helm:"default=false"` // master switch; default false
}

// ProfilingConfig configures opt-in, default-off continuous profiling of the integrator's OWN
// runtime (self-APM). Profiles are stack frames from our binary only — they never touch the data
// plane, so the no-content gate is satisfied structurally. Mode selects pull (expose pprof on a
// dedicated listener for an Alloy/k8s-monitoring scrape) or push (the pyroscope-go agent).
type ProfilingConfig struct {
	Enabled bool          `yaml:"enabled" helm:"default=false"` // master switch; default false
	Mode    string        `yaml:"mode" helm:"default=pull"`     // "pull" (default) | "push"
	Pull    ProfilingPull `yaml:"pull"`
	Push    ProfilingPush `yaml:"push" helm:"omit"` // only when mode: push (push-mode profiling config)
}

// ProfilingPull configures pull mode: a dedicated pprof HTTP listener. Unset Addr ⇒ ":6060".
type ProfilingPull struct {
	Addr string `yaml:"addr" helm:"omit"` // Load supplies :6060 in pull mode; rendering it would break a values override to mode: push
}

// ProfilingPush configures push mode: the pyroscope-go agent → Grafana Cloud Profiles. Same
// instance_id:token Basic-auth shape as the OTLP endpoints, so the same https-or-loopback gate applies.
type ProfilingPush struct {
	Endpoint   string `yaml:"endpoint"`
	InstanceID string `yaml:"instance_id"`
	Token      string `yaml:"token"`
}

// GovernanceConfig configures the cardinality/content guard (DESIGN §7 Cdx-H6/H7).
type GovernanceConfig struct {
	// PerMetricCardinalityBudget caps the number of DISTINCT label-sets tracked per metric NAME — not
	// a global cap. Over-budget series are dropped and counted (genai_otel_bridge_guard_dropped_total). Unset (0)
	// defaults to defaultPerMetricCardinalityBudget. The real ceiling is the downstream Mimir/Adaptive
	// Metrics limit (DESIGN §7 GS2/GS3), not this guard.
	PerMetricCardinalityBudget int `yaml:"per_metric_cardinality_budget" helm:"default=10000"`
	// MaxDPM hard-caps every exported series at ≤ N data points per minute of SERIES (sample-timestamp)
	// time, on BOTH planes. Product plane: emit.CoalesceDPM collapses each (series, 60s) group LWW.
	// Self plane: the PeriodicReader interval is clamped to ≥ 60s/MaxDPM. Unset (0) ⇒ defaultMaxDPM (1);
	// negative is rejected. (followup.md §0.)
	MaxDPM int `yaml:"max_dpm" helm:"default=1"`
	// MaxCatchupPerTick bounds how many windows a backlogged loop drains per cadence period: when a loop
	// is more than one window behind (only possible when max_backfill > window, e.g. raised per GS2), the
	// scheduler accelerates the tick interval to drain up to N windows, then takes a cadence breather —
	// bounded and fair, and won't burst the source (each call is still rate-limited). A cursor-driven logs
	// loop mid-window (runs, logs_export) uses the same budget: up to N pages or chunks per cadence (#374).
	// Unset (0) ⇒
	// defaultMaxCatchupPerTick (1) ⇒ one window per cadence, i.e. no acceleration (the v1 behaviour);
	// negative is rejected. (DESIGN §7a Cdx-C13/F44.)
	MaxCatchupPerTick int `yaml:"max_catchup_per_tick" helm:"default=1"`
	// MaxStreamLabelKeys caps how many OTLP resource attributes a single LOGS loop may contribute to a
	// Loki stream: genai-otel-bridge's product identity resource attrs (service.name/service.namespace/
	// deployment.environment.name) PLUS the loop's indexed attrs (its base content-free set ∪
	// settings.extra_indexed_fields). Loki's max_label_names_per_series (Grafana Cloud default 15) REJECTS
	// — silently drops — a stream over the limit, so the composition root fails fast at startup if a loop
	// would exceed it. Unset (0) ⇒ DefaultMaxStreamLabelKeys (15). Raise ONLY to match a Grafana-staff
	// tenant override of max_label_names_per_series. In the in-cluster-Alloy topology, Alloy's k8s.*/cloud.*
	// enrichment attrs share this same budget — leave headroom.
	MaxStreamLabelKeys int `yaml:"max_stream_label_keys" helm:"default=15"`
	// AllowLabelKeys are EXTRA content-free attribute keys the operator opts into the cardinality guard's
	// indexed/label allow-list, ON TOP OF the keys each enabled source already declares (so a default
	// deployment needs nothing here). Use it to promote a content-free log attribute to the indexed tier
	// (OTLP resource attr → Loki stream-label candidate). Default empty.
	//
	// LIMITATION (label promotion): listing a key here only allows it PAST the guard. A promoted
	// attribute becomes a QUERYABLE Loki stream label only if it is in the Grafana OTel gateway's default
	// label configuration; any label NOT in that default needs a Grafana support ticket before
	// `{label="…"}` matches — until promoted stack-side it lands as structured metadata, not a stream
	// label. Content-floor keys (message bodies / injected PII) are REJECTED here at startup.
	AllowLabelKeys []string `yaml:"allow_label_keys" helm:"default="`
}

// LogConfig configures the integrator's own operational log output. Logs go to STDOUT and are scraped
// by the k8s-monitoring collector then sent to Loki (not pushed via OTLP).
type LogConfig struct {
	// Log output format: logfmt (default, key=value) or json.
	Format string `yaml:"format" helm:"default=logfmt"`
	// Minimum level emitted: debug, info (default), warn, or error. Info keeps warnings and errors
	// visible on kubectl logs out of the box; raise to debug at bring-up for per-tick detail.
	Level string `yaml:"level" helm:"default=info"`
}

type EmitConfig struct {
	Telemetry OTLPTarget  `yaml:"telemetry"`
	Self      *OTLPTarget `yaml:"self" helm:"omit"` // optional separate self-telemetry endpoint; falls back to emit.telemetry
}
type OTLPTarget struct {
	OTLP OTLPConn `yaml:"otlp"`
	// MetricInterval is the self-telemetry export period — honoured only for emit.self (the product
	// plane's rate is gated by the per-loop bucket cadence, structurally 1 point/min). Unset ⇒ 60s
	// (selfobs provider default). Must be ≥ 60s to honour the 1-data-point-per-minute (1DPM) constraint.
	// It parses on emit.telemetry too (shared struct) but is a dead knob there, so Validate REJECTS a
	// non-zero emit.telemetry.metric_interval rather than silently ignore it (#113).
	MetricInterval Duration `yaml:"metric_interval" helm:"omit"`
}
type OTLPConn struct {
	// Grafana Cloud OTLP gateway base URL (no trailing /v1/metrics — the emitter appends it).
	Endpoint   string `yaml:"endpoint" helm:"env=GC_OTLP_ENDPOINT"`
	InstanceID string `yaml:"instance_id" helm:"env=GC_INSTANCE_ID"`
	Token      string `yaml:"token" helm:"env=GC_OTLP_TOKEN"`
	// AllowInsecure opts this endpoint out of the https-only gate for an IN-CLUSTER cleartext OTLP
	// receiver — the regulated/EKS topology where genai-otel-bridge emits to an in-cluster Alloy (which holds the
	// real Grafana Cloud credentials) over the pod network rather than straight to the public gateway.
	// [CP-M7] Default false (https or loopback required). When true, an http NON-loopback endpoint is
	// permitted ONLY IF (a) no credential is set — instance_id/token must be empty, so nothing rides the
	// cleartext link (the collector authenticates to Grafana Cloud, not genai-otel-bridge); and (b) the host is a
	// private target — an IP literal must be RFC-1918/loopback/link-local (a public IP is rejected), while
	// a DNS name (e.g. a Kubernetes Service) is permitted since it can't be resolved at config-load time.
	// https endpoints ignore this flag.
	AllowInsecure bool `yaml:"allow_insecure" helm:"default=false"`
}

// SelfNamespaceSuffix is appended to identity.service_namespace to form the service.namespace of the
// bridge's own telemetry (self-metrics, self-traces, self-profiles), so it never shares a namespace
// with the product telemetry it emits (H4).
const SelfNamespaceSuffix = "-meta"

type IdentityConfig struct {
	ServiceNamespace string `yaml:"service_namespace" helm:"default=genai-otel-bridge"`
	// Deployment environment label (e.g. dev, staging, prod).
	DeploymentEnvironment string `yaml:"deployment_environment" helm:"env=ENV"`
}

// ProductIdentity is the OTLP resource-attribute identity map genai-otel-bridge stamps on every emitted PRODUCT
// resource (cmd/genai-otel-bridge passes it to the emitter). All three keys are in the Grafana Cloud Loki
// `default_resource_attributes_as_index_labels` set, so each consumes one max_label_names_per_series
// stream-label slot. SINGLE SOURCE OF TRUTH: cmd/genai-otel-bridge builds the emitter Identity from this, and the
// composition root counts len(ProductIdentity) against governance.max_stream_label_keys — so the two
// can't drift.
func (ic IdentityConfig) ProductIdentity() map[string]string {
	return map[string]string{
		"service.name":                "genai-otel-bridge",
		"service.namespace":           ic.ServiceNamespace,
		"deployment.environment.name": ic.DeploymentEnvironment,
	}
}

type HAConfig struct {
	// lease: use a k8s Lease for leader election (production multi-replica). none: disable HA (single-replica dev/test). dynamodb: DynamoDB lock (ECS/AWS).
	// Downgrading an HA (lease/dynamodb) deployment to none over the SAME durable checkpoint is safe: the bridge reads the stored leader epoch at startup and adopts it, so watermark writes are not permanently fenced (auto-heal, #45).
	Coordinator string `yaml:"coordinator" helm:"default=lease"` // lease | none | dynamodb
	// configmap: durable watermark in a k8s ConfigMap (required with coordinator=lease). file: local file (dev only). dynamodb: DynamoDB item (ECS/AWS).
	Checkpoint string `yaml:"checkpoint" helm:"default=configmap"` // configmap | file | dynamodb
	// DynamoDB-backed HA (ECS). Required when coordinator|checkpoint == dynamodb. ECS-only ⇒ helm:"omit".
	DynamoDB DynamoDBHAConfig `yaml:"dynamodb" helm:"omit"`
}

// DynamoDBHAConfig configures the DynamoDB lock + checkpoint backends (ECS deployment target).
type DynamoDBHAConfig struct {
	Table         string   `yaml:"table" helm:"omit"`          // required; the lock+checkpoint table
	Region        string   `yaml:"region" helm:"omit"`         // default: AWS_REGION env (resolved by the SDK)
	Endpoint      string   `yaml:"endpoint" helm:"omit"`       // optional: dynamodb-local / VPC endpoint override
	LockName      string   `yaml:"lock_name" helm:"omit"`      // default: genai-otel-bridge-leader
	KeyPrefix     string   `yaml:"key_prefix" helm:"omit"`     // optional: prepended to every pk (shared-table isolation)
	LeaseDuration Duration `yaml:"lease_duration" helm:"omit"` // default 15s
	RenewDeadline Duration `yaml:"renew_deadline" helm:"omit"` // default 10s
	RetryPeriod   Duration `yaml:"retry_period" helm:"omit"`   // default 2s
}
type QueueConfig struct {
	// Per-loop queue capacity (batches). Collection is single-flight with the emit, so at most one batch
	// is ever queued or in flight per loop and the capacity is never used: no backlog is buffered. The
	// value is accepted for compatibility only.
	MaxBatches int `yaml:"max_batches" helm:"default=256"`
	// Per-batch size cap in bytes (~1 MiB). An over-cap batch is proactively split before transmit;
	// a 413 from the gateway triggers a reactive split (DESIGN §4.5/C11).
	MaxBatchBytes int `yaml:"max_batch_bytes" helm:"default=1048576"`
	// emit_workers must be 1: per-loop single-flight emit so the watermark advances monotonically to
	// the contiguous successor (DESIGN §4.2/C3; config validates this).
	EmitWorkers int `yaml:"emit_workers" helm:"default=1"`
}
type SourceConfig struct {
	Type    string `yaml:"type" helm:"default=portkey"`
	Enabled bool   `yaml:"enabled" helm:"default=true"`
	// Source API base URL (public control plane). MUST be https unless http.allow_private is set.
	BaseURL string `yaml:"base_url" helm:"default=https://api.portkey.ai/v1"`
	// source_instance is part of the CheckpointKey — it namespaces the watermark. Use a stable,
	// env-scoped identifier; changing it resets the watermark (a new bootstrap).
	SourceInstance string                `yaml:"source_instance" helm:"default=portkey-${ENV}"`
	Auth           AuthConfig            `yaml:"auth"`
	RateLimit      RateLimitConfig       `yaml:"rate_limit"`
	HTTP           HTTPConfig            `yaml:"http" helm:"omit"` // optional user_agent / allow_hosts / allow_private overrides
	Loops          map[string]LoopConfig `yaml:"loops" helm:"key=analytics"`
	// api_key_use_cases maps a human use-case label to the Portkey api-key UUIDs whose traffic it
	// represents. Non-empty ⇒ each enabled loop scopes per entry (api_key_ids = that entry's UUIDs) and
	// stamps a normalised `api_key_use_case` label (metrics) / record attribute (logs). Empty ⇒ today's
	// behaviour (optional per-loop settings.api_key_ids). Setting this AND a per-loop settings.api_key_ids
	// on an enabled loop is a fail-fast error. Only the listed keys are collected (unlisted-key traffic is
	// intentionally out of scope). helm:"omit" — surfaced as a commented example via the example block.
	APIKeyUseCases []APIKeyUseCase `yaml:"api_key_use_cases" helm:"omit"`
	// Scope is the vendor scope (a workspace) this source's API key must read, and nothing else. No config
	// file can set it (yaml:"-"): only the plan package fills it, for a tenant-shape target. A source with a
	// scope sends it on its requests and is asserted against it before it collects (multi-tenancy design
	// §5.1). The zero value is no scope: a legacy source.
	Scope ScopeConfig `yaml:"-"`
}

// ScopeConfig names a source's scope: the vendor's name for it (sent on requests and compared with what
// the key reads) and its id (part of the remembered-match digest only).
type ScopeConfig struct {
	Name string `yaml:"name"`
	ID   string `yaml:"id"`
}

// DefaultsConfig is defaults.<type>: the loop blocks every tenant-shape target of that source type uses.
// A target has no loops block of its own. Load applies the legacy loop defaults (bucket_settle) to each
// block, and Validate holds each enabled block to the legacy loop rules.
type DefaultsConfig struct {
	Loops map[string]LoopConfig `yaml:"loops" helm:"omit"`
}

// TenantConfig is tenants.<id>: one tenant's identity, its destinations and its targets. Destination and
// target names are local to the tenant; there is no syntax for naming another tenant's objects.
type TenantConfig struct {
	// Identity is stamped on this tenant's product telemetry. Required, with both fields; it does not fall
	// back to the install's top-level identity.
	Identity     IdentityConfig                     `yaml:"identity" helm:"omit"`
	Destinations map[string]TenantDestinationConfig `yaml:"destinations" helm:"omit"`
	Targets      map[string]TargetConfig            `yaml:"targets" helm:"omit"`
}

// TenantDestinationConfig is tenants.<id>.destinations.<name>: one OTLP stack of the tenant.
type TenantDestinationConfig struct {
	OTLP TenantOTLPConfig `yaml:"otlp" helm:"omit"`
}

// TenantOTLPConfig is a tenant destination's endpoint and credentials. It has no allow_insecure: a tenant
// destination is https (or loopback, for development) and always carries credentials.
type TenantOTLPConfig struct {
	Endpoint   string `yaml:"endpoint" helm:"omit"`
	InstanceID string `yaml:"instance_id" helm:"omit"`
	// TokenFile is a bare file name in the tenant's secrets directory, never a path or a value.
	TokenFile string `yaml:"token_file" helm:"omit"`
}

// TargetConfig is tenants.<id>.targets.<name>: one source API key, the one scope it reads, the destination
// it sends to and, optionally, an identity that replaces the tenant's whole.
type TargetConfig struct {
	Source      TenantSourceConfig `yaml:"source" helm:"omit"`
	Scope       ScopeConfig        `yaml:"scope" helm:"omit"`
	Destination string             `yaml:"destination" helm:"omit"`
	Identity    *IdentityConfig    `yaml:"identity" helm:"omit"`
}

// TenantSourceConfig is a target's source block. Unlike SourceConfig it has no enabled, source_instance,
// loops or api_key_use_cases key, and its API key is a file name, not a value.
type TenantSourceConfig struct {
	Type      string           `yaml:"type" helm:"omit"`
	BaseURL   string           `yaml:"base_url" helm:"omit"`
	Auth      TenantAuthConfig `yaml:"auth" helm:"omit"`
	RateLimit RateLimitConfig  `yaml:"rate_limit" helm:"omit"`
	HTTP      HTTPConfig       `yaml:"http" helm:"omit"`
}

// TenantAuthConfig names the header a target's API key is sent in and the file that holds the key.
type TenantAuthConfig struct {
	Header string `yaml:"header" helm:"omit"`
	// ValueFile is a bare file name in the tenant's secrets directory, never a path or a value.
	ValueFile string `yaml:"value_file" helm:"omit"`
}

// APIKeyUseCase binds a use-case label to the Portkey api-key UUIDs whose traffic carries it.
type APIKeyUseCase struct {
	// Label is the human use-case name (e.g. "Data Gen"); the portkey source slugifies it.
	Label string `yaml:"label"`
	// APIKeyIDs are Portkey api-key UUIDs (GET /api-keys ids, NOT secrets). Multiple collapse into one
	// labelled series (api_key_ids is a CSV filter).
	APIKeyIDs []string `yaml:"api_key_ids"`
}

type AuthConfig struct {
	Header string `yaml:"header" helm:"default=x-portkey-api-key"`
	Value  string `yaml:"value" helm:"env=PORTKEY_API_KEY"`
}
type RateLimitConfig struct {
	// Sustained request rate (req/s). Portkey's shared budget is ~10 req/10s across tenants — stay well under (DESIGN §15).
	RPS   float64 `yaml:"rps" helm:"default=1"`
	Burst int     `yaml:"burst" helm:"default=3"`
}
type HTTPConfig struct {
	UserAgent    string   `yaml:"user_agent"`
	AllowHosts   []string `yaml:"allow_hosts"`
	AllowPrivate bool     `yaml:"allow_private"`
}
type LoopConfig struct {
	// A loop runs only when enabled is true. Load applies no default: an omitted enabled means off.
	Enabled bool `yaml:"enabled" helm:"default=true"`
	// Poll cadence (± 10% jitter applied by the scheduler).
	Cadence Duration `yaml:"cadence" helm:"default=60s"`
	// Query window; 50m keeps bucket granularity at 1 minute (≤55m clamp; H5/OP5c).
	Window Duration `yaml:"window" helm:"default=50m"`
	// bucket_settle: age at which a source bucket stops changing after first observation. 10m is the
	// live-measured default (3m was insufficient); tune up if late-arrival lag is observed in prod.
	BucketSettle Duration `yaml:"bucket_settle" helm:"default=10m"`
	// On first run (or after a watermark reset), how far back to bootstrap. Must be ≤ max_backfill.
	BootstrapLookback Duration `yaml:"bootstrap_lookback" helm:"default=50m"`
	// Maximum backfill window. Capped by the metrics backend's accept window — on Grafana Cloud the
	// Mimir out_of_order_time_window is 2h (per-tenant; raise via GS2). 90m leaves margin for clock
	// skew + the catch-up walk; samples older than now-max_backfill are abandoned loud
	// (backfill_unstorable_total). NOTE: unrelated to the ≤55m Portkey granularity clamp.
	MaxBackfill  Duration `yaml:"max_backfill" helm:"default=90m"`
	MetricPrefix string   `yaml:"metric_prefix" helm:"default=portkey_api"`
	// Configured graph set. An unknown graph is logged and skipped; an all-unknown response is an error
	// (no silent-healthy advance without data).
	Graphs []string `yaml:"graphs" helm:"default=requests,cost,tokens,latency,errors"`
	// settings carries source-specific knobs as opaque string values — a DECOUPLED extension point so a
	// new source (e.g. an aggregate-now eval platform) can take typed config without leaking vendor field
	// names into this shared struct. Each source package parses the keys it understands; core ignores it.
	// helm:"omit" so it never pollutes the generated portkey example; per-source chart examples render it.
	Settings map[string]string `yaml:"settings" helm:"omit"`
}

// Load reads, secret-resolves, and structurally parses the config. Secret resolution failure
// (unset ${ENV} / missing file:) is fatal here (F21).
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return LoadBytes(raw)
}

// LoadBytes parses, secret-resolves, and structurally parses config from raw YAML bytes — the file-less
// path used by the ECS GENAI_OTEL_BRIDGE_CONFIG env delivery (parsed in-memory, so no temp file is
// needed and a read-only root filesystem is fine). Identical processing to Load; secret-resolution
// failure (unset ${ENV} / missing file:) is fatal here (F21).
func LoadBytes(raw []byte) (*Config, error) {
	// [ext-review-9] ${VAR} can appear in ANY YAML context, including a flow mapping (`{value: ${X}}`)
	// where the '{' '}' of an unresolved ref are flow indicators that make the raw text invalid YAML —
	// so we cannot parse first. And substituting into raw TEXT before parsing (the old approach) let a
	// secret value with YAML-special characters be re-interpreted (e.g. "tok # x" parsed as "tok" + a
	// comment). So: replace each ${VAR} with a YAML-safe placeholder (valid in every context), parse
	// the now-valid tree, then set the REAL value into scalar node VALUES as literal data — yaml never
	// re-interprets it and Marshal re-quotes it when needed. file: refs resolve per whole scalar.
	subst, placeholders, err := injectEnvPlaceholders(string(raw))
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(subst), &root); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	// Multi-tenancy design §2: every value under tenants and defaults is literal. Checked on the parsed
	// tree before resolveSecretsNode, so a refused file: ref is never read and a refused ${ENV} value never
	// enters the tree; an unset ${VAR} has already failed in injectEnvPlaceholders above.
	if err := rejectComplexKeys(&root); err != nil {
		return nil, err
	}
	if err := rejectTenantRefs(&root, placeholders); err != nil {
		return nil, err
	}
	if err := resolveSecretsNode(&root, placeholders); err != nil {
		return nil, err
	}
	resolved, err := yaml.Marshal(&root)
	if err != nil {
		// [#111] `resolved` now carries the REAL secret values in scalar node values, so any error string
		// derived from it could embed a secret (which then reaches stderr → container log → Loki). Redact.
		return nil, redactSecretValues(fmt.Errorf("config: re-marshal after secret resolution: %w", err), placeholders)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(resolved)))
	dec.KnownFields(true) // unknown YAML key ⇒ error
	if err := dec.Decode(&cfg); err != nil {
		// [#111] A decode/type/duration error (e.g. Duration.UnmarshalYAML's `invalid duration %q`) can
		// echo a resolved secret value from `resolved` — redact every resolved-secret substring first.
		return nil, redactSecretValues(fmt.Errorf("config: parse: %w", err), placeholders)
	}
	if cfg.Governance.PerMetricCardinalityBudget == 0 { // unset ⇒ safe default (never silent-unlimited)
		cfg.Governance.PerMetricCardinalityBudget = defaultPerMetricCardinalityBudget
	}
	if cfg.Governance.MaxDPM == 0 { // unset ⇒ 1DPM (never silent "emit nothing")
		cfg.Governance.MaxDPM = defaultMaxDPM
	}
	if cfg.Governance.MaxCatchupPerTick == 0 { // unset ⇒ 1 (no catch-up acceleration)
		cfg.Governance.MaxCatchupPerTick = defaultMaxCatchupPerTick
	}
	if cfg.Governance.MaxStreamLabelKeys == 0 { // unset ⇒ GC Loki max_label_names_per_series default (15)
		cfg.Governance.MaxStreamLabelKeys = DefaultMaxStreamLabelKeys
	}
	if cfg.Queue.MaxBatches == 0 { // #114: unset ⇒ 256, so rendered and hand-written configs agree (inert, #372)
		cfg.Queue.MaxBatches = defaultQueueMaxBatches
	}
	if cfg.Queue.MaxBatchBytes == 0 { // #114: unset ⇒ 1 MiB (else the proactive over-cap split is disabled)
		cfg.Queue.MaxBatchBytes = defaultQueueMaxBatchBytes
	}
	if cfg.Selfobs.Profiling.Enabled {
		if cfg.Selfobs.Profiling.Mode == "" {
			cfg.Selfobs.Profiling.Mode = "pull" // default mode: zero new egress / dependency surface
		}
		if cfg.Selfobs.Profiling.Mode == "pull" && cfg.Selfobs.Profiling.Pull.Addr == "" {
			cfg.Selfobs.Profiling.Pull.Addr = ":6060" // dedicated pprof listener (NOT the public health port)
		}
	}
	if cfg.HA.Coordinator == "dynamodb" || cfg.HA.Checkpoint == "dynamodb" {
		if cfg.HA.DynamoDB.LockName == "" {
			cfg.HA.DynamoDB.LockName = defaultDynamoDBLockName
		}
		if cfg.HA.DynamoDB.LeaseDuration == 0 {
			cfg.HA.DynamoDB.LeaseDuration = Duration(defaultDynamoDBLeaseDuration)
		}
		if cfg.HA.DynamoDB.RenewDeadline == 0 {
			cfg.HA.DynamoDB.RenewDeadline = Duration(defaultDynamoDBRenewDeadline)
		}
		if cfg.HA.DynamoDB.RetryPeriod == 0 {
			cfg.HA.DynamoDB.RetryPeriod = Duration(defaultDynamoDBRetryPeriod)
		}
	}
	for si := range cfg.Sources {
		// #39: default rps/burst independently so an omitted rate_limit block (or a partial one) never
		// reaches rate.NewLimiter with burst 0. Mirrors the RateLimitConfig helm render tags.
		if cfg.Sources[si].RateLimit.RPS == 0 {
			cfg.Sources[si].RateLimit.RPS = defaultRateLimitRPS
		}
		if cfg.Sources[si].RateLimit.Burst == 0 {
			cfg.Sources[si].RateLimit.Burst = defaultRateLimitBurst
		}
		// #52: bucket_settle unset (0) breaks emit-once-after-settle. Apply the field's documented default.
		for ln, lp := range cfg.Sources[si].Loops {
			if lp.BucketSettle == 0 {
				lp.BucketSettle = Duration(defaultBucketSettle)
				cfg.Sources[si].Loops[ln] = lp
			}
		}
	}
	// The same rate-limit and settle defaults for the tenant shape: each target's source, and each loop
	// block under defaults (the blocks every target of that type uses).
	for _, tc := range cfg.Tenants {
		for name, tg := range tc.Targets {
			if tg.Source.RateLimit.RPS == 0 {
				tg.Source.RateLimit.RPS = defaultRateLimitRPS
			}
			if tg.Source.RateLimit.Burst == 0 {
				tg.Source.RateLimit.Burst = defaultRateLimitBurst
			}
			tc.Targets[name] = tg
		}
	}
	for _, dc := range cfg.Defaults {
		for ln, lp := range dc.Loops {
			if lp.BucketSettle == 0 {
				lp.BucketSettle = Duration(defaultBucketSettle)
				dc.Loops[ln] = lp
			}
		}
	}
	return &cfg, nil
}

// rejectTenantRefs refuses an ${ENV} or file: reference anywhere under the top-level tenants and defaults
// keys (multi-tenancy design §2): a scalar, map key included, that holds a placeholder token from
// injectEnvPlaceholders or starts with "file:". Aliases are followed, so a value anchored elsewhere cannot
// carry a reference in. The error names the key path, never the value.
//
// resolveSecretsNode resolves keys as well as values, so a top-level key that is itself a reference could
// become tenants or defaults only after this check had passed it by. Such a key is refused in either
// shape (fail closed): a top-level key is always one of the fixed config section names, so no working
// config spells one as a reference. An alias key is resolved to its anchor before it is compared.
func rejectTenantRefs(root *yaml.Node, ph map[string]string) error {
	top := root
	if top.Kind == yaml.DocumentNode && len(top.Content) == 1 {
		top = top.Content[0]
	}
	var errs []error
	for _, kv := range mergedPairs(top, map[*yaml.Node]bool{}) {
		key := aliasTarget(kv[0])
		if key.Kind != yaml.ScalarNode {
			continue // the strict decoder refuses a complex key
		}
		if isSecretRef(key.Value, ph) {
			errs = append(errs, errors.New("config: a top-level key is an ${ENV} or file: reference; top-level keys are literal section names"))
			continue
		}
		if k := key.Value; k == "tenants" || k == "defaults" {
			walkTenantRefs(k, kv[1], ph, map[*yaml.Node]bool{}, &errs)
		}
	}
	return errors.Join(errs...)
}

// rejectComplexKeys refuses a mapping key that is not a scalar (a list or a mapping, written as a YAML
// complex key or reached through an alias), anywhere in the config. resolveSecretsNode resolves every
// scalar of the tree, keys included, so a reference inside such a key would be resolved, and a file read,
// before any check could see it; no config section takes a complex key, so refusing them all is safe. The
// error names the path of the mapping that holds the key, never the key or a value.
func rejectComplexKeys(root *yaml.Node) error {
	var errs []error
	var walk func(path string, n *yaml.Node, seen map[*yaml.Node]bool)
	walk = func(path string, n *yaml.Node, seen map[*yaml.Node]bool) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(path, c, seen)
			}
		case yaml.AliasNode:
			walk(path, n.Alias, seen)
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(fmt.Sprintf("%s[%d]", path, i), c, seen)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := aliasTarget(n.Content[i]), n.Content[i+1]
				if k.Kind != yaml.ScalarNode {
					where := path
					if where == "" {
						where = "(top level)"
					}
					errs = append(errs, fmt.Errorf("config: %s: a mapping key must be a single value, not a list or a mapping", where))
					continue
				}
				child := k.Value
				if path != "" {
					child = path + "." + k.Value
				}
				if k.Tag == "!!merge" {
					child = path
				}
				walk(child, v, seen)
			}
		}
	}
	walk("", root, map[*yaml.Node]bool{})
	return errors.Join(errs...)
}

// aliasTarget follows an alias node to the node it names.
func aliasTarget(n *yaml.Node) *yaml.Node {
	for seen := 0; n.Kind == yaml.AliasNode && n.Alias != nil && seen < 64; seen++ {
		n = n.Alias
	}
	return n
}

// mergedPairs returns a mapping's key and value pairs, with the pairs of any "<<" merge expanded.
func mergedPairs(n *yaml.Node, seen map[*yaml.Node]bool) [][2]*yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if n.Kind != yaml.MappingNode || seen[n] {
		return nil
	}
	seen[n] = true
	var out [][2]*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Tag == "!!merge" {
			if v.Kind == yaml.SequenceNode {
				for _, m := range v.Content {
					out = append(out, mergedPairs(m, seen)...)
				}
			} else {
				out = append(out, mergedPairs(v, seen)...)
			}
			continue
		}
		out = append(out, [2]*yaml.Node{k, v})
	}
	return out
}

func walkTenantRefs(path string, n *yaml.Node, ph map[string]string, seen map[*yaml.Node]bool, errs *[]error) {
	if seen[n] {
		return
	}
	seen[n] = true
	switch n.Kind {
	case yaml.AliasNode:
		if n.Alias != nil {
			walkTenantRefs(path, n.Alias, ph, seen, errs)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := aliasTarget(n.Content[i]), n.Content[i+1]
			if k.Tag == "!!merge" {
				walkTenantRefs(path, v, ph, seen, errs)
				continue
			}
			if k.Kind == yaml.ScalarNode && isSecretRef(k.Value, ph) {
				*errs = append(*errs, tenantRefError(path+" (a key)"))
				continue
			}
			walkTenantRefs(path+"."+k.Value, v, ph, seen, errs)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			walkTenantRefs(fmt.Sprintf("%s[%d]", path, i), c, ph, seen, errs)
		}
	case yaml.ScalarNode:
		if isSecretRef(n.Value, ph) {
			*errs = append(*errs, tenantRefError(path))
		}
	}
}

// isSecretRef reports whether a parsed scalar is, or contains, a reference resolveSecretsNode would
// resolve: an ${ENV} placeholder token anywhere in it, or a file: prefix.
func isSecretRef(v string, ph map[string]string) bool {
	if strings.HasPrefix(v, "file:") {
		return true
	}
	for tok := range ph {
		if strings.Contains(v, tok) {
			return true
		}
	}
	return false
}

func tenantRefError(path string) error {
	return fmt.Errorf("config: %s: an ${ENV} or file: reference is not allowed under tenants or defaults; every value there is literal, and a secret is a file named by token_file or value_file", path)
}

// [AR-M-sec] Match ONLY ${NAME} (never bare $NAME or a literal $), so a secret value containing
// '$' is never mangled.
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// injectEnvPlaceholders replaces each ${VAR} in the raw text with a YAML-safe alphanumeric token
// (valid as a plain scalar in any context — block, flow, or quoted), resolving the env value now
// (fatal if unset/empty, F21). Returns the substituted text + a token→value map for the node pass.
func injectEnvPlaceholders(s string) (string, map[string]string, error) {
	ph := map[string]string{}
	var firstErr error
	i := 0
	out := envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		key := envRefRe.FindStringSubmatch(m)[1]
		v := os.Getenv(key)
		if v == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("config: required env %q is unset", key)
			}
			return m
		}
		tok := fmt.Sprintf("genai-otel-bridgeXsecretX%dXgenai-otel-bridge", i)
		i++
		ph[tok] = v
		return tok
	})
	if firstErr != nil {
		return "", nil, firstErr
	}
	return out, ph, nil
}

// fileSecretKeyPrefix marks placeholder-map entries that hold a file:-sourced secret (no real
// placeholder token starts with it); they are redacted at any length.
const fileSecretKeyPrefix = "file-secret:"

// redactSecretValues replaces every resolved secret value (the VALUES of the placeholder map) with
// "[redacted]" in err's message [#111]. After secret resolution, a YAML decode/type/duration error can
// echo a resolved config value verbatim; unredacted it reaches stderr → the container log → Loki. This
// is a fatal config-load path (the error is only printed), so the %w chain is intentionally dropped —
// keeping it would re-expose the raw string. Values ≤3 chars are skipped to avoid mangling the message
// with incidental substrings; a real credential is far longer.
func redactSecretValues(err error, ph map[string]string) error {
	if err == nil {
		return nil
	}
	var needles []string
	for tok, v := range ph {
		fromFile := strings.HasPrefix(tok, fileSecretKeyPrefix)
		if v == "" || (len(v) <= 3 && !fromFile) {
			continue
		}
		needles = append(needles, v)
		// yaml.v3 type errors print only the first 7 bytes plus "..." of a scalar longer than 10 bytes.
		if len(v) > 10 {
			needles = append(needles, v[:7]+"...")
		}
	}
	// Longest first (ties by value) so a short secret that is a substring of a longer one cannot be
	// replaced first and split the longer one into pieces that no longer match; map order is random.
	sort.Slice(needles, func(i, j int) bool {
		if len(needles[i]) != len(needles[j]) {
			return len(needles[i]) > len(needles[j])
		}
		return needles[i] < needles[j]
	})
	msg := err.Error()
	for _, n := range needles {
		msg = strings.ReplaceAll(msg, n, "[redacted]")
	}
	return errors.New(msg)
}

// resolveSecretsNode walks the parsed tree and, in scalar VALUES only, substitutes placeholders with
// their real (literal) secret values, then resolves a whole-scalar `file:/path` ref. Leaves Tag/Style
// untouched — the placeholder scalars are !!str, and Marshal switches a plain scalar to a quoted style
// when the literal value would otherwise be YAML-significant.
func resolveSecretsNode(n *yaml.Node, ph map[string]string) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode, yaml.MappingNode:
		for _, c := range n.Content {
			if err := resolveSecretsNode(c, ph); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		val := n.Value
		for tok, secret := range ph {
			if strings.Contains(val, tok) {
				val = strings.ReplaceAll(val, tok, secret)
			}
		}
		if strings.HasPrefix(val, "file:") {
			p := strings.TrimSpace(strings.TrimPrefix(val, "file:"))
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("config: file secret %q: %w", p, err)
			}
			val = strings.TrimSpace(string(b))
			if val == "" { // [#381] an empty/unsynced secret file is fatal, like an unset ${VAR} (F21)
				return fmt.Errorf("config: file secret %q is empty", p)
			}
			// [#381] record it for redaction (the placeholder map doubles as the redaction set).
			ph[fmt.Sprintf("%s%d", fileSecretKeyPrefix, len(ph))] = val
		}
		n.Value = val
	}
	return nil
}

// Validate cross-checks the loaded config. `known` is the set of registered source types.
func (c *Config) Validate(known map[string]struct{}) error {
	var errs []error
	// Multi-tenancy design §2: a config with at least one tenant has the tenant shape, where emit.telemetry
	// must be empty (validateTenantShape); otherwise it is the legacy shape, checked as before.
	tenantShape := len(c.Tenants) > 0
	if !tenantShape && c.Emit.Telemetry.OTLP.Endpoint == "" {
		errs = append(errs, errors.New("emit.telemetry.otlp.endpoint required"))
	}
	if c.Queue.MaxBatches < 0 {
		errs = append(errs, fmt.Errorf("queue.max_batches must be >= 0 (0 means the default %d), got %d", defaultQueueMaxBatches, c.Queue.MaxBatches))
	}
	if c.Queue.MaxBatchBytes < 0 {
		errs = append(errs, fmt.Errorf("queue.max_batch_bytes must be >= 0 (0 means the default %d), got %d", defaultQueueMaxBatchBytes, c.Queue.MaxBatchBytes))
	}
	if c.Queue.EmitWorkers != 1 {
		errs = append(errs, fmt.Errorf("queue.emit_workers must be 1 (per-loop single-flight emit, C3); got %d", c.Queue.EmitWorkers))
	}
	if c.Governance.PerMetricCardinalityBudget < 0 {
		errs = append(errs, fmt.Errorf("governance.per_metric_cardinality_budget must be >= 0 (0 ⇒ default %d), got %d", defaultPerMetricCardinalityBudget, c.Governance.PerMetricCardinalityBudget))
	}
	if c.Governance.MaxDPM < 0 {
		errs = append(errs, fmt.Errorf("governance.max_dpm must be >= 0 (0 ⇒ default %d), got %d", defaultMaxDPM, c.Governance.MaxDPM))
	}
	if c.Governance.MaxCatchupPerTick < 0 {
		errs = append(errs, fmt.Errorf("governance.max_catchup_per_tick must be >= 0 (0 ⇒ default %d), got %d", defaultMaxCatchupPerTick, c.Governance.MaxCatchupPerTick))
	}
	if c.Governance.MaxStreamLabelKeys < 0 {
		errs = append(errs, fmt.Errorf("governance.max_stream_label_keys must be >= 0 (0 ⇒ default %d), got %d", DefaultMaxStreamLabelKeys, c.Governance.MaxStreamLabelKeys))
	}
	switch c.Log.Format {
	case "", "logfmt", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format must be logfmt|json (empty ⇒ logfmt), got %q", c.Log.Format))
	}
	switch c.Log.Level {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level must be debug|info|warn|error (empty ⇒ info), got %q", c.Log.Level))
	}
	switch c.HA.Coordinator {
	case "lease", "none", "dynamodb":
	default:
		errs = append(errs, fmt.Errorf("ha.coordinator must be lease|none|dynamodb, got %q", c.HA.Coordinator))
	}
	switch c.HA.Checkpoint {
	case "configmap", "file", "dynamodb":
	default:
		errs = append(errs, fmt.Errorf("ha.checkpoint must be configmap|file|dynamodb, got %q", c.HA.Checkpoint))
	}
	if c.HA.Coordinator == "lease" && c.HA.Checkpoint == "file" { // [CP-H7/H11]
		errs = append(errs, errors.New("ha.checkpoint=file is unsafe with ha.coordinator=lease (per-pod, not shared across replicas → restart loses the watermark; use configmap)"))
	}
	if c.HA.Coordinator == "dynamodb" && c.HA.Checkpoint != "dynamodb" {
		errs = append(errs, errors.New("ha.coordinator=dynamodb requires ha.checkpoint=dynamodb (they share the table)"))
	}
	if (c.HA.Coordinator == "dynamodb" || c.HA.Checkpoint == "dynamodb") && c.HA.DynamoDB.Table == "" {
		errs = append(errs, errors.New("ha.dynamodb.table is required when coordinator|checkpoint is dynamodb"))
	}
	if c.HA.Coordinator == "dynamodb" {
		// #38: negative durations slip past the ordering checks below (e.g. lease=-5s <= renew=-10s is
		// false, so `lease > renew` wrongly "passes"). Reject them explicitly first.
		if c.HA.DynamoDB.LeaseDuration < 0 || c.HA.DynamoDB.RenewDeadline < 0 || c.HA.DynamoDB.RetryPeriod < 0 {
			errs = append(errs, errors.New("ha.dynamodb.lease_duration/renew_deadline/retry_period must each be >= 0"))
		}
		if c.HA.DynamoDB.LeaseDuration <= c.HA.DynamoDB.RenewDeadline {
			errs = append(errs, errors.New("ha.dynamodb.lease_duration must be > renew_deadline"))
		}
		if c.HA.DynamoDB.RetryPeriod <= 0 {
			errs = append(errs, errors.New("ha.dynamodb.retry_period must be > 0"))
		}
		// #46: the DynamoDB renew ticker cadence IS retry_period (coordinate/dynamodb renew loop). With
		// retry_period >= renew_deadline the lock can expire between renew ticks on every term → the
		// standby takes over while the leader still runs (recurring split-brain + leadership flapping).
		// client-go enforces the analogous renewDeadline > retryPeriod invariant for the k8s path; the
		// DynamoDB params are operator-configurable so the same ordering must be validated here.
		if c.HA.DynamoDB.RetryPeriod >= c.HA.DynamoDB.RenewDeadline {
			errs = append(errs, errors.New("ha.dynamodb.retry_period must be < renew_deadline (retry_period is the renew cadence; retry >= renew lets the lock expire between renews → recurring split-brain)"))
		}
	}
	errs = append(errs, validateEmitEndpoint("emit.telemetry.otlp", c.Emit.Telemetry.OTLP)...) // [CP-M7]
	// #113: MetricInterval lives on the shared OTLPTarget so emit.telemetry.metric_interval parses under
	// KnownFields, but it is never read (main.go only consults emit.self) or validated — a silently dead
	// knob. The product plane's export rate is structurally gated by the per-loop bucket cadence (1DPM),
	// not this field. Reject a non-zero value rather than let it lie about intent (matches the profiling
	// pull/push cross-checks); the self-telemetry interval belongs under emit.self.metric_interval.
	if time.Duration(c.Emit.Telemetry.MetricInterval) != 0 {
		errs = append(errs, errors.New("emit.telemetry.metric_interval is not honoured (the product plane's export rate is gated by the per-loop bucket cadence, 1DPM) — set the self-telemetry export period under emit.self.metric_interval instead"))
	}
	// [round3 MEDIUM-1] the optional self-telemetry endpoint carries the same instance_id:token Basic
	// auth, so the same cleartext-credential gate applies.
	if c.Emit.Self != nil {
		// #41: the documented endpoint fallback to emit.telemetry applies ONLY when the whole emit.self
		// block is absent (nil). A present emit.self with an empty otlp.endpoint does NOT fall back — it
		// exports to a malformed URL (http:///v1/metrics) and the entire self-telemetry plane dies
		// silently. validateEmitEndpoint below no-ops on an empty endpoint, so require it explicitly here.
		if c.Emit.Self.OTLP.Endpoint == "" {
			errs = append(errs, errors.New("emit.self.otlp.endpoint is required when the emit.self block is present (remove emit.self to fall back to emit.telemetry)"))
		}
		errs = append(errs, validateEmitEndpoint("emit.self.otlp", c.Emit.Self.OTLP)...)
		// 1DPM: the self-obs PeriodicReader must not push faster than the configured DPM floor
		// (60s / max_dpm). Unset interval ⇒ 60s (always ≥ the floor for max_dpm≥1). Guard maxDPM≥1
		// so a directly-constructed Config (test path, no Load) can't divide by zero.
		maxDPM := c.Governance.MaxDPM
		if maxDPM < 1 {
			maxDPM = 1
		}
		floor := time.Minute / time.Duration(maxDPM)
		if mi := time.Duration(c.Emit.Self.MetricInterval); mi != 0 && mi < floor {
			errs = append(errs, fmt.Errorf("emit.self.metric_interval %s < %s violates the configured %d-DPM emission constraint", mi, floor, maxDPM))
		}
	}
	// Opt-in self-profiling. Validate only when enabled, so a disabled stale block never blocks startup.
	if p := c.Selfobs.Profiling; p.Enabled {
		switch p.Mode {
		case "", "pull":
			// Pull mode: a stray push.* block means the config lies about intent — reject it (default-deny instinct).
			if p.Push.Endpoint != "" || p.Push.InstanceID != "" || p.Push.Token != "" {
				errs = append(errs, errors.New("selfobs.profiling.push.* set but mode is pull — remove the push block or set mode: push"))
			}
		case "push":
			if p.Push.Endpoint == "" {
				errs = append(errs, errors.New("selfobs.profiling.push.endpoint required when mode: push"))
			} else if insecureURL(p.Push.Endpoint) && !loopbackURL(p.Push.Endpoint) { // [CP-M7] cleartext credential egress
				errs = append(errs, fmt.Errorf("selfobs.profiling.push.endpoint must be https:// (got %q) — cleartext credentials", p.Push.Endpoint))
			}
			if p.Push.InstanceID == "" || p.Push.Token == "" {
				errs = append(errs, errors.New("selfobs.profiling.push.instance_id and token required when mode: push"))
			}
			if p.Pull.Addr != "" {
				errs = append(errs, errors.New("selfobs.profiling.pull.addr set but mode is push — remove it or set mode: pull"))
			}
		default:
			errs = append(errs, fmt.Errorf("selfobs.profiling.mode must be pull|push (empty ⇒ pull), got %q", p.Mode))
		}
	}
	seenInstance := map[string]bool{}
	for _, s := range c.Sources {
		if !s.Enabled {
			continue
		}
		if _, ok := known[s.Type]; !ok {
			errs = append(errs, fmt.Errorf("unknown source type %q", s.Type))
		}
		if s.SourceInstance == "" {
			errs = append(errs, fmt.Errorf("source %q: source_instance required (part of CheckpointKey)", s.Type))
		}
		if seenInstance[s.SourceInstance] {
			errs = append(errs, fmt.Errorf("duplicate source_instance %q", s.SourceInstance))
		}
		seenInstance[s.SourceInstance] = true
		if strings.Contains(s.SourceInstance, "/") { // [CP-H2] CheckpointKey joins fields with '/'
			errs = append(errs, fmt.Errorf("source_instance %q must not contain '/' (CheckpointKey delimiter)", s.SourceInstance))
		}
		if s.BaseURL == "" || (insecureURL(s.BaseURL) && !loopbackURL(s.BaseURL) && !s.HTTP.AllowPrivate) { // [CP-M7/H7]
			errs = append(errs, fmt.Errorf("source %q: base_url must be a non-empty https:// URL (or set http.allow_private for an in-VPC plaintext host); got %q", s.SourceInstance, s.BaseURL))
		}
		if s.Auth.Header == "" || s.Auth.Value == "" { // [CP-H7]
			errs = append(errs, fmt.Errorf("source %q: auth.header and auth.value are required", s.SourceInstance))
		}
		// #39: burst 0 makes x/time/rate reject every request (Wait(1) > burst) — the loop never
		// collects. Load defaults an unset (0) rps/burst; a negative or struct-built zero is caught here
		// before it can reach rate.NewLimiter at construction.
		if s.RateLimit.RPS <= 0 {
			errs = append(errs, fmt.Errorf("source %q: rate_limit.rps must be > 0 (unset ⇒ default %d applied in Load), got %v", s.SourceInstance, defaultRateLimitRPS, s.RateLimit.RPS))
		}
		if s.RateLimit.Burst < 1 {
			errs = append(errs, fmt.Errorf("source %q: rate_limit.burst must be >= 1 (burst 0 fails every request; unset ⇒ default %d applied in Load), got %d", s.SourceInstance, defaultRateLimitBurst, s.RateLimit.Burst))
		}
		for name, lp := range s.Loops {
			errs = append(errs, validateLoop(s.SourceInstance+"/"+name, lp)...)
		}
	}
	if tenantShape {
		errs = append(errs, c.validateTenantShape(known)...)
	} else if len(c.Defaults) > 0 {
		errs = append(errs, errors.New("defaults is allowed only in the tenant shape (a config with at least one tenants entry); a legacy source carries its own loops"))
	}
	return errors.Join(errs...)
}

// validateLoop holds one loop block to the loop rules: a legacy source's loop (field is
// "<source_instance>/<loop>") or a defaults block of the tenant shape (field is its key path). A disabled
// block is not checked.
func validateLoop(field string, lp LoopConfig) []error {
	if !lp.Enabled {
		return nil
	}
	var errs []error
	// #38: config.Duration decodes negative strings ("-10m") fine, and negative settle/backfill
	// silently corrupt the collect window / backfill floor (a negative settle even LOOSENS the M3
	// window check below). Reject any negative per-loop duration up front, naming the field.
	for _, f := range []struct {
		name string
		v    Duration
	}{
		{"cadence", lp.Cadence}, {"window", lp.Window}, {"bucket_settle", lp.BucketSettle},
		{"bootstrap_lookback", lp.BootstrapLookback}, {"max_backfill", lp.MaxBackfill},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Errorf("%s: %s must be >= 0, got %s", field, f.name, time.Duration(f.v)))
		}
	}
	if time.Duration(lp.Cadence) < minCadence {
		errs = append(errs, fmt.Errorf("%s: cadence %s < floor %s", field, time.Duration(lp.Cadence), minCadence))
	}
	// A loop with no window is an aggregate-now (snapshot) loop — it pulls a current total each
	// tick, not a time-bucketed window. The window/settle/backfill checks below encode time-bucket
	// semantics (graphs-style) and are N/A for it; only the cadence floor above applies. (Decoupled:
	// keyed on window==0, not on the source type.) A time-bucketed source MUST reject a
	// non-positive window in its OWN constructor (portkey.New does) — so an omitted window on a
	// bucketed source fails fast at build, not silently no-ops here (AR-HIGH).
	if time.Duration(lp.Window) == 0 {
		return errs
	}
	if time.Duration(lp.Window) > maxWindow {
		errs = append(errs, fmt.Errorf("%s: window %s > %s (would flip bucket granularity, H5)", field, time.Duration(lp.Window), maxWindow))
	}
	// [AR-M-win] worst-case gap between two consecutive jittered ticks is cadence*(1+2·jitter)
	// (a +jitter tick following a −jitter tick), so the window must cover that gap + settle.
	lower := time.Duration(float64(time.Duration(lp.Cadence))*(1+2*jitterFrac)) + time.Duration(lp.BucketSettle)
	if time.Duration(lp.Window) < lower {
		errs = append(errs, fmt.Errorf("%s: window %s < cadence*(1+2·jitter)+settle %s (uncovered time after settle, M3)", field, time.Duration(lp.Window), lower))
	}
	if time.Duration(lp.MaxBackfill) > 0 && time.Duration(lp.BootstrapLookback) > time.Duration(lp.MaxBackfill) { // [CP-H7]
		errs = append(errs, fmt.Errorf("%s: bootstrap_lookback %s > max_backfill %s (first-run would skip storable data)", field, time.Duration(lp.BootstrapLookback), time.Duration(lp.MaxBackfill)))
	}
	return errs
}

// tenantNamePattern is the form of a tenant id and of a destination or target name (design §2).
const tenantNamePattern = `[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?`

var tenantNameRe = regexp.MustCompile(`^` + tenantNamePattern + `$`)

// validateTenantShape holds a tenant-shape config to the load-phase rules of multi-tenancy design §2 and
// §3. The resolve-phase rules (a target's destination exists, the stack-sharing and self-telemetry rules)
// and the assemble-phase rules (scope assertion support, duplicate keys, log-emitting loops) are not
// here. No rule names a vendor setting: a vendor package refuses its own.
func (c *Config) validateTenantShape(known map[string]struct{}) []error {
	var errs []error
	if len(c.Sources) > 0 {
		errs = append(errs, errors.New("sources must be empty in the tenant shape: every pipeline is a target under tenants"))
	}
	if c.Emit.Telemetry != (OTLPTarget{}) {
		errs = append(errs, errors.New("emit.telemetry must be empty in the tenant shape: each target sends to its tenant's destination, and self-telemetry goes to emit.self"))
	}
	if c.Emit.Self == nil {
		errs = append(errs, errors.New("emit.self is required in the tenant shape: self-telemetry never falls back to a tenant's destination"))
	}
	if c.Identity.ServiceNamespace == "" || c.Identity.DeploymentEnvironment == "" {
		errs = append(errs, errors.New("identity: service_namespace and deployment_environment are required in the tenant shape (the install's own identity, for self-telemetry)"))
	}
	if c.HA.Coordinator != "lease" || c.HA.Checkpoint != "configmap" {
		errs = append(errs, fmt.Errorf("ha.coordinator must be lease and ha.checkpoint must be configmap in the tenant shape, got %q and %q", c.HA.Coordinator, c.HA.Checkpoint))
	}
	for _, typ := range sortedKeys(c.Defaults) {
		if _, ok := known[typ]; !ok {
			errs = append(errs, fmt.Errorf("defaults.%s: unknown source type %q", typ, typ))
		}
		loops := c.Defaults[typ].Loops
		for _, name := range sortedKeys(loops) {
			errs = append(errs, validateLoop("defaults."+typ+".loops."+name, loops[name])...)
		}
	}
	// A scope belongs to one tenant: the first tenant (in id order) to name a scope name or id owns it.
	scopeNameOwner := map[string]string{}
	scopeIDOwner := map[string]string{}
	for _, id := range sortedKeys(c.Tenants) {
		tc := c.Tenants[id]
		p := "tenants." + id
		if !tenantNameRe.MatchString(id) {
			errs = append(errs, fmt.Errorf("tenant id %q must match %s", id, tenantNamePattern))
		}
		if tc.Identity.ServiceNamespace == "" || tc.Identity.DeploymentEnvironment == "" {
			errs = append(errs, fmt.Errorf("%s.identity: service_namespace and deployment_environment are required (a tenant's identity does not fall back to the top-level identity)", p))
		}
		if len(tc.Targets) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one target is required", p))
		}
		for _, dn := range sortedKeys(tc.Destinations) {
			dp := p + ".destinations." + dn + ".otlp"
			if !tenantNameRe.MatchString(dn) {
				errs = append(errs, fmt.Errorf("%s: destination name %q must match %s", p, dn, tenantNamePattern))
			}
			o := tc.Destinations[dn].OTLP
			switch {
			case o.Endpoint == "":
				errs = append(errs, fmt.Errorf("%s.endpoint is required", dp))
			case insecureURL(o.Endpoint) && !loopbackURL(o.Endpoint): // [CP-M7]
				errs = append(errs, fmt.Errorf("%s.endpoint must be https:// (or loopback for development), got %q: a tenant destination has no setting that allows a plain-text endpoint", dp, o.Endpoint))
			}
			if o.InstanceID == "" {
				errs = append(errs, fmt.Errorf("%s.instance_id is required: a receiver that takes no credentials has no boundary between tenants", dp))
			}
			errs = append(errs, bareSecretFileName(dp+".token_file", o.TokenFile)...)
		}
		for _, tn := range sortedKeys(tc.Targets) {
			tg := tc.Targets[tn]
			tp := p + ".targets." + tn
			if !tenantNameRe.MatchString(tn) {
				errs = append(errs, fmt.Errorf("%s: target name %q must match %s", p, tn, tenantNamePattern))
			}
			errs = append(errs, c.validateTargetSource(tp+".source", tg.Source, known)...)
			if tg.Destination == "" {
				errs = append(errs, fmt.Errorf("%s.destination is required", tp))
			}
			if tg.Identity != nil && (tg.Identity.ServiceNamespace == "" || tg.Identity.DeploymentEnvironment == "") {
				errs = append(errs, fmt.Errorf("%s.identity: service_namespace and deployment_environment are both required when the block is present (it replaces the tenant's identity whole)", tp))
			}
			for _, f := range []struct {
				field, value string
				owner        map[string]string
			}{
				{"name", tg.Scope.Name, scopeNameOwner},
				{"id", tg.Scope.ID, scopeIDOwner},
			} {
				switch {
				case f.value == "":
					errs = append(errs, fmt.Errorf("%s.scope.%s is required: a target reads one scope, named and asserted", tp, f.field))
				case strings.ContainsFunc(f.value, unicode.IsControl):
					errs = append(errs, fmt.Errorf("%s.scope.%s must not contain a control character", tp, f.field))
				}
				if f.value == "" {
					continue
				}
				if owner, ok := f.owner[f.value]; !ok {
					f.owner[f.value] = id
				} else if owner != id {
					errs = append(errs, fmt.Errorf("scope %s %q is used by tenants %s and %s: a scope belongs to one tenant", f.field, f.value, owner, id))
				}
			}
		}
	}
	return errs
}

// validateTargetSource checks a target's source block: a known type with a defaults block (every target
// takes its loops from defaults.<type>.loops), the base_url rule a legacy source has, the auth header and
// key file, and the rate limit after Load's defaults.
func (c *Config) validateTargetSource(sp string, s TenantSourceConfig, known map[string]struct{}) []error {
	var errs []error
	switch _, isKnown := known[s.Type]; {
	case s.Type == "":
		errs = append(errs, fmt.Errorf("%s.type is required", sp))
	case !isKnown:
		errs = append(errs, fmt.Errorf("%s.type: unknown source type %q", sp, s.Type))
	default:
		if _, ok := c.Defaults[s.Type]; !ok {
			errs = append(errs, fmt.Errorf("%s.type %q has no defaults.%s block: every target takes its loops from defaults.<type>.loops", sp, s.Type, s.Type))
		}
	}
	if s.BaseURL == "" || (insecureURL(s.BaseURL) && !loopbackURL(s.BaseURL) && !s.HTTP.AllowPrivate) { // [CP-M7/H7]
		errs = append(errs, fmt.Errorf("%s.base_url must be a non-empty https:// URL (or set http.allow_private for an in-VPC plaintext host); got %q", sp, s.BaseURL))
	}
	if s.Auth.Header == "" {
		errs = append(errs, fmt.Errorf("%s.auth.header is required", sp))
	}
	errs = append(errs, bareSecretFileName(sp+".auth.value_file", s.Auth.ValueFile)...)
	if s.RateLimit.RPS <= 0 {
		errs = append(errs, fmt.Errorf("%s.rate_limit.rps must be > 0 (unset ⇒ default %d applied in Load), got %v", sp, defaultRateLimitRPS, s.RateLimit.RPS))
	}
	if s.RateLimit.Burst < 1 {
		errs = append(errs, fmt.Errorf("%s.rate_limit.burst must be >= 1 (burst 0 fails every request; unset ⇒ default %d applied in Load), got %d", sp, defaultRateLimitBurst, s.RateLimit.Burst))
	}
	return errs
}

// bareSecretFileName requires a tenant secret to be named by a bare file name in the tenant's own secrets
// directory: no path separator, and not "." or "..", so one tenant's block cannot name another's file.
func bareSecretFileName(field, name string) []error {
	switch {
	case name == "":
		return []error{fmt.Errorf("%s is required", field)}
	case strings.Contains(name, "/") || name == "." || name == "..":
		return []error{fmt.Errorf("%s must be a bare file name in the tenant's secrets directory (no '/', not . or ..), got %q", field, name)}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateEmitEndpoint enforces the [CP-M7] cleartext-credential gate for one OTLP emit endpoint
// (telemetry or self). https or genuine loopback is always accepted. An http NON-loopback endpoint is
// rejected unless allow_insecure is set, and even then only token-less (no credential rides cleartext —
// the in-cluster collector holds the Grafana Cloud credentials) and only for a private target (a public
// IP literal is rejected; a DNS host is accepted, being unresolvable at config-load time). `field` names
// the config path for the error message.
func validateEmitEndpoint(field string, c OTLPConn) []error {
	ep := c.Endpoint
	if ep == "" || !insecureURL(ep) || loopbackURL(ep) {
		return nil // unset, https, or genuine loopback — always fine
	}
	if !c.AllowInsecure {
		return []error{fmt.Errorf("%s.endpoint must be https:// (got %q) — cleartext credentials; set %s.allow_insecure to emit to a token-less in-cluster cleartext receiver", field, ep, field)}
	}
	var errs []error
	if c.Token != "" || c.InstanceID != "" {
		errs = append(errs, fmt.Errorf("%s.allow_insecure is set on a cleartext (http) endpoint but instance_id/token are also set — credentials must not ride a cleartext link; the in-cluster collector holds the Grafana Cloud credentials, so point allow_insecure at a token-less in-cluster receiver", field))
	}
	if ipLiteralIsPublic(ep) {
		errs = append(errs, fmt.Errorf("%s.allow_insecure permits cleartext only for private/in-cluster targets; %q is a public address", field, ep))
	}
	return errs
}

// ipLiteralIsPublic reports whether the URL's host is an IP LITERAL that is not private/loopback/
// link-local. A DNS hostname (a Kubernetes Service name, etc.) returns false — it can't be resolved at
// config-load time, so we accept it under validateEmitEndpoint's token-less guarantee rather than do a
// flaky/TOCTOU startup DNS lookup. NOTE: link-local (e.g. the 169.254.169.254 cloud-metadata IMDS) is
// treated as private/allowed — harmless for a token-less EMIT target (nothing to exfiltrate), but if this
// helper is ever reused for an outbound FETCH/SSRF context it would need an explicit IMDS carve-out.
func ipLiteralIsPublic(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return false // DNS name, not an IP literal
	}
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

// [CP-M7] URL scheme helpers: require https for real endpoints, but permit http on loopback (dev/
// tests) and — for a source base_url — an explicitly opted-in in-VPC plaintext host (allow_private).
func insecureURL(u string) bool { return !strings.HasPrefix(u, "https://") }

// loopbackURL reports whether u points at a genuine loopback host. [ext-review-6] It PARSES the URL
// and checks the host exactly — a prefix match (HasPrefix "http://localhost") wrongly accepts
// "http://localhost.evil.example", which would let cleartext credentials egress to an external host.
func loopbackURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
