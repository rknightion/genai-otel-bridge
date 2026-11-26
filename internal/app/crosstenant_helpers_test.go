// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// Helpers of the cross-tenant release gate (unit 5b-1, #528; design §5.3). The gate builds tenant
// pipelines from hand-made plan rows through BuildFromPlan and OTLPEmitterFactory, runs them against one
// fake vendor and one fake OTLP receiver, and judges what arrived with four checks. Every check returns
// findings, so the seeded faults in crosstenant_gate_test.go can show that each one reports what it is
// there to catch.

// The four checks, as a finding names them.
const (
	checkDelivery     = "delivery"
	checkSlug         = "slug"
	checkSelfEndpoint = "self-endpoint"
	checkSelfIdentity = "self-identity"
)

// gateChecks is every check in a fixed order.
var gateChecks = []string{checkDelivery, checkSlug, checkSelfEndpoint, checkSelfIdentity}

// selfMetricPrefix is the name prefix of every self-telemetry metric (selfobs.NewMetrics). Any other
// metric name is product telemetry.
const selfMetricPrefix = "genai_otel_bridge_"

// gateTenant is one tenant of the gate: one target, its scope (the workspace its key reads), its API key,
// its destination credential and the marker its workspace's data carries.
type gateTenant struct {
	Tenant, Target string
	// Workspace is the scope name, the workspace slug.
	Workspace, ScopeID string
	Key                string
	// OrgKey makes Key an organisation key: grouping and data follow the slug, and with no slug the key
	// reads DefaultWorkspace. Otherwise Key is a workspace key, bound to Workspace whatever the slug.
	OrgKey           bool
	DefaultWorkspace string
	InstanceID       string
	Token            string
	// Marker is the value of every data point of Workspace's analytics data.
	Marker float64
}

func (g gateTenant) pipeline() string   { return tenancy.PipelineID(g.Tenant, g.Target) }
func (g gateTenant) credential() string { return g.InstanceID + ":" + g.Token }

// gateTenants are the tenants every gate world starts from. tenant-c holds an organisation key whose
// default workspace is tenant-a's, so a request of tenant-c's that loses its slug reads tenant-a's data.
// tenant-d is the tenant that takes over tenant-c's target name once tenant-c is removed.
func gateTenants() []gateTenant {
	return []gateTenant{
		{Tenant: "tenant-a", Target: "primary", Workspace: "ws-a", ScopeID: "ws-id-a", Key: "key-a", InstanceID: "1001", Token: "token-a", Marker: 1111},
		{Tenant: "tenant-b", Target: "secondary", Workspace: "ws-b", ScopeID: "ws-id-b", Key: "key-b", InstanceID: "1002", Token: "token-b", Marker: 2222},
		{Tenant: "tenant-c", Target: "spare", Workspace: "ws-c", ScopeID: "ws-id-c", Key: "key-c", OrgKey: true, DefaultWorkspace: "ws-a", InstanceID: "1003", Token: "token-c", Marker: 3333},
		{Tenant: "tenant-d", Target: "spare", Workspace: "ws-d", ScopeID: "ws-id-d", Key: "key-d", InstanceID: "1004", Token: "token-d", Marker: 4444},
	}
}

// --- the fake vendor ---

// vendorRequest is one request the fake vendor saw: the plan row whose base URL it came through, the
// endpoint, the API key and the workspace_slug, if any.
type vendorRequest struct {
	Row, Path, Key, Slug string
	HasSlug              bool
}

// vendorKey is what one API key reads.
type vendorKey struct {
	workspace        string
	org              bool
	defaultWorkspace string
}

// markerVendor is a fake Portkey analytics API. Each row's base URL is the vendor's URL plus
// "/r/<pipeline id>", so every request names the row that sent it; the vendor records that for the slug
// check and never answers by it. It answers by API key and slug only:
//   - the workspace grouping returns the one workspace the key reads (with a slug, an organisation key
//     reads the slug's workspace; a workspace key always reads its own);
//   - each analytics graph returns buckets one-minute buckets before now (nine unless a world sets more),
//     every value the marker of the workspace the key reads, or of foreign[key] when that fault is seeded
//     (latency in milliseconds, so the seconds the loop emits are the marker too);
//   - every other groups endpoint returns an empty page; an unknown key gets 401.
type markerVendor struct {
	*httptest.Server
	mu       sync.Mutex
	keys     map[string]vendorKey
	markers  map[string]float64 // by workspace
	foreign  map[string]string  // seeded fault: key -> the workspace whose marker its data carries
	buckets  int
	now      time.Time
	requests []vendorRequest
}

