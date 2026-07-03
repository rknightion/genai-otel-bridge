// SPDX-License-Identifier: AGPL-3.0-only

// Package app is the composition root: wiring only, no logic.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/langsmith"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

type App struct {
	sched *schedule.Scheduler
	coord coordinate.Coordinator
	specs []schedule.LoopSpec
}

func (a *App) Specs() []schedule.LoopSpec { return a.specs }

// authErrorHook is the default Deps.OnAuthError: count the AuthError metric (the alertable rate-of-truth)
// AND emit a rate-limited WARN so a credential failure (401/403) is visible on `kubectl logs`, not only
// in the self-o11y metrics stack. Rate-limited because a persistent bad/expired key fires every tick.
func authErrorHook(m schedule.Metrics, lim *logging.Limiter) func(loop, src string) {
	return func(loop, src string) {
		m.AuthError(loop, src)
		if lim.Allow("auth:" + loop + ":" + src) {
			slog.Warn("upstream auth failure (401/403); check source credentials", "loop", loop, "source", src)
		}
	}
}

// Build assembles the runtime. Deps (cp/coord/emitter/metrics) are injected so cmd supplies the
// real OTLP/k8s/selfobs implementations and tests supply fakes.
func Build(_ context.Context, cfg *config.Config, cp checkpoint.Checkpointer, coord coordinate.Coordinator, em emit.Emitter, m schedule.Metrics, deps source.Deps) (*App, error) {
	reg := source.NewRegistry()
	portkey.Register(reg)
	langsmith.Register(reg)
	if err := cfg.Validate(reg.Known()); err != nil {
		return nil, fmt.Errorf("config invalid: %w", err)
	}
	// Indexed/label allow-list (guard is default-deny, CP-C6): the UNCONDITIONAL union of EVERY registered
	// vendor package's declared content-free keys — quantile (latency/percentile gauges), ai_model/
	// metadata_key/metadata_value (groups), ai_org/response_status_code (portkey logs), session/feedback_key
	// (langsmith sessions), run_type/status (langsmith runs) — PLUS the operator's additive
	// governance.allow_label_keys opt-in. The union is NOT gated on which sources are enabled (#75): every
	// key here is content-free by declaration and each label key is chosen by SOURCE CODE, never derived
	// from upstream data, so a disabled vendor's keys widen the default-deny surface with no live leak path
	// (a Portkey-only deployment allow-lists langsmith's run_type/status etc., harmlessly). Keying it on
	// enabled sources would tighten the surface but needs a per-type registry hook; the union is accepted
	// as-is. The keys live in the vendor packages (portkey/langsmith AllowedLabelKeys), not hardcoded here,
	// per the decoupling hard rule. An un-listed indexed attr is dropped (a Sample) or drops the whole
	// LogRecord (okLog).
	// Operator promotions to the indexed/label tier: top-level governance.allow_label_keys PLUS each
	// loop's settings.extra_indexed_fields (per-loop). The latter is AUTO-allow-listed here so a strip
	// that promotes a field to IndexedAttributes can't then be silently dropped by the default-deny guard
	// (the footgun a separate two-knob design would create).
	operatorPromoted := append(append([]string{}, cfg.Governance.AllowLabelKeys...), optedInIndexedFields(cfg)...)
	allowKeys := dedupe(append(append(append([]string{},
		portkey.AllowedLabelKeys()...), langsmith.AllowedLabelKeys()...), operatorPromoted...))
	// Reject a content-floor key (message body / injected PII) named in any operator promotion: the guard's
	// deny floor would otherwise silently neutralise it (deny beats allow), and a silent no-op of operator
	// intent violates the operationally-honest rule. (The owning source ALSO rejects it at construction;
	// this composition-root check is the backstop that can't drift.) source.IsContentFloorKey matches by
	// EXACT key AND by gen_ai content-namespace PREFIX, so a flattened content attr such as
	// gen_ai.prompt.0.content is rejected here too, not only the bare gen_ai.prompt (#97). Fail fast at startup.
	for _, k := range operatorPromoted {
		if source.IsContentFloorKey(k) {
			return nil, fmt.Errorf("governance.allow_label_keys / settings.extra_indexed_fields: %q is a content-floor key (message body / injected PII) and cannot be promoted to a label", k)
		}
	}
	// [settle-exceedance] Wire the settle-exceedance signal: a source that re-observes an
	// already-emitted (settled) bucket change value calls this hook; it does NOT re-emit (DESIGN
	// §3.3/F6). Mirrors the guard's OnNewLabelValue wiring above. Only set if a source didn't already
	// supply one (it doesn't today), so the composition root owns the metrics binding.
	if deps.OnBucketRevised == nil {
		deps.OnBucketRevised = m.BucketRevisedAfterSettle
	}
	// [round3-#4] Make a source's (otherwise silent) capability-skip observable: a configured graph that
	// 404s is logged + skipped (derive from the rest), but now also counted via SourceGraphUnavailable so
	// a flapping-404 graph is distinguishable from a permanently-absent one. Same pattern as above.
	if deps.OnGraphSkipped == nil {
		deps.OnGraphSkipped = m.SourceGraphUnavailable
	}
	// [followup §9] An upstream 401/403 already surfaces as a retryable Collect error (window_lag rises),
	// but that is indistinguishable in metrics from a generic slow/erroring endpoint. Wire OnAuthError to
	// AuthError{loop,source} so a credential failure (wrong/expired key, missing scope) is its OWN
	// alertable signal — AND a rate-limited WARN so it's visible on `kubectl logs`, not only in self-metrics.
	if deps.OnAuthError == nil {
		deps.OnAuthError = authErrorHook(m, logging.NewLimiter(time.Minute))
	}
	// [Loki stream-label budget] A logs loop's IndexedAttributes become OTLP resource attrs → Loki stream
	// labels once GS1 promotes them; together with genai-otel-bridge's product identity resource attrs (all in the GC
	// Loki default-promoted set) they consume the tenant's max_label_names_per_series budget. Loki REJECTS
	// (silently drops) a stream over that limit — which the operationally-honest rule forbids — so fail
	// fast here, not at emit time. Ceiling = governance.max_stream_label_keys (default = the GC Loki
	// default 15, tenant-overridable by Grafana staff). NOTE: in the in-cluster-Alloy topology, Alloy's
	// k8s.*/cloud.* enrichment attrs (also default-promoted) share this budget — operators size with
	// headroom. (Default applied here too: a struct-built config that bypassed config.Load has 0.)
	streamLabelCeiling := cfg.Governance.MaxStreamLabelKeys
	if streamLabelCeiling == 0 {
		streamLabelCeiling = config.DefaultMaxStreamLabelKeys
	}
	identityReserve := len(cfg.Identity.ProductIdentity())
	// FIRST PASS: build every enabled source and its loops. Loops are built BEFORE the guard so the
	// per-loop content denylist (#130) can be keyed by each loop's real identity (CheckpointKey.String()),
	// which SanitizeLogs receives — and so we honour a content opt-in ONLY on loops that actually consume
	// it (the logs loops, identified by the source.IndexedKeyDeclarer capability).
	var srcs []source.Source
	var loops []loopConfigured
	for _, sc := range cfg.Sources {
		if !sc.Enabled {
			continue
		}
		src, err := reg.Build(sc, deps)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, src)
		// #40: loop names are free-form map keys; each source constructs only the fixed names it knows and
		// silently ignores the rest. A typo'd enabled loop (e.g. `log_export` for `logs_export`) would
		// otherwise pass Validate (its cadence/window are even checked, feigning validation) and then never
		// collect — no error, no warning. Reconcile every enabled configured loop name against what the
		// source actually built and fail fast on an unrecognised one (matching CP-H4's unknown-graph
		// fail-fast). The known set comes from src.Loops() so no vendor loop-name knowledge leaks here.
		built := map[string]bool{}
		var builtNames []string
		for _, lp := range src.Loops() {
			if n := lp.Key().Loop; !built[n] {
				built[n] = true
				builtNames = append(builtNames, n)
			}
		}
		for lcName, lc := range sc.Loops {
			if lc.Enabled && !built[lcName] {
				return nil, fmt.Errorf("source %q (type %q): configured loop %q is enabled but not recognised by the source (built loops: %s) — check for a typo", sc.SourceInstance, sc.Type, lcName, strings.Join(builtNames, ", "))
			}
		}
		for _, lp := range src.Loops() {
			lcName := lp.Key().Loop
			lc := sc.Loops[lcName]
			if d, ok := lp.(source.IndexedKeyDeclarer); ok {
				if n := identityReserve + len(d.IndexedKeys()); n > streamLabelCeiling {
					return nil, fmt.Errorf("loop %q would emit %d Loki stream labels (identity %d + indexed %d) > governance.max_stream_label_keys %d (Loki max_label_names_per_series); reduce settings.extra_indexed_fields / governance.allow_label_keys, or raise the limit only if your tenant's max_label_names_per_series was increased by Grafana staff", lcName, n, identityReserve, len(d.IndexedKeys()), streamLabelCeiling)
				}
			}
			loops = append(loops, loopConfigured{loop: lp, cfg: lc})
		}
	}
	if err := source.ValidateOwnership(srcs); err != nil {
		return nil, err
	}
	if len(loops) == 0 {
		return nil, fmt.Errorf("no enabled loops")
	}
	// Build the guard now the per-loop denylists (keyed by loop identity) are known.
	guard := source.NewGuard(source.GuardConfig{
		AllowLabelKeys: allowKeys,
		// Global content denylist = the never-subtractable floor + the ENTIRE gray backstop, NOTHING
		// subtracted. It polices the metrics path (Sample labels, additionally allow-list default-denied) and
		// is the fallback for any logs loop with no per-loop entry. Per-loop gray-field releases live in
		// DenyFieldKeysByLoop (#130) so one loop's opt-in can never weaken another loop's defence-in-depth
		// backstop, and a content opt-in on a metrics loop (which merely warns it away as an unknown setting)
		// has no effect on the guard at all.
		DenyFieldKeys:       contentDenylist(nil),
		DenyFieldKeysByLoop: contentDenyByLoop(loops),
		PerSeriesBudget:     cfg.Governance.PerMetricCardinalityBudget, // per-metric cap; config-keyed, default 10k
		OnNewLabelValue:     m.NewLabelValue,
	})
	// SECOND PASS: build the runners + specs now the shared guard exists.
	var specs []schedule.LoopSpec
	for _, l := range loops {
		runner := schedule.NewLoopRunner(l.loop, em, cp, guard, cfg.Queue.MaxBatches, cfg.Governance.MaxDPM, m)
		specs = append(specs, schedule.LoopSpec{
			Runner: runner, Loop: l.loop, Cadence: time.Duration(l.cfg.Cadence), MaxBackfill: time.Duration(l.cfg.MaxBackfill),
			Window: time.Duration(l.cfg.Window), MaxCatchupPerTick: cfg.Governance.MaxCatchupPerTick,
		})
	}
	return &App{sched: schedule.NewScheduler(specs, m), coord: coord, specs: specs}, nil
}

