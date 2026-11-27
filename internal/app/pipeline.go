// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// pipeline is one plan row while BuildFromPlan assembles it: the row with its secrets read (pass 1),
// the row's telemetry, its source and loops (pass 1), and its group's guard (pass 2).
type pipeline struct {
	row   tenancy.Row
	t     PipelineTelemetry
	src   source.Source
	loops []loopConfigured
	guard *source.Guard
	// gate is the row's scope gate (pass 1, rowScopeGate): nil for a row with no scope.
	gate *source.ScopeGate
}

// specs is assembly pass 3 (design §4, §5.2): the only production code that puts a loop and an emitter
// together, and the only production caller of schedule.NewLoopRunner. Both come from this one row: the
// emitter factory is called once, with the row's destination, identity and telemetry, and every one of
// the row's loops gets a runner on that emitter. Nothing at run time chooses a destination.
func (p *pipeline) specs(cfg *config.Config, cp checkpoint.Checkpointer, ef EmitterFactory) []schedule.LoopSpec {
	em := ef(EmitterSpec{
		Endpoint:   p.row.Destination.Endpoint,
		InstanceID: p.row.Destination.InstanceID,
		Token:      p.row.Destination.Token,
		Identity:   pipelineIdentity(p.row),
		Tenant:     p.row.Tenant,
		Pipeline:   p.row.Pipeline,
	}, p.t)
	pl := schedule.Pipeline{Tenant: p.row.Tenant, ID: p.row.Pipeline}
	specs := make([]schedule.LoopSpec, 0, len(p.loops))
	for _, l := range p.loops {
		runner := schedule.NewLoopRunner(l.loop, em, cp, p.guard, cfg.Queue.MaxBatches, cfg.Governance.MaxDPM, p.t, schedule.WithPipeline(pl))
		specs = append(specs, schedule.LoopSpec{
			Runner: runner, Loop: l.loop, Cadence: time.Duration(l.cfg.Cadence), MaxBackfill: time.Duration(l.cfg.MaxBackfill),
			Window: time.Duration(l.cfg.Window), MaxCatchupPerTick: cfg.Governance.MaxCatchupPerTick,
			Metrics: p.t, Pipeline: pl, Gate: p.gate,
		})
	}
	return specs
}

// rowScopeGate is assembly pass 1's scope step for one row (design §5.1, §7), after the row's source is
// built. It refuses, in every row, a loop named source.ScopeMatchLoop: that name keys the pipeline's
// remembered match, so a vendor loop under it could overwrite the match or be read as one. A row with no
// scope (every legacy row) gets no gate. A row with a scope, meaning a name or an id, fails unless its
// source implements source.ScopeAsserter; it gets one gate, which pass 3 sets on every one of its specs.
// The gate remembers its match in cp under the row's source instance, with the digest of the fingerprint
// of the API key the source sends (the row's, after its secret files were read), the scope name and the
// scope id; it reads the clock now (nil: time.Now) and reports through the row's telemetry.
func rowScopeGate(row tenancy.Row, src source.Source, t PipelineTelemetry, cp checkpoint.Checkpointer, now func() time.Time) (*source.ScopeGate, error) {
	for _, lp := range src.Loops() {
		if lp.Key().Loop == source.ScopeMatchLoop {
			return nil, fmt.Errorf("source %q (type %q): %s is a reserved loop name (the pipeline's remembered scope match); no source may build a loop under it", row.Source.SourceInstance, row.Source.Type, source.ScopeMatchLoop)
		}
	}
	scope := row.Source.Scope
	if scope == (config.ScopeConfig{}) {
		return nil, nil
	}
	asserter, ok := src.(source.ScopeAsserter)
	if !ok {
		return nil, fmt.Errorf("source %q (type %q) has a scope, but the source type cannot assert one (it does not implement source.ScopeAsserter); a scope is never collected unchecked", row.Source.SourceInstance, row.Source.Type)
	}
	return source.NewScopeGate(source.ScopeGateConfig{
		Asserter:            asserter,
		Store:               cp,
		SourceInstance:      row.Source.SourceInstance,
		Digest:              source.ScopeMatchDigest(source.KeyFingerprint(row.Source.Auth.Value), scope.Name, scope.ID),
		Now:                 now,
		OnResult:            t.ScopeAssertion,
		OnState:             t.ScopeState,
		OnMatchWriteFailure: t.ScopeMatchWriteFailed,
	}), nil
}

