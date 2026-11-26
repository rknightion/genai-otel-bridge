// SPDX-License-Identifier: Apache-2.0

// Package tenancy resolves a loaded config into a plan: one row per pipeline, holding everything
// the composition root needs to assemble it (design docs/designs/multi-tenancy.md §3). The plan is
// built once, before assembly; nothing consults it at emit time. It imports internal/config only.
package tenancy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/rknightion/genai-otel-bridge/internal/config"
)

// PipelineAttribute is the OTLP resource attribute that carries a tenant-shape row's pipeline id on
// its product telemetry.
const PipelineAttribute = "genai_otel_bridge.pipeline"

// Row is one resolved pipeline: a source, its destination and the identity stamped on what it emits.
// A legacy row leaves Tenant, Target, Pipeline, DestinationName, AuthValueFile and TokenFile empty.
// In the tenant shape the two secrets are file names (AuthValueFile, TokenFile) read at assembly;
// their values never enter a Row from Resolve.
type Row struct {
	Tenant          string
	Target          string
	Pipeline        string
	DestinationName string
	AuthValueFile   string
	TokenFile       string
	// Source is the source entry with Enabled true. For a legacy row it is the config entry copied
	// unchanged, so SourceInstance (the checkpoint namespace) is the operator's own.
	Source      config.SourceConfig
	Destination config.OTLPConn
	// Identity is the product resource identity; each row owns its map.
	Identity map[string]string
}

// SecretReader reads one tenant's secret file at assembly (design §4, pass 1). Read returns the
// file's value; its error names the tenant and the file and never carries a value.
type SecretReader interface {
	Read(tenant, file string) (string, error)
}

// ErrTenantShapeNotEnabled is returned for a tenant-shape config: this version parses and validates the
// shape but does not run it. main returns it right after app.Prepare, and app.ValidateConfigFile before it
// would build.
var ErrTenantShapeNotEnabled = errors.New("the tenant shape is not enabled in this version")

// Plan is the resolved config. TenantShape is false for a legacy config.
type Plan struct {
	Rows        []Row
	TenantShape bool
}

// Resolve turns a loaded config into a plan.
//
// The legacy shape gives one row per enabled sources entry, in config order, each with the
// emit.telemetry destination and the top-level identity. It needs no validated config: it only copies
// enabled sources. It logs a warning (slog's default logger) for each enabled source whose
// source_instance starts with "mt.", the prefix of the tenant shape's checkpoint keys (design §7).
//
// A config with at least one tenant gives a plan with TenantShape set and one row per target, sorted by
// tenant id then target name (resolveTenants), after the resolve-phase rules of design §3. In this
// version main and the config check still refuse that plan with ErrTenantShapeNotEnabled.
func Resolve(cfg *config.Config) (Plan, error) {
	if cfg == nil {
		return Plan{}, errors.New("tenancy: nil config")
	}
	if len(cfg.Tenants) > 0 {
		return resolveTenants(cfg)
	}
	var plan Plan
	for _, sc := range cfg.Sources {
		if !sc.Enabled {
			continue
		}
		if strings.HasPrefix(sc.SourceInstance, tenantInstancePrefix) {
			slog.Warn("a legacy source_instance starts with \""+tenantInstancePrefix+"\", the prefix the tenant shape gives its checkpoint keys: "+
				"if this install is converted to the tenant shape, a tenant and target with the same names would share this source's checkpoints",
				"source_instance", sc.SourceInstance)
		}
		plan.Rows = append(plan.Rows, Row{
			Source:      sc,
			Destination: cfg.Emit.Telemetry.OTLP,
			Identity:    cfg.Identity.ProductIdentity(),
		})
	}
	return plan, nil
}

// tenantInstancePrefix starts every tenant-shape source_instance (SourceInstance).
const tenantInstancePrefix = "mt."

