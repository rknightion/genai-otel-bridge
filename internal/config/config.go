// SPDX-License-Identifier: AGPL-3.0-only

// Package config loads, resolves secret refs in, and validates the YAML config (DESIGN §4.1).
// It is intentionally free of a `source` import (no cycle): unknown-type and series-name
// ownership checks are done by the caller (registry/composition root) at construction.
//
// The Helm chart's default `config:` block (deploy/helm/values.yaml) is GENERATED from the structs
// below — their helm:"..." render tags supply values/env-refs and their Go doc-comments become the
// chart's inline comments. After changing any field, tag, default, or doc-comment, regenerate with
// `make generate` (the go:generate runs from the module root so the relative paths resolve);
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
	"strings"
	"time"

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
// log series; Loki REJECTS (silently drops) a stream that exceeds it. aip-oi's product identity resource
// attrs PLUS each logs loop's indexed attrs consume this budget once GS1 promotes the indexed attrs to
// stream labels. The limit is per-tenant overridable by Grafana staff, so an operator whose tenant limit
// was raised sets the knob to match. NOTE: in the in-cluster-Alloy topology, Alloy's enrichment attrs
// (k8s.*/cloud.* — also in the Loki default-promoted set) share this same budget, so size with headroom.
const DefaultMaxStreamLabelKeys = 15

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
	Addr string `yaml:"addr" helm:"default=:6060"`
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
	// a global cap. Over-budget series are dropped and counted (aip_oi_guard_dropped_total). Unset (0)
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
	// bounded and fair, and won't burst the source (each call is still rate-limited). Unset (0) ⇒
	// defaultMaxCatchupPerTick (1) ⇒ one window per cadence, i.e. no acceleration (the v1 behaviour);
	// negative is rejected. (DESIGN §7a Cdx-C13/F44.)
	MaxCatchupPerTick int `yaml:"max_catchup_per_tick" helm:"default=1"`
	// MaxStreamLabelKeys caps how many OTLP resource attributes a single LOGS loop may contribute to a
	// Loki stream: aip-oi's product identity resource attrs (service.name/service.namespace/
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
	MetricInterval Duration `yaml:"metric_interval" helm:"omit"`
}
type OTLPConn struct {
	// Grafana Cloud OTLP gateway base URL (no trailing /v1/metrics — the emitter appends it).
	Endpoint   string `yaml:"endpoint" helm:"env=GC_OTLP_ENDPOINT"`
	InstanceID string `yaml:"instance_id" helm:"env=GC_INSTANCE_ID"`
	Token      string `yaml:"token" helm:"env=GC_OTLP_TOKEN"`
	// AllowInsecure opts this endpoint out of the https-only gate for an IN-CLUSTER cleartext OTLP
	// receiver — the regulated/EKS topology where aip-oi emits to an in-cluster Alloy (which holds the
	// real Grafana Cloud credentials) over the pod network rather than straight to the public gateway.
	// [CP-M7] Default false (https or loopback required). When true, an http NON-loopback endpoint is
	// permitted ONLY IF (a) no credential is set — instance_id/token must be empty, so nothing rides the
	// cleartext link (the collector authenticates to Grafana Cloud, not aip-oi); and (b) the host is a
	// private target — an IP literal must be RFC-1918/loopback/link-local (a public IP is rejected), while
	// a DNS name (e.g. a Kubernetes Service) is permitted since it can't be resolved at config-load time.
	// https endpoints ignore this flag.
	AllowInsecure bool `yaml:"allow_insecure" helm:"default=false"`
}
type IdentityConfig struct {
	ServiceNamespace string `yaml:"service_namespace" helm:"default=aip-oi"`
	// Deployment environment label (e.g. dev, staging, prod).
	DeploymentEnvironment string `yaml:"deployment_environment" helm:"env=ENV"`
}

// ProductIdentity is the OTLP resource-attribute identity map aip-oi stamps on every emitted PRODUCT
// resource (cmd/aip-oi passes it to the emitter). All three keys are in the Grafana Cloud Loki
// `default_resource_attributes_as_index_labels` set, so each consumes one max_label_names_per_series
// stream-label slot. SINGLE SOURCE OF TRUTH: cmd/aip-oi builds the emitter Identity from this, and the
// composition root counts len(ProductIdentity) against governance.max_stream_label_keys — so the two
// can't drift.
func (ic IdentityConfig) ProductIdentity() map[string]string {
	return map[string]string{
		"service.name":                "aip-oi",
		"service.namespace":           ic.ServiceNamespace,
		"deployment.environment.name": ic.DeploymentEnvironment,
	}
}