// pipelineIdentity is the resource identity a row's emitter stamps: the row's own identity map, plus
// tenancy.PipelineAttribute set to the pipeline id when the row has one (design §6). A legacy row has no
// pipeline id, so it stamps exactly its identity, as before. The row's map is copied, never changed.
func pipelineIdentity(row tenancy.Row) map[string]string {
	id := maps.Clone(row.Identity)
	if id == nil {
		id = map[string]string{}
	}
	if row.Pipeline != "" {
		id[tenancy.PipelineAttribute] = row.Pipeline
	}
	return id
}

// Reasons a tenant is blocked at assembly (design §5.5, §9): the reason label of tenant_blocked and the
// reason Options.OnTenantBlocked is called with.
const (
	blockedSecretUnreadable = "secret_unreadable"
	blockedDuplicateKey     = "duplicate_key"
)

// blockedReasons is every reason, in the order OnTenantBlocked is called with them.
var blockedReasons = []string{blockedSecretUnreadable, blockedDuplicateKey}

// readRowSecrets returns a copy of the row with its secret files read through r: AuthValueFile into
// Source.Auth.Value and TokenFile into Destination.Token. A row that names no file is returned as it is
// and r is not consulted, so a legacy row (whose secrets are already in its config) needs no reader. Both
// files are attempted, and keyRead reports that AuthValueFile was named and read, so the duplicate-key
// check sees a key even when the same row's token failed. A read error, a value that is empty, or a nil
// reader for a row that names a file is an error naming the tenant and the file; the message never
// carries a value (the reader's own error names tenant and file only, by its contract).
func readRowSecrets(row tenancy.Row, r tenancy.SecretReader) (out tenancy.Row, keyRead bool, err error) {
	read := func(file string) (string, error) {
		if r == nil {
			return "", fmt.Errorf("tenant %q: secret file %q: no secret reader is configured", row.Tenant, file)
		}
		v, err := r.Read(row.Tenant, file)
		if err != nil {
			return "", fmt.Errorf("tenant %q: secret file %q could not be read: %w", row.Tenant, file, err)
		}
		if v == "" {
			return "", fmt.Errorf("tenant %q: secret file %q is empty", row.Tenant, file)
		}
		return v, nil
	}
	var errs []error
	if row.AuthValueFile != "" {
		if v, err := read(row.AuthValueFile); err != nil {
			errs = append(errs, err)
		} else {
			row.Source.Auth.Value, keyRead = v, true
		}
	}
	if row.TokenFile != "" {
		if v, err := read(row.TokenFile); err != nil {
			errs = append(errs, err)
		} else {
			row.Destination.Token = v
		}
	}
	return row, keyRead, errors.Join(errs...)
}