func newMarkerVendor(t *testing.T, tenants []gateTenant, now time.Time) *markerVendor {
	t.Helper()
	v := &markerVendor{keys: map[string]vendorKey{}, markers: map[string]float64{}, foreign: map[string]string{}, buckets: 9, now: now}
	for _, g := range tenants {
		v.keys[g.Key] = vendorKey{workspace: g.Workspace, org: g.OrgKey, defaultWorkspace: g.DefaultWorkspace}
		v.markers[g.Workspace] = g.Marker
	}
	v.Server = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.Close)
	return v
}

func (v *markerVendor) serve(w http.ResponseWriter, r *http.Request) {
	row, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/r/"), "/")
	path = "/" + path
	key := r.Header.Get("x-portkey-api-key")
	slug, hasSlug := r.URL.Query()["workspace_slug"]
	req := vendorRequest{Row: row, Path: path, Key: key, HasSlug: hasSlug}
	if hasSlug {
		req.Slug = slug[0]
	}
	v.mu.Lock()
	v.requests = append(v.requests, req)
	k, known := v.keys[key]
	now := v.now
	ws := k.workspace
	if k.org {
		ws = k.defaultWorkspace
		if hasSlug {
			ws = req.Slug
		}
	}
	dataWS := ws
	if f, ok := v.foreign[key]; ok {
		dataWS = f
	}
	marker := v.markers[dataWS]
	buckets := v.buckets
	v.mu.Unlock()
	if !known {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasSuffix(path, "/analytics/groups/workspace"):
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "total": 1, "is_quota_exceeded": false,
			"data": []map[string]any{{"workspace": ws, "requests": 1}}})
	case strings.Contains(path, "/analytics/groups/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "total": 0, "data": []any{}})
	case strings.Contains(path, "/analytics/graphs/"):
		var dps []map[string]any
		for i := buckets; i >= 1; i-- {
			ts := now.Add(-time.Duration(i) * time.Minute)
			dps = append(dps, map[string]any{"timestamp": ts.Format(time.RFC3339),
				"total": marker, "total_request_units": marker, "total_response_units": marker,
				"avg": marker * 1000, "p50": marker * 1000, "p90": marker * 1000, "p99": marker * 1000})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": marker}, "data_points": dps, "is_quota_exceeded": false})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (v *markerVendor) setNow(now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.now = now
}

// answerWithForeignMarker seeds the fault of a vendor answering key's data with workspace's marker while
// its workspace grouping still names the key's own workspace.
func (v *markerVendor) answerWithForeignMarker(key, workspace string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.foreign[key] = workspace
}

func (v *markerVendor) seen() []vendorRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.requests)
}

// newSlugStripProxy is an HTTP proxy in front of target that drops workspace_slug from every request
// and forwards it otherwise unchanged. A row reaches it through its base URL, so no production code is
// involved in the fault.
func newSlugStripProxy(t *testing.T, target string) *httptest.Server {
	t.Helper()
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		q.Del("workspace_slug")
		out, err := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.Path+"?"+q.Encode(), nil)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(out)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.Close)
	return p
}

// --- the fake OTLP receiver ---

// payloadPoint is one number data point of a received metrics payload.
type payloadPoint struct {
	Metric string
	Value  float64
	Attrs  map[string]string
	Time   time.Time
}

// payloadResource is one resource of a received payload: its attributes, the names of every metric it
// carries, its number data points, and its log record count.
type payloadResource struct {
	Attrs   map[string]string
	Metrics []string
	Points  []payloadPoint
	Logs    int
}

// otlpPayload is one request the receiver got, accepted or refused: the Basic-auth credential
// ("instance:token") it carried, its path, the status the receiver answered with, and its decoded body.
type otlpPayload struct {
	Credential, Path string
	Status           int
	DecodeErr        string
	Resources        []payloadResource
}

func (p otlpPayload) points() int {
	n := 0
	for _, r := range p.Resources {
		n += len(r.Points)
	}
	return n
}

// otlpReceiver is one fake OTLP endpoint on one URL that records every payload by credential. Its
// faults apply to every credential but exempt's (the self-telemetry one): failOnce answers the first
// request of each credential 503, and refuseOver answers 413 to any payload with more than that many
// data points.
type otlpReceiver struct {
	*httptest.Server
	exempt     string
	mu         sync.Mutex
	failOnce   bool
	refuseOver int
	failed     map[string]bool
	payloads   []otlpPayload
}