type HAConfig struct {
	// lease: use a k8s Lease for leader election (production multi-replica). none: disable HA (single-replica dev/test).
	Coordinator string `yaml:"coordinator" helm:"default=lease"` // lease | none
	// configmap: durable watermark in a k8s ConfigMap (required with coordinator=lease). file: local file (dev only).
	Checkpoint string `yaml:"checkpoint" helm:"default=configmap"` // configmap | file
}
type QueueConfig struct {
	// Per-loop in-memory queue depth (batches). At ~1 batch/min this is ~4h of backlog before the
	// queue blocks the scheduler (the block-on-full backpressure path).
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
	if err := resolveSecretsNode(&root, placeholders); err != nil {
		return nil, err
	}
	resolved, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("config: re-marshal after secret resolution: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(resolved)))
	dec.KnownFields(true) // unknown YAML key ⇒ error
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
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
	if cfg.Selfobs.Profiling.Enabled {
		if cfg.Selfobs.Profiling.Mode == "" {
			cfg.Selfobs.Profiling.Mode = "pull" // default mode: zero new egress / dependency surface
		}
		if cfg.Selfobs.Profiling.Mode == "pull" && cfg.Selfobs.Profiling.Pull.Addr == "" {
			cfg.Selfobs.Profiling.Pull.Addr = ":6060" // dedicated pprof listener (NOT the public health port)
		}
	}
	return &cfg, nil
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
		tok := fmt.Sprintf("aipoiXsecretX%dXaipoi", i)
		i++
		ph[tok] = v
		return tok
	})
	if firstErr != nil {
		return "", nil, firstErr
	}
	return out, ph, nil
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
		}
		n.Value = val
	}
	return nil
}

// Validate cross-checks the loaded config. `known` is the set of registered source types.
func (c *Config) Validate(known map[string]struct{}) error {
	var errs []error
	if c.Emit.Telemetry.OTLP.Endpoint == "" {
		errs = append(errs, errors.New("emit.telemetry.otlp.endpoint required"))
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
	if c.HA.Coordinator != "lease" && c.HA.Coordinator != "none" {
		errs = append(errs, fmt.Errorf("ha.coordinator must be lease|none, got %q", c.HA.Coordinator))
	}
	if c.HA.Checkpoint != "configmap" && c.HA.Checkpoint != "file" {
		errs = append(errs, fmt.Errorf("ha.checkpoint must be configmap|file, got %q", c.HA.Checkpoint))
	}
	if c.HA.Coordinator == "lease" && c.HA.Checkpoint == "file" { // [CP-H7/H11]
		errs = append(errs, errors.New("ha.checkpoint=file is unsafe with ha.coordinator=lease (per-pod, not shared across replicas → restart loses the watermark; use configmap)"))
	}
	errs = append(errs, validateEmitEndpoint("emit.telemetry.otlp", c.Emit.Telemetry.OTLP)...) // [CP-M7]
	// [round3 MEDIUM-1] the optional self-telemetry endpoint carries the same instance_id:token Basic
	// auth, so the same cleartext-credential gate applies.
	if c.Emit.Self != nil {
		errs = append(errs, validateEmitEndpoint("emit.self.otlp", c.Emit.Self.OTLP)...)
		// 1DPM: the self-obs PeriodicReader must not push faster than the configured DPM floor
		// (60s / max_dpm). Unset interval ⇒ provider default (the floor). Guard maxDPM≥1 so a
		// directly-constructed Config (test path, no Load) can't divide by zero.
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
		for name, lp := range s.Loops {
			if !lp.Enabled {
				continue
			}
			if time.Duration(lp.Cadence) < minCadence {
				errs = append(errs, fmt.Errorf("%s/%s: cadence %s < floor %s", s.SourceInstance, name, time.Duration(lp.Cadence), minCadence))
			}
			// A loop with no window is an aggregate-now (snapshot) loop — it pulls a current total each
			// tick, not a time-bucketed window. The window/settle/backfill checks below encode time-bucket
			// semantics (graphs-style) and are N/A for it; only the cadence floor above applies. (Decoupled:
			// keyed on window==0, not on the source type.) A time-bucketed source MUST reject a
			// non-positive window in its OWN constructor (portkey.New does) — so an omitted window on a
			// bucketed source fails fast at build, not silently no-ops here (AR-HIGH).
			if time.Duration(lp.Window) == 0 {
				continue
			}
			if time.Duration(lp.Window) > maxWindow {
				errs = append(errs, fmt.Errorf("%s/%s: window %s > %s (would flip bucket granularity, H5)", s.SourceInstance, name, time.Duration(lp.Window), maxWindow))
			}
			// [AR-M-win] worst-case gap between two consecutive jittered ticks is cadence*(1+2·jitter)
			// (a +jitter tick following a −jitter tick), so the window must cover that gap + settle.
			lower := time.Duration(float64(time.Duration(lp.Cadence))*(1+2*jitterFrac)) + time.Duration(lp.BucketSettle)
			if time.Duration(lp.Window) < lower {
				errs = append(errs, fmt.Errorf("%s/%s: window %s < cadence*(1+2·jitter)+settle %s (uncovered time after settle, M3)", s.SourceInstance, name, time.Duration(lp.Window), lower))
			}
			if time.Duration(lp.MaxBackfill) > 0 && time.Duration(lp.BootstrapLookback) > time.Duration(lp.MaxBackfill) { // [CP-H7]
				errs = append(errs, fmt.Errorf("%s/%s: bootstrap_lookback %s > max_backfill %s (first-run would skip storable data)", s.SourceInstance, name, time.Duration(lp.BootstrapLookback), time.Duration(lp.MaxBackfill)))
			}
		}
	}
	return errors.Join(errs...)
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