// readPlanSecrets is assembly pass 1's secret step over the whole plan (design §4, §5.5, §9). It returns
// the rows to build, in plan order, each a copy with its secret files read, leaving out every row of a
// blocked tenant. A tenant row is one with a tenant id; a legacy row names no secret file, and a legacy
// row that did and could not read it still fails the build, as before.
//
//   - A tenant any of whose secret files cannot be read (readRowSecrets) is blocked: secret_unreadable.
//   - The fingerprint (source.KeyFingerprint) of every API key read from a secret file is compared across
//     every target of every tenant, a blocked tenant's included. Every tenant with a target whose
//     fingerprint another target also has is blocked, all its targets with it: duplicate_key. Only
//     fingerprints are compared, and nothing logs or returns one.
//   - One error line is logged per blocked-by-unreadable tenant (the tenant and the reader's error, which
//     names tenant and file only) and one per duplicate set, naming the set's pipeline ids only.
//   - onBlocked (nil is fine) is called for every tenant, in tenant order, and every reason, with blocked
//     true for each reason that blocks that tenant and false otherwise.
//   - When every tenant is blocked, it fails with every tenant's reasons (tenant, file and pipeline ids,
//     never a value or a fingerprint): the process does not start.
func readPlanSecrets(rows []tenancy.Row, r tenancy.SecretReader, onBlocked func(tenant, reason string, blocked bool)) ([]tenancy.Row, error) {
	read := make([]tenancy.Row, len(rows))
	keyRead := make([]bool, len(rows))
	unreadable := map[string][]error{}
	var tenants []string
	for i, row := range rows {
		rr, ok, err := readRowSecrets(row, r)
		if row.Tenant == "" {
			if err != nil {
				return nil, err
			}
			read[i] = rr
			continue
		}
		if !slices.Contains(tenants, row.Tenant) {
			tenants = append(tenants, row.Tenant)
		}
		read[i], keyRead[i] = rr, ok
		if err != nil {
			unreadable[row.Tenant] = append(unreadable[row.Tenant], err)
		}
	}
	sort.Strings(tenants)

	// Rows by key fingerprint. The map is local and its keys never leave this function.
	byKey := map[string][]int{}
	for i, row := range read {
		if row.Tenant != "" && keyRead[i] {
			fp := source.KeyFingerprint(row.Source.Auth.Value)
			byKey[fp] = append(byKey[fp], i)
		}
	}
	var dupSets [][]string
	for _, idx := range byKey {
		if len(idx) < 2 {
			continue
		}
		ps := make([]string, 0, len(idx))
		for _, i := range idx {
			ps = append(ps, read[i].Pipeline)
		}
		sort.Strings(ps)
		dupSets = append(dupSets, ps)
	}
	sort.Slice(dupSets, func(a, b int) bool { return slices.Compare(dupSets[a], dupSets[b]) < 0 })
	// duplicateIn is, per tenant, the duplicate sets one of its targets is in.
	duplicateIn := map[string][][]string{}
	for _, ps := range dupSets {
		for _, i := range rowsOfPipelines(read, ps) {
			t := read[i].Tenant
			if n := len(duplicateIn[t]); n == 0 || !slices.Equal(duplicateIn[t][n-1], ps) {
				duplicateIn[t] = append(duplicateIn[t], ps)
			}
		}
	}

	for _, t := range tenants {
		if errs := unreadable[t]; len(errs) > 0 {
			slog.Error("tenant blocked: a secret file could not be read; none of the tenant's pipelines is built",
				"tenant", t, "reason", blockedSecretUnreadable, "err", errors.Join(errs...))
		}
	}
	for _, ps := range dupSets {
		slog.Error("tenants blocked: one source API key is configured for more than one target; the tenant of every pipeline listed is not built",
			"reason", blockedDuplicateKey, "pipelines", strings.Join(ps, ","))
	}

	blocked := map[string]bool{}
	for _, t := range tenants {
		why := map[string]bool{
			blockedSecretUnreadable: len(unreadable[t]) > 0,
			blockedDuplicateKey:     len(duplicateIn[t]) > 0,
		}
		for _, reason := range blockedReasons {
			blocked[t] = blocked[t] || why[reason]
			if onBlocked != nil {
				onBlocked(t, reason, why[reason])
			}
		}
	}
	if len(tenants) > 0 && !slices.ContainsFunc(tenants, func(t string) bool { return !blocked[t] }) {
		var errs []error
		for _, t := range tenants {
			errs = append(errs, unreadable[t]...)
			for _, ps := range duplicateIn[t] {
				errs = append(errs, fmt.Errorf("tenant %q: one source API key is configured for more than one target (pipelines %s)", t, strings.Join(ps, ",")))
			}
		}
		return nil, fmt.Errorf("every tenant is blocked, so no pipeline would run: %w", errors.Join(errs...))
	}
	out := make([]tenancy.Row, 0, len(read))
	for _, row := range read {
		if row.Tenant == "" || !blocked[row.Tenant] {
			out = append(out, row)
		}
	}
	return out, nil
}

// rowsOfPipelines is the indexes of the rows whose pipeline id is in ps, in row order.
func rowsOfPipelines(rows []tenancy.Row, ps []string) []int {
	var out []int
	for i, row := range rows {
		if slices.Contains(ps, row.Pipeline) {
			out = append(out, i)
		}
	}
	return out
}

// refuseLogsLoops is assembly pass 1's log-record rule (design §2, §3): a row with a pipeline id, which
// every tenant-shape row has, may not run a loop that emits log records. Such a loop is one implementing
// source.IndexedKeyDeclarer, the capability the content denylist and the stream-label budget already use
// to tell the logs loops. A legacy row has no pipeline id and is not checked.
func refuseLogsLoops(row tenancy.Row, src source.Source) error {
	if row.Pipeline == "" {
		return nil
	}
	for _, lp := range src.Loops() {
		if _, ok := lp.(source.IndexedKeyDeclarer); ok {
			return fmt.Errorf("pipeline %q (source type %q): loop %q emits log records, which a tenant-shape pipeline may not run; disable it in defaults.%s.loops", row.Pipeline, row.Source.Type, lp.Key().Loop, row.Source.Type)
		}
	}
	return nil
}

// rowSources and pipelineSources are the source configs of a set of rows, for the opt-in helpers.
func rowSources(rows []tenancy.Row) []config.SourceConfig {
	out := make([]config.SourceConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Source)
	}
	return out
}

func pipelineSources(ps []*pipeline) []config.SourceConfig {
	out := make([]config.SourceConfig, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.row.Source)
	}
	return out
}