// resolveTenants builds the tenant-shape plan: one row per target, sorted by tenant id then target
// name, and the resolve-phase rules of design §2 and §3:
//
//   - a target names a destination of its own tenant;
//   - no two tenants declare a destination on one stack (StackKey);
//   - no two targets of one tenant send to one stack with one identity;
//   - no target sends to the self-telemetry stack (emit.self, else emit.telemetry, as main falls back)
//     with the self-telemetry identity (the top-level identity with config.SelfNamespaceSuffix);
//   - no scope name and no scope id appears in two tenants. config.Validate checks this at load too;
//     Resolve does not call Validate, so it holds the rule itself.
//
// Every broken rule is reported, in tenant and target order, and the plan is then empty. Each row owns
// its maps and slices: the loops are deep-copied from defaults.<type>.loops, settings and graphs
// included, so changing one row never changes another row or the config. A row's secrets are file
// names (AuthValueFile, TokenFile); Source.Auth.Value and Destination.Token stay empty.
func resolveTenants(cfg *config.Config) (Plan, error) {
	var errs []error
	ids := sortedKeys(cfg.Tenants)

	// Two tenants on one stack, over every declared destination: the first tenant in id order to
	// declare a stack owns it.
	stackOwner := map[string]string{}
	for _, id := range ids {
		dests := cfg.Tenants[id].Destinations
		for _, dn := range sortedKeys(dests) {
			o := dests[dn].OTLP
			k := StackKey(o.Endpoint, o.InstanceID)
			if owner, ok := stackOwner[k]; !ok {
				stackOwner[k] = id
			} else if owner != id {
				errs = append(errs, fmt.Errorf("tenants.%s.destinations.%s is on a stack (endpoint and instance_id) that tenant %s also sends to: two tenants may not share a stack", id, dn, owner))
			}
		}
	}

	// A scope name or id in two tenants: the first tenant in id order to name it owns it. One tenant
	// may name one scope in two targets.
	scopeOwner := map[string]map[string]string{"name": {}, "id": {}}
	for _, id := range ids {
		targets := cfg.Tenants[id].Targets
		for _, tn := range sortedKeys(targets) {
			sc := targets[tn].Scope
			for _, f := range []struct{ field, value string }{{"name", sc.Name}, {"id", sc.ID}} {
				if f.value == "" {
					continue
				}
				if owner, ok := scopeOwner[f.field][f.value]; !ok {
					scopeOwner[f.field][f.value] = id
				} else if owner != id {
					errs = append(errs, fmt.Errorf("tenants.%s.targets.%s.scope.%s %q is also used by tenant %s: a scope belongs to one tenant", id, tn, f.field, f.value, owner))
				}
			}
		}
	}

	self := cfg.Emit.Telemetry.OTLP
	if cfg.Emit.Self != nil {
		self = cfg.Emit.Self.OTLP
	}
	selfStack := StackKey(self.Endpoint, self.InstanceID)
	selfIdentity := config.IdentityConfig{
		ServiceNamespace:      cfg.Identity.ServiceNamespace + config.SelfNamespaceSuffix,
		DeploymentEnvironment: cfg.Identity.DeploymentEnvironment,
	}.ProductIdentity()

	plan := Plan{TenantShape: true}
	for _, id := range ids {
		tc := cfg.Tenants[id]
		seen := map[string]string{} // stack and identity -> the first target of this tenant on them
		for _, tn := range sortedKeys(tc.Targets) {
			tg := tc.Targets[tn]
			tp := "tenants." + id + ".targets." + tn
			dest, ok := tc.Destinations[tg.Destination]
			if !ok {
				errs = append(errs, fmt.Errorf("%s.destination %q is not a destination of tenant %s", tp, tg.Destination, id))
				continue
			}
			idc := tc.Identity
			if tg.Identity != nil {
				idc = *tg.Identity // a target's identity block replaces the tenant's whole
			}
			identity := idc.ProductIdentity()
			stack := StackKey(dest.OTLP.Endpoint, dest.OTLP.InstanceID)
			key := stack + "\x00" + identityKey(identity)
			if first, ok := seen[key]; ok {
				errs = append(errs, fmt.Errorf("%s and tenants.%s.targets.%s send to one stack with one identity: they would write the same series, so give one of them its own identity", tp, id, first))
			} else {
				seen[key] = tn
			}
			if stack == selfStack && maps.Equal(identity, selfIdentity) {
				errs = append(errs, fmt.Errorf("%s sends to the self-telemetry stack with the self-telemetry identity (service.namespace %q): give it another identity or destination", tp, selfIdentity["service.namespace"]))
			}
			plan.Rows = append(plan.Rows, Row{
				Tenant:          id,
				Target:          tn,
				Pipeline:        PipelineID(id, tn),
				DestinationName: tg.Destination,
				AuthValueFile:   tg.Source.Auth.ValueFile,
				TokenFile:       dest.OTLP.TokenFile,
				Source: config.SourceConfig{
					Type:           tg.Source.Type,
					Enabled:        true,
					BaseURL:        tg.Source.BaseURL,
					SourceInstance: SourceInstance(id, tn),
					Auth:           config.AuthConfig{Header: tg.Source.Auth.Header},
					RateLimit:      tg.Source.RateLimit,
					HTTP: config.HTTPConfig{
						UserAgent:    tg.Source.HTTP.UserAgent,
						AllowHosts:   slices.Clone(tg.Source.HTTP.AllowHosts),
						AllowPrivate: tg.Source.HTTP.AllowPrivate,
					},
					Loops: copyLoops(cfg.Defaults[tg.Source.Type].Loops),
					Scope: tg.Scope,
				},
				Destination: config.OTLPConn{Endpoint: dest.OTLP.Endpoint, InstanceID: dest.OTLP.InstanceID},
				Identity:    identity,
			})
		}
	}
	if len(errs) > 0 {
		return Plan{}, errors.Join(errs...)
	}
	return plan, nil
}

