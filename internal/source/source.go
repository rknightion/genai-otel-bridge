// SPDX-License-Identifier: AGPL-3.0-only

// Package source defines the Source/Loop seam (ARCHITECTURE.md §5), the type registry, and
// the governance guard. Vendor packages (portkey, …) register themselves here.
package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/httpx"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// Sentinel Collect outcomes a source can signal without importing schedule (the scheduler maps
// them to self-metrics). ErrQuotaExceeded ⇒ discard batch, no advance, back off (F3/F34);
// ErrGranularityUnexpected ⇒ the server returned a wrong bucket step, alert + no advance (F27).
var (
	ErrQuotaExceeded         = errors.New("source: quota exceeded")
	ErrGranularityUnexpected = errors.New("source: unexpected bucket granularity")
)

// Source is one vendor integration. FROZEN.
type Source interface {
	ID() string
	Loops() []Loop
}

// Loop is one independent pull→derive cycle. FROZEN — do not add methods.
type Loop interface {
	Key() model.CheckpointKey
	Cadence() time.Duration
	Collect(ctx context.Context, since model.Watermark) (model.Batch, error)
}

// SeriesDeclarer is an OPTIONAL capability (not part of the frozen Loop): a loop that can
// declare the series names it will emit, enabling startup ownership validation (F22/F42).
type SeriesDeclarer interface {
	SeriesNames() []string
}

// IndexedKeyDeclarer is an OPTIONAL capability (not part of the frozen Loop): a LOGS loop that promotes
// record fields to LogRecord.IndexedAttributes (OTLP resource attrs → Loki stream labels via GS1)
// declares the FULL set it may emit (its base content-free allow-list ∪ settings.extra_indexed_fields).
// The composition root sums these with decant's product identity resource attrs and rejects a config
// that would exceed the Loki max_label_names_per_series budget (governance.max_stream_label_keys) — a
// stream over that limit is REJECTED (silently dropped) by Loki. Mirrors SeriesDeclarer.
type IndexedKeyDeclarer interface {
	IndexedKeys() []string
}

// Deps carries composition-root dependencies that are NOT config data — cross-cutting hooks a source
// needs but can't get from YAML. The zero value is a no-op (nil hooks), so tests can pass Deps{}.
type Deps struct {
	// UpstreamObserver, if set, is wired into the source's httpx client to record the self-obs
	// upstream-request histogram. nil ⇒ no instrumentation.
	UpstreamObserver httpx.Observer
	// OnBucketRevised, if set, is called by a source when it observes an ALREADY-EMITTED (settled)
	// bucket change value on a later poll — i.e. a late arrival landed after bucket_settle. `age` is how
	// late the revision is (now − bucketEnd; always ≥ settle). The source does NOT re-emit (that would
	// break gap-free / byte-identical emit); it only signals. The composition root wires this to
	// metrics.BucketRevisedAfterSettle{loop,age} (count + age histogram). nil ⇒ no detection.
	// Mirrors the GuardConfig.OnNewLabelValue early-warning hook pattern.
	OnBucketRevised func(loop string, age time.Duration)
	// OnGraphSkipped, if set, is called when a source skips a configured sub-stream on a poll because it
	// 404'd (capability detection / permission / absence) — by-design, the loop derives from the rest and
	// advances, but the skip was previously SILENT (only a log). The composition root wires this to
	// metrics.SourceGraphUnavailable{loop,graph} so a flapping-404 graph is observable and distinguishable
	// from a permanently-absent one (round3-#4). nil ⇒ no signal. Same injected-hook pattern as above.
	OnGraphSkipped func(loop, graph string)
	// OnAuthError, if set, is called when a loop's upstream API responds 401/403 — a credential
	// problem (wrong/expired key, missing scope/permission), distinct from "the endpoint is slow". A
	// 401/403 already surfaces as a retryable Collect error (window_lag rises — loud, never silent), but
	// that is indistinguishable in metrics from a generic 4xx/timeout. The composition root wires this to
	// metrics.AuthError{loop,source} so a credential failure is its OWN alertable signal (followup §9).
	// `source` is the source instance id (credentials are per-source). nil ⇒ no signal. Same hook pattern.
	OnAuthError func(loop, source string)
}

// IsAuthStatus reports whether an HTTP status code is a credential/authorization failure (401/403) —
// the codes a source maps to Deps.OnAuthError. Shared so vendor packages don't re-spell the magic codes.
func IsAuthStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

// Constructor builds a Source from config plus composition-root deps.
type Constructor func(config.SourceConfig, Deps) (Source, error)

// Registry maps source type strings to their constructors.
type Registry struct{ m map[string]Constructor }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Constructor{}} }

// Register associates typ with its Constructor.
func (r *Registry) Register(typ string, c Constructor) { r.m[typ] = c }

// Known returns a snapshot of registered types — passed to config.Validate to catch unknown types.
func (r *Registry) Known() map[string]struct{} {
	out := map[string]struct{}{}
	for k := range r.m {
		out[k] = struct{}{}
	}
	return out
}

// Build constructs a Source for the given config + deps, returning an error for unknown types (F22).
func (r *Registry) Build(cfg config.SourceConfig, deps Deps) (Source, error) {
	c, ok := r.m[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("source: unknown type %q", cfg.Type)
	}
	return c(cfg, deps)
}

// ValidateOwnership fails if two loops declare the same normalized (post-gateway) series name
// (F42) — config validation alone can't catch a data-dependent duplicate-timestamp clash (M7).
func ValidateOwnership(sources []Source) error {
	owner := map[string]string{}
	for _, s := range sources {
		for _, lp := range s.Loops() {
			sd, ok := lp.(SeriesDeclarer)
			if !ok {
				continue
			}
			for _, n := range sd.SeriesNames() {
				norm := NormalizeSeriesName(n)
				key := lp.Key().String()
				if prev, dup := owner[norm]; dup && prev != key {
					return fmt.Errorf("series %q owned by both %q and %q", norm, prev, key)
				}
				owner[norm] = key
			}
		}
	}
	return nil
}

// NormalizeSeriesName mirrors the gateway's metric-name transform (dots→_, lowercase) so
// ownership is checked on the FINAL series identity. [AR-H-F] NOTE: this does NOT yet model the
// gateway's unit-suffixing / `_total` appending (F42) — two names differing only by a suffix the
// gateway would add could still collide post-gateway. v1 names are pre-suffixed and distinct, so
// this is sufficient now; extend when unit-suffixed or counter names are added.
func NormalizeSeriesName(n string) string {
	return strings.ToLower(strings.ReplaceAll(n, ".", "_"))
}
