// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"maps"
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
			Metrics: p.t, Pipeline: pl,
		})
	}
	return specs
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

// readRowSecrets returns a copy of the row with its secret files read through r: AuthValueFile into
// Source.Auth.Value and TokenFile into Destination.Token. A row that names no file is returned as it is
// and r is not consulted, so a legacy row (whose secrets are already in its config) needs no reader. A
// read error, or a nil reader for a row that names a file, fails with the tenant and the file named; the
// message never carries a value (the reader's own error names tenant and file only, by its contract).
func readRowSecrets(row tenancy.Row, r tenancy.SecretReader) (tenancy.Row, error) {
	read := func(file string) (string, error) {
		if r == nil {
			return "", fmt.Errorf("tenant %q: secret file %q: no secret reader is configured", row.Tenant, file)
		}
		v, err := r.Read(row.Tenant, file)
		if err != nil {
			return "", fmt.Errorf("tenant %q: secret file %q could not be read: %w", row.Tenant, file, err)
		}
		return v, nil
	}
	if row.AuthValueFile != "" {
		v, err := read(row.AuthValueFile)
		if err != nil {
			return tenancy.Row{}, err
		}
		row.Source.Auth.Value = v
	}
	if row.TokenFile != "" {
		v, err := read(row.TokenFile)
		if err != nil {
			return tenancy.Row{}, err
		}
		row.Destination.Token = v
	}
	return row, nil
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