// Run serves health and runs the scheduler under the coordinator until ctx is cancelled.
// [AR-H-beat] `beat` is wired into the scheduler so /healthz reflects loop progress.
// [CP-C5] `setLeader` toggles leadership so a standby (not running the scheduler) stays healthy.
func (a *App) Run(ctx context.Context, health http.Handler, healthAddr string, markReady, beat func(), setLeader func(bool)) error {
	a.sched.SetBeat(beat)
	// [CP-R3/M8] A health-server bind failure (e.g. port in use) must ABORT the app synchronously —
	// not run probe-less and blind. Cancel the run context on failure and surface the error.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// [#72] ReadHeaderTimeout bounds slow-header (Slowloris) clients dribbling request headers to hold
	// health-server goroutines/FDs open — which would starve liveness/readiness probes and get an
	// otherwise-healthy leader killed (also satisfies gosec G114). Health responses are tiny and fast,
	// so a whole-request ReadTimeout is safe too.
	srv := &http.Server{Addr: healthAddr, Handler: health, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health server failed", "err", err)
			srvErr <- err
			cancel()
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()
	markReady()
	err := a.coord.Run(runCtx, func(leaderCtx context.Context) {
		setLeader(true)        // [CP-C5] now leader → /healthz judges this replica's heartbeat
		defer setLeader(false) // demotion → back to standby-healthy
		a.sched.Run(leaderCtx) // blocks until leadership/ctx ends
	})
	select {
	case se := <-srvErr:
		return fmt.Errorf("health server bind failed: %w", se) // → main fatal → pod restart
	default:
		return err
	}
}

// grayBackstopDenyKeys are content/free-text fields that are content-free-BY-DEFAULT (the per-loop strip
// drops them via default-deny) but are kept on the guard denylist as DEFENCE-IN-DEPTH — the second
// independent layer that catches a hypothetical strip-allow-list regression. They are the "gray" tier:
// SUBTRACTABLE. When an operator explicitly opts one into a loop's record allow-list
// (settings.extra_record_fields), it is removed from the EFFECTIVE denylist for that deployment so the
// guard doesn't drop the whole record (okLog) — content-governance is the customer's policy call (see
// the content-governance-configurable rationale). A deployment that opts in NOTHING keeps the full
// backstop; only the specific opted-in keys are released. Contrast source.AbsoluteNeverDenyKeys (the
// never-subtractable, never-opt-in-able floor of message bodies + injected PII).
func grayBackstopDenyKeys() []string {
	// NOTE: inputs_preview/outputs_preview are NOT here — they are truncated renderings of the actual
	// prompt/response (LLM content), so they were promoted to the never-subtractable content FLOOR
	// (source.AbsoluteNeverDenyKeys) and can no longer be opted in via any knob (#95). "no LLM content to
	// Grafana Cloud — ever" (followup.md). The keys below are content-free-BY-DEFAULT free-text/operational
	// fields: subtractable defence-in-depth only.
	return []string{
		"events", "extra", "serialized",
		"manifest", "s3_urls", "error", "name",
	}
}

// contentDenylist is the outbound field denylist (defence beyond data-minimisation, Cdx-H7): the
// DEFENCE-IN-DEPTH backstop to a source's own content strip — if a strip allow-list ever regresses, the
// guard still blocks any emitted field whose key is named here (a denied key in either attr map drops
// the WHOLE record — okLog). It is the FLOOR (source.AbsoluteNeverDenyKeys — message bodies + the two
// Portkey-injected PII/config fields, never subtractable) PLUS the gray backstop tier MINUS the fields
// any enabled loop explicitly opted into its record allow-list (so the guard does not silently eat an
// opted-in record). The floor is added unconditionally, so opting in a body never weakens it.
//
// [logs_export, lane-F review HIGH] `metadata` + `portkeyHeaders` (in the floor) are the two fields the
// Portkey PoC proved are injected into the export payload REGARDLESS of requested_data (PoC §3 — customer
// PII: owner names, RITM ticket ids, data_classification; and gateway config). The strips drop them, but
// they MUST also be on this list so the guard is a true independent second layer for the live leak.
// dedupe returns xs with duplicate entries removed, preserving first-seen order. Used to union the
// per-vendor label-key lists (`quantile` is emitted by both vendors) plus the operator opt-in.
func dedupe(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func contentDenylist(optedIn map[string]bool) []string {
	out := source.AbsoluteNeverDenyKeys() // floor — always denied, never subtracted
	for _, g := range grayBackstopDenyKeys() {
		if !optedIn[g] { // released only when explicitly opted in by a loop
			out = append(out, g)
		}
	}
	return out
}

// loopConfigured pairs a built loop with its per-loop config, so the composition root can build the
// runners AND the per-loop content denylist (#130) from one collected slice.
type loopConfigured struct {
	loop source.Loop
	cfg  config.LoopConfig
}

// optedInContentFieldsForLoop is the union of a SINGLE loop's THREE content-governance opt-in knobs —
// settings.extra_record_fields ∪ extra_indexed_fields ∪ metadata_record_fields (cross-source conventions
// — the per-loop strips read these same keys). It is governance wiring, not vendor/domain knowledge (the
// key names are generic and the values are operator policy, not defaults). A gray backstop field named in
// ANY knob is an explicit opt-in and is subtracted from THIS loop's effective denylist, so the guard
// cannot silently deny-drop a field the operator promoted+allow-listed for the loop (#51). A floor field
// present here is still kept denied (contentDenylist never subtracts the floor) and is rejected fail-fast
// by the owning source / the composition-root floor check.
func optedInContentFieldsForLoop(lc config.LoopConfig) map[string]bool {
	out := map[string]bool{}
	for _, knob := range []string{"extra_record_fields", "extra_indexed_fields", "metadata_record_fields"} {
		for f := range strings.SplitSeq(lc.Settings[knob], ",") {
			if f = strings.TrimSpace(f); f != "" {
				out[f] = true
			}
		}
	}
	return out
}

// optedInContentFields is the union of optedInContentFieldsForLoop across every ENABLED loop. It is no
// longer used to build the guard (that is now strictly per-loop — see contentDenyByLoop, #130), but is
// retained as the union primitive exercised by the focused #51 denylist-composition tests.
func optedInContentFields(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, sc := range cfg.Sources {
		if !sc.Enabled {
			continue
		}
		for _, lp := range sc.Loops {
			if !lp.Enabled {
				continue
			}
			for f := range optedInContentFieldsForLoop(lp) {
				out[f] = true
			}
		}
	}
	return out
}

// contentDenyByLoop builds the PER-LOOP effective content denylist for the logs path (#130), keyed by each
// loop's identity (CheckpointKey.String() — the same string the runner passes to Guard.SanitizeLogs). It
// scopes the gray-backstop subtraction so an opt-in on one loop can NEVER widen another loop's allowed
// set (fixing the cross-loop bleed), and it honours the opt-in ONLY on loops that actually consume it —
// the LOGS loops, identified by the source.IndexedKeyDeclarer capability (implemented solely by the two
// logs loops). A metrics loop that merely warns extra_record_fields away as an unknown setting gets NO
// entry, so its stray opt-in has no effect on the guard; such a loop falls back to the full global
// backstop (GuardConfig.DenyFieldKeys). Vendor-neutral: it uses a generic capability, no loop-name knowledge.
func contentDenyByLoop(loops []loopConfigured) map[string][]string {
	out := map[string][]string{}
	for _, l := range loops {
		if _, isLogs := l.loop.(source.IndexedKeyDeclarer); !isLogs {
			continue // a metrics loop does not consume the content-governance knobs — do not release anything
		}
		out[l.loop.Key().String()] = contentDenylist(optedInContentFieldsForLoop(l.cfg))
	}
	return out
}

// optedInIndexedFields is the ordered, de-duplicated union of every enabled loop's
// settings.extra_indexed_fields — the per-loop INDEXED-tier (Loki stream-label) promotion. The
// composition root auto-allow-lists these in the guard (so a promoted indexed attr is never silently
// dropped) and floor-rejects any content key. Same generic-wiring rationale as optedInContentFields (no
// vendor/domain knowledge — a generic key name + operator-policy values). NOTE: only the LOGS loops
// (portkey logs_export, langsmith runs) actually consume the key in their strip; on a metrics loop the
// key is a no-op (allow-listing it here is harmless, and a floor key is still rejected) — matching the
// pre-existing extra_record_fields behaviour.
func optedInIndexedFields(cfg *config.Config) []string {
	var out []string
	seen := map[string]bool{}
	for _, sc := range cfg.Sources {
		if !sc.Enabled {
			continue
		}
		for _, lp := range sc.Loops {
			if !lp.Enabled {
				continue
			}
			for f := range strings.SplitSeq(lp.Settings["extra_indexed_fields"], ",") {
				if f = strings.TrimSpace(f); f != "" && !seen[f] {
					seen[f] = true
					out = append(out, f)
				}
			}
		}
	}
	return out
}
