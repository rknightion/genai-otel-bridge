// SPDX-License-Identifier: Apache-2.0

// Package tenancy resolves a loaded config into a plan: one row per pipeline, holding everything
// the composition root needs to assemble it (design docs/designs/multi-tenancy.md §3). The plan is
// built once, before assembly; nothing consults it at emit time. It imports internal/config only.
package tenancy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
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

// Plan is the resolved config. TenantShape is false for a legacy config.
type Plan struct {
	Rows        []Row
	TenantShape bool
}

// Resolve turns a loaded config into a plan. The legacy shape gives one row per enabled sources
// entry, in config order, each with the emit.telemetry destination and the top-level identity. It
// needs no validated config: it only copies enabled sources.
func Resolve(cfg *config.Config) (Plan, error) {
	if cfg == nil {
		return Plan{}, errors.New("tenancy: nil config")
	}
	var plan Plan
	for _, sc := range cfg.Sources {
		if !sc.Enabled {
			continue
		}
		plan.Rows = append(plan.Rows, Row{
			Source:      sc,
			Destination: cfg.Emit.Telemetry.OTLP,
			Identity:    cfg.Identity.ProductIdentity(),
		})
	}
	return plan, nil
}

// PipelineID is a tenant-shape row's pipeline id.
func PipelineID(tenant, target string) string { return tenant + "." + target }

// SourceInstance is a tenant-shape row's source_instance, which names its checkpoints.
func SourceInstance(tenant, target string) string { return "mt." + tenant + "." + target }

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