// copyLoops deep-copies a defaults loop map: a new map, and in each block a new graphs slice and a new
// settings map, so no two rows share one.
func copyLoops(in map[string]config.LoopConfig) map[string]config.LoopConfig {
	if in == nil {
		return nil
	}
	out := make(map[string]config.LoopConfig, len(in))
	for name, lc := range in {
		lc.Graphs = slices.Clone(lc.Graphs)
		lc.Settings = maps.Clone(lc.Settings)
		out[name] = lc
	}
	return out
}

// identityKey is an unambiguous comparison key for an identity map: each key and value Go-quoted,
// sorted by key.
func identityKey(id map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(id) {
		b.WriteString(strconv.Quote(k))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(id[k]))
		b.WriteByte(',')
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PipelineID is a tenant-shape row's pipeline id.
func PipelineID(tenant, target string) string { return tenant + "." + target }

// SourceInstance is a tenant-shape row's source_instance, which names its checkpoints.
func SourceInstance(tenant, target string) string {
	return tenantInstancePrefix + tenant + "." + target
}

// StackKey is the one definition of "the same stack": two destinations are the same stack when their
// instance ids are equal and their endpoints are equal after lower-casing the host (url.Parse also
// lower-cases the scheme) and dropping a trailing slash. The key holds the endpoint otherwise as
// written, so it is a comparison key, not a value to log.
func StackKey(endpoint, instanceID string) string {
	ep := endpoint
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		u.Host = strings.ToLower(u.Host)
		ep = u.String()
	}
	return strings.TrimRight(ep, "/") + "|" + instanceID
}

// tableHeader is the plan table's columns (design §3). The layout is not a contract across versions:
// the hash is compared only between CI and a leader on the same image.
var tableHeader = []string{
	"tenant", "target", "source_type", "source_url", "scope_name", "scope_id", "pipeline",
	"destination", "endpoint", "instance_id", "identity", "loops",
}

// Table renders the plan as text, one header line and one line per row, with no secret: it never
// prints a source's auth value or a destination's token, and it drops the userinfo, query and fragment
// of every URL it prints. A legacy row shows its source_instance in the pipeline column. An empty
// value prints as "-"; a value holding white space, a control character, a quote or invalid UTF-8 is
// Go-quoted, so each row is one line with one cell per column.
func (p Plan) Table() string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	writeLine(tw, tableHeader)
	for _, r := range p.Rows {
		pipeline := r.Pipeline
		if pipeline == "" {
			pipeline = r.Source.SourceInstance
		}
		writeLine(tw, []string{
			r.Tenant,
			r.Target,
			r.Source.Type,
			displayURL(r.Source.BaseURL),
			r.Source.Scope.Name, // "-" for a row with no scope, as every legacy row is
			r.Source.Scope.ID,
			pipeline,
			r.DestinationName,
			displayURL(r.Destination.Endpoint),
			r.Destination.InstanceID,
			identityCell(r.Identity),
			loopsCell(r.Source.Loops),
		})
	}
	_ = tw.Flush() // writes to a strings.Builder, which never fails
	return b.String()
}

// Hash is the lower-case hex SHA-256 of Table().
func (p Plan) Hash() string {
	sum := sha256.Sum256([]byte(p.Table()))
	return hex.EncodeToString(sum[:])
}

func writeLine(tw *tabwriter.Writer, cells []string) {
	for i, c := range cells {
		if i > 0 {
			_, _ = tw.Write([]byte{'\t'})
		}
		_, _ = tw.Write([]byte(cell(c)))
	}
	_, _ = tw.Write([]byte{'\n'})
}

func cell(s string) string {
	if s == "" {
		return "-"
	}
	if s == "-" || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"'
	}) {
		return strconv.Quote(s)
	}
	return s
}

// displayURL prints a URL without the parts that can carry a credential. A value that does not parse
// is not printed at all.
func displayURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

func identityCell(id map[string]string) string {
	keys := make([]string, 0, len(id))
	for k := range id {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+id[k])
	}
	return strings.Join(parts, ",")
}

func loopsCell(loops map[string]config.LoopConfig) string {
	var names []string
	for name, lc := range loops {
		if lc.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