func newOTLPReceiver(t *testing.T, exempt string) *otlpReceiver {
	t.Helper()
	rc := &otlpReceiver{exempt: exempt, failed: map[string]bool{}}
	rc.Server = httptest.NewServer(http.HandlerFunc(rc.serve))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *otlpReceiver) serve(w http.ResponseWriter, req *http.Request) {
	p := otlpPayload{Path: req.URL.Path}
	if user, pass, ok := req.BasicAuth(); ok {
		p.Credential = user + ":" + pass
	}
	body, err := io.ReadAll(req.Body)
	if err == nil && req.Header.Get("Content-Encoding") == "gzip" {
		var zr *gzip.Reader
		if zr, err = gzip.NewReader(bytes.NewReader(body)); err == nil {
			body, err = io.ReadAll(zr)
		}
	}
	if err == nil {
		p.Resources, err = decodeOTLP(req.URL.Path, body)
	}
	if err != nil {
		p.DecodeErr = err.Error()
	}
	rc.mu.Lock()
	p.Status = http.StatusOK
	switch {
	case p.DecodeErr != "":
		p.Status = http.StatusBadRequest
	case p.Credential == rc.exempt:
	case rc.failOnce && !rc.failed[p.Credential]:
		rc.failed[p.Credential] = true
		p.Status = http.StatusServiceUnavailable
	case rc.refuseOver > 0 && p.points() > rc.refuseOver:
		p.Status = http.StatusRequestEntityTooLarge
	}
	rc.payloads = append(rc.payloads, p)
	rc.mu.Unlock()
	w.WriteHeader(p.Status)
}

func (rc *otlpReceiver) setFaults(failOnce bool, refuseOver int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.failOnce, rc.refuseOver = failOnce, refuseOver
}

func (rc *otlpReceiver) all() []otlpPayload {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.payloads)
}

// under is every payload received under credential.
func (rc *otlpReceiver) under(credential string) []otlpPayload {
	var out []otlpPayload
	for _, p := range rc.all() {
		if p.Credential == credential {
			out = append(out, p)
		}
	}
	return out
}

func decodeOTLP(path string, body []byte) ([]payloadResource, error) {
	switch path {
	case "/v1/metrics":
		var msg collectormetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &msg); err != nil {
			return nil, err
		}
		var out []payloadResource
		for _, rm := range msg.ResourceMetrics {
			res := payloadResource{Attrs: stringAttrs(rm.GetResource().GetAttributes())}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					res.Metrics = append(res.Metrics, m.Name)
					var dps []*metricspb.NumberDataPoint
					switch {
					case m.GetGauge() != nil:
						dps = m.GetGauge().DataPoints
					case m.GetSum() != nil:
						dps = m.GetSum().DataPoints
					}
					for _, dp := range dps {
						v := dp.GetAsDouble()
						if _, ok := dp.Value.(*metricspb.NumberDataPoint_AsInt); ok {
							v = float64(dp.GetAsInt())
						}
						res.Points = append(res.Points, payloadPoint{Metric: m.Name, Value: v, Attrs: stringAttrs(dp.Attributes), Time: time.Unix(0, int64(dp.TimeUnixNano)).UTC()})
					}
				}
			}
			out = append(out, res)
		}
		return out, nil
	case "/v1/logs":
		var msg collectorlogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &msg); err != nil {
			return nil, err
		}
		var out []payloadResource
		for _, rl := range msg.ResourceLogs {
			res := payloadResource{Attrs: stringAttrs(rl.GetResource().GetAttributes())}
			for _, sl := range rl.ScopeLogs {
				res.Logs += len(sl.LogRecords)
			}
			out = append(out, res)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown OTLP path %q", path)
	}
}

func stringAttrs(kvs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.Value.GetStringValue()
	}
	return out
}

// --- the four checks ---

// finding is one thing a check reports: which check, the pipeline (or credential) it is about, and what
// was wrong.
type finding struct {
	Check, Subject, Detail string
}

func (f finding) String() string { return f.Check + " " + f.Subject + ": " + f.Detail }

// markerOwner names the pipeline whose marker v is, or "an unknown marker".
func markerOwner(tenants []gateTenant, v float64) string {
	for _, g := range tenants {
		if g.Marker == v {
			return "the marker of " + g.pipeline()
		}
	}
	return fmt.Sprintf("an unknown marker %v", v)
}

// credentialHolders maps each tenant credential to the pipelines that send under it, in tenant order. In
// the hand-made worlds each credential is one pipeline's; two targets of one tenant on one destination
// (the fixture's team-a) share one.
func credentialHolders(tenants []gateTenant) map[string][]gateTenant {
	out := map[string][]gateTenant{}
	for _, g := range tenants {
		out[g.credential()] = append(out[g.credential()], g)
	}
	return out
}

// holdersSubject names the pipelines holding one credential, comma-joined.
func holdersSubject(gs []gateTenant) string {
	ps := make([]string, 0, len(gs))
	for _, g := range gs {
		ps = append(ps, g.pipeline())
	}
	return strings.Join(ps, ",")
}

// deliveryCheck: every payload received under a tenant credential, refused ones included, carries only
// the pipeline id of a pipeline holding that credential, and every point of a resource carries that
// pipeline's marker. A resource whose pipeline id no holder has is a finding, and its points are judged
// against the one holder's marker (with several holders, against any holder's). A payload under a
// credential that is neither a tenant's nor self is a finding, and so is one that cannot be decoded.
// Subject: the receiving pipeline (the holders, comma-joined, when a resource names none of them).
func deliveryCheck(payloads []otlpPayload, tenants []gateTenant, selfCred string) []finding {
	byCred := credentialHolders(tenants)
	var out []finding
	for _, p := range payloads {
		if p.Credential == selfCred {
			continue
		}
		gs, ok := byCred[p.Credential]
		if !ok {
			out = append(out, finding{checkDelivery, "unknown credential", fmt.Sprintf("a payload to %s under a credential no tenant holds", p.Path)})
			continue
		}
		subject := holdersSubject(gs)
		if p.DecodeErr != "" {
			out = append(out, finding{checkDelivery, subject, "a payload that does not decode: " + p.DecodeErr})
		}
		for _, r := range p.Resources {
			got := r.Attrs[tenancy.PipelineAttribute]
			idx := slices.IndexFunc(gs, func(g gateTenant) bool { return g.pipeline() == got })
			markers := gs
			if idx >= 0 {
				markers = gs[idx : idx+1]
			} else {
				out = append(out, finding{checkDelivery, subject, fmt.Sprintf("a resource with %s=%q", tenancy.PipelineAttribute, got)})
			}
			at := subject
			if len(markers) == 1 {
				at = markers[0].pipeline()
			}
			for _, pt := range r.Points {
				if !slices.ContainsFunc(markers, func(g gateTenant) bool { return g.Marker == pt.Value }) {
					out = append(out, finding{checkDelivery, at, fmt.Sprintf("%s carries %s", pt.Metric, markerOwner(tenants, pt.Value))})
				}
			}
		}
	}
	return dedupeFindings(out)
}

// slugCheck: every vendor request a row sent carries that row's own scope name as workspace_slug. The
// sender is the row whose base URL the request came through; the scope name is the tenant's workspace,
// whether or not the row was given its scope. Subject: the sending pipeline.
func slugCheck(reqs []vendorRequest, tenants []gateTenant) []finding {
	byRow := map[string]gateTenant{}
	for _, g := range tenants {
		byRow[g.pipeline()] = g
	}
	var out []finding
	for _, r := range reqs {
		g, ok := byRow[r.Row]
		switch {
		case !ok:
			out = append(out, finding{checkSlug, "unknown row", fmt.Sprintf("a request to %s from row %q", r.Path, r.Row)})
		case !r.HasSlug:
			out = append(out, finding{checkSlug, g.pipeline(), fmt.Sprintf("a request to %s with no workspace_slug", r.Path)})
		case r.Slug != g.Workspace:
			out = append(out, finding{checkSlug, g.pipeline(), fmt.Sprintf("a request to %s with workspace_slug %q, want %q", r.Path, r.Slug, g.Workspace)})
		}
	}
	return dedupeFindings(out)
}

// selfEndpointCheck: no payload under the self credential carries a product metric (any name outside
// selfMetricPrefix), a log record or a genai_otel_bridge.pipeline resource. Subject: the pipeline the
// resource names, or "self".
func selfEndpointCheck(payloads []otlpPayload, selfCred string) []finding {
	var out []finding
	for _, p := range payloads {
		if p.Credential != selfCred {
			continue
		}
		for _, r := range p.Resources {
			subject := r.Attrs[tenancy.PipelineAttribute]
			if subject == "" {
				subject = "self"
			}
			if _, ok := r.Attrs[tenancy.PipelineAttribute]; ok {
				out = append(out, finding{checkSelfEndpoint, subject, "a resource with " + tenancy.PipelineAttribute})
			}
			if r.Logs > 0 {
				out = append(out, finding{checkSelfEndpoint, subject, "log records"})
			}
			for _, m := range r.Metrics {
				if !strings.HasPrefix(m, selfMetricPrefix) {
					out = append(out, finding{checkSelfEndpoint, subject, "product metric " + m})
				}
			}
		}
	}
	return dedupeFindings(out)
}

// selfIdentityCheck: no payload under any credential but the self one carries a resource with the self
// identity's service.namespace. Subject: the receiving pipeline, or the credential when no tenant holds it.
func selfIdentityCheck(payloads []otlpPayload, tenants []gateTenant, selfCred, selfNamespace string) []finding {
	byCred := credentialHolders(tenants)
	var out []finding
	for _, p := range payloads {
		if p.Credential == selfCred {
			continue
		}
		subject := "unknown credential"
		if gs, ok := byCred[p.Credential]; ok {
			subject = holdersSubject(gs)
		}
		for _, r := range p.Resources {
			if r.Attrs["service.namespace"] == selfNamespace {
				out = append(out, finding{checkSelfIdentity, subject, "a resource with the self identity " + selfNamespace})
			}
		}
	}
	return dedupeFindings(out)
}

func dedupeFindings(fs []finding) []finding {
	seen := map[finding]bool{}
	var out []finding
	for _, f := range fs {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// subjects is the sorted set of subjects of fs.
func subjects(fs []finding) []string {
	set := map[string]bool{}
	for _, f := range fs {
		set[f.Subject] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// --- the gate world ---

// gateWorld is one run of the gate: the receiver every destination and the self-telemetry share, the
// vendor, the tenants the checks know, their secrets, one checkpoint file every app of the run shares,
// and self-telemetry wired as main wires it.
type gateWorld struct {
	t       *testing.T
	cfg     *config.Config
	recv    *otlpReceiver
	vendor  *markerVendor
	tenants []gateTenant
	secrets *mapSecrets
	cpPath  string

	selfInstance, selfToken, selfNamespace string
	mp                                     *sdkmetric.MeterProvider
	metrics                                *selfobs.Metrics

	// maxBatchBytes is the max_batch_bytes OTLPEmitterFactory is built with.
	maxBatchBytes int

	clockMu sync.Mutex
	now     time.Time
	epoch   int64

	// blocked is every OnTenantBlocked call of every build, as "tenant reason" for a blocking one.
	blockedMu sync.Mutex
	blocked   []string
}

func newGateWorld(t *testing.T) *gateWorld {
	t.Helper()
	return newGateWorldWith(t, gateTenants(), 9)
}

// newGateWorldWith is a gate world over the given tenants, whose vendor serves buckets one-minute buckets
// of history.
func newGateWorldWith(t *testing.T, tenants []gateTenant, buckets int) *gateWorld {
	t.Helper()
	w := &gateWorld{
		t: t, tenants: tenants, cpPath: filepath.Join(t.TempDir(), "wm.yaml"),
		selfInstance: "9000", selfToken: "self-token", maxBatchBytes: 1 << 20, now: assemblyAnchor,
	}
	w.recv = newOTLPReceiver(t, w.selfCred())
	w.vendor = newMarkerVendor(t, w.tenants, w.now)
	w.vendor.buckets = buckets
	w.cfg = minimalConfig(w.vendor.URL)
	w.secrets = &mapSecrets{values: map[[2]string]string{}}
	for _, g := range w.tenants {
		w.secrets.values[[2]string{g.Tenant, "api-key-" + g.Target}] = g.Key
		w.secrets.values[[2]string{g.Tenant, "otlp-token-" + g.Target}] = g.Token
	}
	// Self-telemetry as main wires it (newSelfProvider, then selfobs.NewMetrics and metrics.With as the
	// telemetry factory), pointed at the same receiver under its own credential and the self identity.
	w.selfNamespace = w.cfg.Identity.ServiceNamespace + config.SelfNamespaceSuffix
	mp, shutdown, err := selfobs.NewProvider(context.Background(), selfobs.ProviderConfig{
		Endpoint: w.recv.URL, InstanceID: w.selfInstance, Token: w.selfToken,
		ServiceNamespace: w.selfNamespace, Environment: w.cfg.Identity.DeploymentEnvironment,
		Instance: "replica-1", Version: "test", MaxDPM: w.cfg.Governance.MaxDPM,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	w.mp = mp
	if w.metrics, err = selfobs.NewMetrics(mp); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *gateWorld) selfCred() string { return w.selfInstance + ":" + w.selfToken }

func (w *gateWorld) tenant(name string) gateTenant {
	w.t.Helper()
	for _, g := range w.tenants {
		if g.Tenant == name {
			return g
		}
	}
	w.t.Fatalf("no gate tenant %q", name)
	return gateTenant{}
}

func (w *gateWorld) clock() time.Time {
	w.clockMu.Lock()
	defer w.clockMu.Unlock()
	return w.now
}

// advance moves the vendor's clock and every app's loop and gate clock forward by d.
func (w *gateWorld) advance(d time.Duration) {
	w.clockMu.Lock()
	w.now = w.now.Add(d)
	now := w.now
	w.clockMu.Unlock()
	w.vendor.setNow(now)
}

// row is the plan row of a tenant: its target's source behind the vendor (base URL tagged with the
// pipeline id), the analytics loop over four graphs whose values are raw (six samples a bucket) and the
// groups loop, its scope, its API key and OTLP token as secret files, and its destination on the
// receiver's one URL under its own instance id.
func (w *gateWorld) row(g gateTenant) tenancy.Row {
	r := tenantRow(w.vendor.URL+"/r/"+g.pipeline(), g.Tenant, g.Target, w.recv.URL, g.InstanceID)
	analytics := r.Source.Loops["analytics"]
	analytics.Graphs = []string{"requests", "tokens", "errors", "users"}
	r.Source.Loops["analytics"] = analytics
	r.Source.Loops["groups"] = config.LoopConfig{Enabled: true, Cadence: config.Duration(5 * time.Minute), MetricPrefix: "portkey_api",
		Settings: map[string]string{"page_size": "100"}}
	r.Source.Scope = config.ScopeConfig{Name: g.Workspace, ID: g.ScopeID}
	r.AuthValueFile, r.TokenFile = "api-key-"+g.Target, "otlp-token-"+g.Target
	r.Source.Auth.Value, r.Destination.Token = "", ""
	return r
}

// rows is the plan rows of the named tenants, in that order.
func (w *gateWorld) rows(names ...string) []tenancy.Row {
	out := make([]tenancy.Row, 0, len(names))
	for _, n := range names {
		out = append(out, w.row(w.tenant(n)))
	}
	return out
}

// build assembles rows through BuildFromPlan with OTLPEmitterFactory (the fast retry policy of
// recorder_test.go's buildCycleApp), optionally wrapped by a seeded fault, over a fresh file store on
// the world's one checkpoint file, as a new process would. Every row but a seeded fault's must send to
// the receiver's one URL.
func (w *gateWorld) build(rows []tenancy.Row, wrap func(EmitterFactory) EmitterFactory) *App {
	w.t.Helper()
	return w.buildPlan(w.cfg, tenancy.Plan{Rows: rows}, wrap)
}

// buildPlan is build for a whole plan and the config it was resolved from. OnTenantBlocked is wired as main
// wires it, to the self-telemetry's TenantBlocked, and also recorded for blockedFor.
func (w *gateWorld) buildPlan(cfg *config.Config, plan tenancy.Plan, wrap func(EmitterFactory) EmitterFactory) *App {
	w.t.Helper()
	rows := plan.Rows
	for _, r := range rows {
		if r.Destination.Endpoint != w.recv.URL {
			w.t.Fatalf("row %s sends to %s, not the receiver's one URL %s", r.Pipeline, r.Destination.Endpoint, w.recv.URL)
		}
	}
	cp, err := file.New(w.cpPath, false)
	if err != nil {
		w.t.Fatal(err)
	}
	ef := OTLPEmitterFactory(w.maxBatchBytes, fastRetry(), logging.NewLimiter(time.Minute))
	if wrap != nil {
		ef = wrap(ef)
	}
	tf := func(tenant, pipeline string) PipelineTelemetry { return w.metrics.With(tenant, pipeline) }
	onBlocked := func(tenant, reason string, blocked bool) {
		w.metrics.TenantBlocked(tenant, reason, blocked)
		if blocked {
			w.blockedMu.Lock()
			w.blocked = append(w.blocked, tenant+" "+reason)
			w.blockedMu.Unlock()
		}
	}
	a, err := BuildFromPlan(context.Background(), cfg, plan, cp, coordinate.Noop{}, ef, tf,
		Options{Secrets: w.secrets, Now: w.clock, OnTenantBlocked: onBlocked})
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

// lead runs one leadership of a, as Scheduler.Run does but with one tick per loop instead of timers:
// a new epoch; every distinct gate told of the election before any loop starts; every runner reset and
// its emit worker started; one scheduler tick (scope gate, collect, enqueue) of every loop at once,
// each awaited until its batch is emitted and saved; a flush of self-telemetry while still leader; then
// the leadership ends, the workers join, and every gate is reset.
func (w *gateWorld) lead(a *App) {
	w.t.Helper()
	w.epoch++
	ctx, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), w.epoch))
	defer cancel()
	specs := a.Specs()
	var gates []*source.ScopeGate
	for _, sp := range specs {
		if sp.Gate != nil && !slices.Contains(gates, sp.Gate) {
			gates = append(gates, sp.Gate)
		}
	}
	for _, g := range gates {
		g.Elected(ctx)
	}
	var workers sync.WaitGroup
	for _, sp := range specs {
		sp.Runner.Reset()
		if !portkey.SetLoopClockForTest(sp.Loop, w.clock) {
			w.t.Fatalf("cannot set the clock of loop %s", sp.Loop.Key().String())
		}
		workers.Add(1)
		go func() { defer workers.Done(); sp.Runner.Run(ctx) }()
	}
	now := w.clock()
	var ticks sync.WaitGroup
	errs := make(chan string, len(specs))
	for _, sp := range specs {
		ticks.Add(1)
		go func() {
			defer ticks.Done()
			a.sched.RunOnceForTest(ctx, sp, now)
			for deadline := time.Now().Add(10 * time.Second); sp.Runner.Busy(); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					errs <- "loop " + sp.Loop.Key().String() + " never finished its batch"
					return
				}
			}
		}()
	}
	ticks.Wait()
	close(errs)
	for e := range errs {
		w.t.Error(e)
	}
	w.flush()
	cancel()
	workers.Wait()
	for _, sp := range specs {
		sp.Runner.ClearDegradedGauge()
	}
	for _, g := range gates {
		g.Reset()
	}
}

// flush exports self-telemetry now, as the test must before it checks.
func (w *gateWorld) flush() {
	w.t.Helper()
	if err := w.mp.ForceFlush(context.Background()); err != nil {
		w.t.Fatalf("self-telemetry flush: %v", err)
	}
}

// checks flushes self-telemetry and runs the four checks over everything the receiver and the vendor
// saw.
func (w *gateWorld) checks() map[string][]finding {
	w.flush()
	payloads := w.recv.all()
	w.logSummary(payloads)
	return map[string][]finding{
		checkDelivery:     deliveryCheck(payloads, w.tenants, w.selfCred()),
		checkSlug:         slugCheck(w.vendor.seen(), w.tenants),
		checkSelfEndpoint: selfEndpointCheck(payloads, w.selfCred()),
		checkSelfIdentity: selfIdentityCheck(payloads, w.tenants, w.selfCred(), w.selfNamespace),
	}
}

// logSummary logs what the receiver and the vendor saw, by credential and by row, for a failure's
// reader (and -v).
func (w *gateWorld) logSummary(payloads []otlpPayload) {
	w.t.Helper()
	type tally struct {
		requests, points int
		status           map[int]int
	}
	byCred := map[string]*tally{}
	for _, p := range payloads {
		tl := byCred[p.Credential]
		if tl == nil {
			tl = &tally{status: map[int]int{}}
			byCred[p.Credential] = tl
		}
		tl.requests++
		tl.points += p.points()
		tl.status[p.Status]++
	}
	for _, c := range slices.Sorted(maps.Keys(byCred)) {
		tl := byCred[c]
		w.t.Logf("receiver: credential %s: %d requests, %d points, statuses %v", c, tl.requests, tl.points, tl.status)
	}
	byRow := map[string]map[string]int{}
	for _, r := range w.vendor.seen() {
		if byRow[r.Row] == nil {
			byRow[r.Row] = map[string]int{}
		}
		slug := "no slug"
		if r.HasSlug {
			slug = "slug " + r.Slug
		}
		byRow[r.Row][r.Path+" ("+slug+")"]++
	}
	for _, row := range slices.Sorted(maps.Keys(byRow)) {
		w.t.Logf("vendor: row %s: %v", row, byRow[row])
	}
}

// requireClean fails the test with every finding of every check.
func (w *gateWorld) requireClean() {
	w.t.Helper()
	found := w.checks()
	for _, c := range gateChecks {
		for _, f := range found[c] {
			w.t.Errorf("finding: %s", f)
		}
	}
}

// delivered is every data point received under a tenant's credential and accepted.
func (w *gateWorld) delivered(name string) []payloadPoint {
	var out []payloadPoint
	for _, p := range w.recv.under(w.tenant(name).credential()) {
		if p.Status != http.StatusOK {
			continue
		}
		for _, r := range p.Resources {
			out = append(out, r.Points...)
		}
	}
	return out
}

// requireDelivered fails unless each named tenant had at least one data point accepted.
func (w *gateWorld) requireDelivered(names ...string) {
	w.t.Helper()
	for _, n := range names {
		if len(w.delivered(n)) == 0 {
			w.t.Errorf("%s: nothing was delivered under its credential", w.tenant(n).pipeline())
		}
	}
}

// requireNothingUnder fails if any request at all reached the receiver under a named tenant's credential.
func (w *gateWorld) requireNothingUnder(names ...string) {
	w.t.Helper()
	for _, n := range names {
		if got := w.recv.under(w.tenant(n).credential()); len(got) != 0 {
			w.t.Errorf("%s: %d requests reached the receiver under its credential, want none", w.tenant(n).pipeline(), len(got))
		}
	}
}

// sawScopeState reports whether self-telemetry exported scope_state 1 for state under pipeline.
func (w *gateWorld) sawScopeState(pipeline, state string) bool {
	for _, p := range w.recv.under(w.selfCred()) {
		for _, r := range p.Resources {
			for _, pt := range r.Points {
				if pt.Metric == "genai_otel_bridge_scope_state" && pt.Attrs["pipeline"] == pipeline && pt.Attrs["state"] == state && pt.Value == 1 {
					return true
				}
			}
		}
	}
	return false
}

// blockedFor reports whether any build reported tenant blocked for reason.
func (w *gateWorld) blockedFor(tenant, reason string) bool {
	w.blockedMu.Lock()
	defer w.blockedMu.Unlock()
	return slices.Contains(w.blocked, tenant+" "+reason)
}

// sawTenantBlocked reports whether self-telemetry exported tenant_blocked 1 for tenant and reason.
func (w *gateWorld) sawTenantBlocked(tenant, reason string) bool {
	for _, p := range w.recv.under(w.selfCred()) {
		for _, r := range p.Resources {
			for _, pt := range r.Points {
				if pt.Metric == "genai_otel_bridge_tenant_blocked" && pt.Attrs["tenant"] == tenant && pt.Attrs["reason"] == reason && pt.Value == 1 {
					return true
				}
			}
		}
	}
	return false
}

// deliveredTo is every data point accepted under a pipeline's credential in a resource naming that
// pipeline, so two pipelines sharing one credential are told apart.
func (w *gateWorld) deliveredTo(g gateTenant) []payloadPoint {
	var out []payloadPoint
	for _, p := range w.recv.under(g.credential()) {
		if p.Status != http.StatusOK {
			continue
		}
		for _, r := range p.Resources {
			if r.Attrs[tenancy.PipelineAttribute] == g.pipeline() {
				out = append(out, r.Points...)
			}
		}
	}
	return out
}

// latest is the newest data point time delivered under a tenant's credential.
func (w *gateWorld) latest(name string) time.Time {
	var t time.Time
	for _, pt := range w.delivered(name) {
		if pt.Time.After(t) {
			t = pt.Time
		}
	}
	return t
}

// earliest is the oldest data point time delivered under a tenant's credential.
func (w *gateWorld) earliest(name string) time.Time {
	var t time.Time
	for _, pt := range w.delivered(name) {
		if t.IsZero() || pt.Time.Before(t) {
			t = pt.Time
		}
	}
	return t
}

// --- seeded-fault plumbing ---

// swapEmitters is a seeded fault: an emitter factory under which pipelines p and q each emit through the
// emitter built for the other. Every emitter is built from its own spec by inner; only which one a
// pipeline's runners get is swapped, resolved at emit time because the factory is called row by row.
func swapEmitters(p, q string) func(EmitterFactory) EmitterFactory {
	return func(inner EmitterFactory) EmitterFactory {
		var mu sync.Mutex
		built := map[string]emit.Emitter{}
		other := map[string]string{p: q, q: p}
		return func(spec EmitterSpec, t PipelineTelemetry) emit.Emitter {
			em := inner(spec, t)
			mu.Lock()
			built[spec.Pipeline] = em
			mu.Unlock()
			o, ok := other[spec.Pipeline]
			if !ok {
				return em
			}
			return lateEmitter(func() emit.Emitter {
				mu.Lock()
				defer mu.Unlock()
				return built[o]
			})
		}
	}
}

// lateEmitter emits through the emitter its function returns at the time of the call.
type lateEmitter func() emit.Emitter

func (l lateEmitter) Emit(ctx context.Context, b model.Batch) error { return l().Emit(ctx, b) }
