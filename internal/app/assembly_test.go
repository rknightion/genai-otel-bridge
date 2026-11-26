// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/model"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// Tests for unit 3b-2 (#524): BuildFromPlan assembles one pipeline per plan row in three passes.

var assemblyAnchor = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// assemblyPortkey is a fake Portkey analytics API: six 1-minute requests buckets before the anchor,
// whatever the window, recording the API key header of every request.
type assemblyPortkey struct {
	*httptest.Server
	mu   sync.Mutex
	keys []string
}

func newAssemblyPortkey(t *testing.T) *assemblyPortkey {
	t.Helper()
	f := &assemblyPortkey{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("x-portkey-api-key"))
		f.mu.Unlock()
		var dps []map[string]any
		for i := 9; i >= 1; i-- {
			ts := assemblyAnchor.Add(-time.Duration(i) * time.Minute)
			dps = append(dps, map[string]any{"timestamp": ts.Format(time.RFC3339), "total": float64(100 + i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": 1}, "data_points": dps, "is_quota_exceeded": false})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *assemblyPortkey) apiKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// assemblyRow is a hand-built plan row: minimalConfig's Portkey source under its own source instance,
// sent to its own destination with its own identity.
func assemblyRow(pkURL, sourceInstance, endpoint, instanceID, namespace string) tenancy.Row {
	sc := minimalConfig(pkURL).Sources[0]
	sc.SourceInstance = sourceInstance
	return tenancy.Row{
		Source:      sc,
		Destination: config.OTLPConn{Endpoint: endpoint, InstanceID: instanceID, Token: "token-" + sourceInstance},
		Identity:    config.IdentityConfig{ServiceNamespace: namespace, DeploymentEnvironment: "dev"}.ProductIdentity(),
	}
}

// tenantRow is assemblyRow for a tenant-shape pipeline: tenant, target, pipeline id and source instance
// as the plan package names them.
func tenantRow(pkURL, tenant, target, endpoint, instanceID string) tenancy.Row {
	r := assemblyRow(pkURL, tenancy.SourceInstance(tenant, target), endpoint, instanceID, tenant)
	r.Tenant, r.Target, r.Pipeline = tenant, target, tenancy.PipelineID(tenant, target)
	return r
}

func assemblyCheckpointer(t *testing.T) *file.Store {
	t.Helper()
	cp, err := file.New(filepath.Join(t.TempDir(), "wm.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// specEmitter records the spec it was built from and the key of every batch it is asked to emit.
type specEmitter struct {
	spec EmitterSpec
	mu   sync.Mutex
	keys []model.CheckpointKey
}

func (e *specEmitter) Emit(_ context.Context, b model.Batch) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keys = append(e.keys, b.Key)
	return nil
}

func (e *specEmitter) emittedKeys() []model.CheckpointKey {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]model.CheckpointKey(nil), e.keys...)
}

// specRecorder is an EmitterFactory that builds a specEmitter per call, kept in call order.
type specRecorder struct {
	mu    sync.Mutex
	built []*specEmitter
}

func (r *specRecorder) factory(spec EmitterSpec, _ PipelineTelemetry) emit.Emitter {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := &specEmitter{spec: spec}
	r.built = append(r.built, e)
	return e
}

// byEndpoint is the one emitter built for endpoint; the test fails if there is not exactly one.
func (r *specRecorder) byEndpoint(t *testing.T, endpoint string) *specEmitter {
	t.Helper()
	var found []*specEmitter
	for _, e := range r.built {
		if e.spec.Endpoint == endpoint {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d emitters built for endpoint %q, want 1", len(found), endpoint)
	}
	return found[0]
}

// cycleOnce runs one Since, Collect, ProcessBatch on a spec with the loop clock pinned to the anchor.
func cycleOnce(t *testing.T, sp schedule.LoopSpec) {
	t.Helper()
	if !portkey.SetLoopClockForTest(sp.Loop, func() time.Time { return assemblyAnchor }) {
		t.Fatal("failed to inject clock into portkey loop")
	}
	lc := coordinate.WithEpoch(context.Background(), 1)
	wm, err := sp.Runner.Since(lc)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	b, err := sp.Loop.Collect(lc, wm)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	sp.Runner.ProcessBatch(lc, b)
}

// TestPassThreePairsRowsWithTheirEmitter (AC1): each runner emits through the emitter built from its own
// row, which carries that row's endpoint, credentials and identity. Two rows on two stacks, one tick
// each: every batch a row's emitter receives comes from that row's source.
func TestPassThreePairsRowsWithTheirEmitter(t *testing.T) {
	pk := newAssemblyPortkey(t)
	rows := []tenancy.Row{
		assemblyRow(pk.URL, "pk-a", "https://stack-a.example", "100", "team-a"),
		assemblyRow(pk.URL, "pk-b", "https://stack-b.example", "200", "team-b"),
	}
	rec := &specRecorder{}
	a, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: rows}, assemblyCheckpointer(t), coordinate.Noop{}, rec.factory, noopTelemetryFactory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.built) != len(rows) {
		t.Fatalf("built %d emitters for %d rows, want one per row", len(rec.built), len(rows))
	}
	for _, sp := range a.Specs() {
		cycleOnce(t, sp)
	}
	for _, row := range rows {
		e := rec.byEndpoint(t, row.Destination.Endpoint)
		if e.spec.InstanceID != row.Destination.InstanceID || e.spec.Token != row.Destination.Token {
			t.Errorf("emitter for %s has credentials (%q, %q), want the row's (%q, %q)", row.Source.SourceInstance, e.spec.InstanceID, e.spec.Token, row.Destination.InstanceID, row.Destination.Token)
		}
		if !maps.Equal(e.spec.Identity, row.Identity) {
			t.Errorf("emitter for %s has identity %v, want the row's %v", row.Source.SourceInstance, e.spec.Identity, row.Identity)
		}
		keys := e.emittedKeys()
		if len(keys) == 0 {
			t.Errorf("the emitter for %s emitted nothing; its row's loop never reached it", row.Source.SourceInstance)
		}
		for _, k := range keys {
			if k.SourceInstance != row.Source.SourceInstance {
				t.Errorf("the emitter for %s (endpoint %s) emitted a batch of %s", row.Source.SourceInstance, row.Destination.Endpoint, k.String())
			}
		}
	}
}

// TestOwnershipIsPerGroup (AC2): the series-ownership check runs per group of rows sharing a stack and an
// identity. Two rows declaring one metric name may sit on different stacks, or on one stack under
// different identities; on one stack with one identity they collide, whatever the endpoint's case or
// trailing slash.
func TestOwnershipIsPerGroup(t *testing.T) {
	pk := newAssemblyPortkey(t)
	build := func(rows ...tenancy.Row) error {
		_, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: rows}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory, Options{})
		return err
	}
	t.Run("different stacks", func(t *testing.T) {
		if err := build(
			assemblyRow(pk.URL, "pk-a", "https://stack-a.example", "100", "team"),
			assemblyRow(pk.URL, "pk-b", "https://stack-b.example", "100", "team"),
		); err != nil {
			t.Fatalf("two stacks may both carry portkey_api_requests: %v", err)
		}
	})
	t.Run("one stack, different identities", func(t *testing.T) {
		if err := build(
			assemblyRow(pk.URL, "pk-a", "https://stack.example", "100", "team-a"),
			assemblyRow(pk.URL, "pk-b", "https://stack.example", "100", "team-b"),
		); err != nil {
			t.Fatalf("two identities on one stack write distinct series: %v", err)
		}
	})
	t.Run("one stack and identity", func(t *testing.T) {
		err := build(
			assemblyRow(pk.URL, "pk-a", "https://Stack.example/", "100", "team"),
			assemblyRow(pk.URL, "pk-b", "https://stack.example", "100", "team"),
		)
		if err == nil || !strings.Contains(err.Error(), "owned by both") {
			t.Fatalf("two writers of one series on one stack and identity must fail ownership, got %v", err)
		}
	})
}

// TestPipelineAttributeOnlyWithPipelineID (AC3): a row with a pipeline id adds genai_otel_bridge.pipeline
// to its emitter's identity, and its spec and runner carry the pipeline; a row without one gets exactly
// its own identity. The wire check runs both rows through the production OTLP emitter.
func TestPipelineAttributeOnlyWithPipelineID(t *testing.T) {
	pk := newAssemblyPortkey(t)
	withID := tenantRow(pk.URL, "tenant-a", "primary", "https://stack-a.example", "100")
	without := assemblyRow(pk.URL, "pk-legacy", "https://stack-b.example", "200", "team-b")
	rows := []tenancy.Row{withID, without}

	rec := &specRecorder{}
	a, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: rows}, assemblyCheckpointer(t), coordinate.Noop{}, rec.factory, noopTelemetryFactory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := maps.Clone(withID.Identity)
	want[tenancy.PipelineAttribute] = "tenant-a.primary"
	e := rec.byEndpoint(t, withID.Destination.Endpoint)
	if !maps.Equal(e.spec.Identity, want) || e.spec.Tenant != "tenant-a" || e.spec.Pipeline != "tenant-a.primary" {
		t.Errorf("pipeline row's emitter spec = identity %v, tenant %q, pipeline %q; want %v, tenant-a, tenant-a.primary", e.spec.Identity, e.spec.Tenant, e.spec.Pipeline, want)
	}
	if _, ok := withID.Identity[tenancy.PipelineAttribute]; ok {
		t.Error("assembly wrote the pipeline attribute into the plan row's own identity map")
	}
	e = rec.byEndpoint(t, without.Destination.Endpoint)
	if !maps.Equal(e.spec.Identity, without.Identity) || e.spec.Tenant != "" || e.spec.Pipeline != "" {
		t.Errorf("row without a pipeline id: emitter spec = identity %v, tenant %q, pipeline %q; want exactly %v and no tenant or pipeline", e.spec.Identity, e.spec.Tenant, e.spec.Pipeline, without.Identity)
	}
	for _, sp := range a.Specs() {
		wantPL := schedule.Pipeline{}
		if sp.Loop.Key().SourceInstance == withID.Source.SourceInstance {
			wantPL = schedule.Pipeline{Tenant: "tenant-a", ID: "tenant-a.primary"}
		}
		if sp.Pipeline != wantPL {
			t.Errorf("spec %s carries pipeline %+v, want %+v", sp.Loop.Key().String(), sp.Pipeline, wantPL)
		}
	}

	// On the wire: the same two rows, each to its own receiver, through OTLPEmitterFactory.
	recvA, recvB := newBodyReceiver(t), newBodyReceiver(t)
	withID.Destination.Endpoint, without.Destination.Endpoint = recvA.URL, recvB.URL
	ef := OTLPEmitterFactory(1<<20, fastRetry(), logging.NewLimiter(time.Minute))
	a, err = BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: []tenancy.Row{withID, without}}, assemblyCheckpointer(t), coordinate.Noop{}, ef, noopTelemetryFactory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range a.Specs() {
		cycleOnce(t, sp)
	}
	bodyA, bodyB := recvA.joined(t), recvB.joined(t)
	if !bytes.Contains(bodyA, []byte(tenancy.PipelineAttribute)) || !bytes.Contains(bodyA, []byte("tenant-a.primary")) {
		t.Error("the pipeline row's OTLP body does not carry genai_otel_bridge.pipeline=tenant-a.primary")
	}
	if bytes.Contains(bodyB, []byte(tenancy.PipelineAttribute)) {
		t.Error("a row without a pipeline id sent genai_otel_bridge.pipeline on the wire")
	}
}

// bodyReceiver is a fake OTLP endpoint keeping every decompressed request body.
type bodyReceiver struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newBodyReceiver(t *testing.T) *bodyReceiver {
	t.Helper()
	r := &bodyReceiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		zr, err := gzip.NewReader(req.Body)
		if err == nil {
			body, err = io.ReadAll(zr)
		}
		if err != nil {
			t.Errorf("receiver: %v", err)
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *bodyReceiver) joined(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		t.Fatalf("receiver %s got no request", r.URL)
	}
	return bytes.Join(r.bodies, nil)
}

// electTwice is a coordinator that elects this replica twice, each time with an already-ended
// leadership, then returns.
type electTwice struct{}

func (electTwice) Run(ctx context.Context, onElected func(context.Context)) error {
	for i := range 2 {
		lc, cancel := context.WithCancel(coordinate.WithEpoch(ctx, int64(i+1)))
		cancel()
		onElected(lc)
	}
	return nil
}

// TestRunLogsPlanHashOnElection (AC4): Run logs "leader elected" with the plan's hash and row count each
// time the coordinator elects the replica: once under coordinate.Noop, twice under a coordinator that
// elects twice.
func TestRunLogsPlanHashOnElection(t *testing.T) {
	pk := newAssemblyPortkey(t)
	cfg := minimalConfig(pk.URL)
	plan, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	elections := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			var rec map[string]any
			if json.Unmarshal(line, &rec) == nil && rec["msg"] == "leader elected" {
				out = append(out, rec)
			}
		}
		buf.Reset()
		return out
	}
	check := func(t *testing.T, got []map[string]any, n int) {
		t.Helper()
		if len(got) != n {
			t.Fatalf("logged %d \"leader elected\" lines, want %d", len(got), n)
		}
		for _, rec := range got {
			if rec["plan_hash"] != plan.Hash() || rec["plan_rows"] != float64(len(plan.Rows)) {
				t.Errorf("leader line = plan_hash %v plan_rows %v, want %s and %d", rec["plan_hash"], rec["plan_rows"], plan.Hash(), len(plan.Rows))
			}
		}
	}
	run := func(t *testing.T, coord coordinate.Coordinator, elected chan<- struct{}) (*App, context.CancelFunc, <-chan error) {
		t.Helper()
		ef, tf := fixedFactories(noopEmitter{}, schedule.NoopMetrics{})
		a, err := Build(context.Background(), cfg, assemblyCheckpointer(t), coord, ef, tf, Options{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- a.Run(ctx, http.NotFoundHandler(), "127.0.0.1:0", func() {}, func() {}, func(leader bool) {
				if leader && elected != nil {
					elected <- struct{}{}
				}
			})
		}()
		return a, cancel, done
	}

	t.Run("noop coordinator", func(t *testing.T) {
		elected := make(chan struct{}, 1)
		_, cancel, done := run(t, coordinate.Noop{}, elected)
		select {
		case <-elected:
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatal("never elected")
		}
		cancel()
		<-done
		check(t, elections(), 1)
	})
	t.Run("elected twice", func(t *testing.T) {
		_, cancel, done := run(t, electTwice{}, nil)
		defer cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return")
		}
		check(t, elections(), 2)
	})
}

// mapSecrets is a SecretReader over a map keyed by tenant and file; a missing entry, or one in fail, is
// a read error naming tenant and file only.
type mapSecrets struct {
	values map[[2]string]string
	fail   map[[2]string]bool
	mu     sync.Mutex
	asked  [][2]string
}

func (m *mapSecrets) Read(tenant, file string) (string, error) {
	m.mu.Lock()
	m.asked = append(m.asked, [2]string{tenant, file})
	m.mu.Unlock()
	k := [2]string{tenant, file}
	if v, ok := m.values[k]; ok && !m.fail[k] {
		return v, nil
	}
	return "", fmt.Errorf("tenant %s: secret %s: %w", tenant, file, errors.New("permission denied"))
}

// TestBuildFromPlanReadsRowSecrets (AC5): a row naming secret files is assembled with the values read
// through Options.Secrets: the emitter factory receives the token read, and the source sends the API key
// read. The plan's own row keeps no value.
func TestBuildFromPlanReadsRowSecrets(t *testing.T) {
	pk := newAssemblyPortkey(t)
	row := tenantRow(pk.URL, "tenant-a", "primary", "https://stack-a.example", "100")
	row.AuthValueFile, row.TokenFile = "api-key", "otlp-token"
	row.Source.Auth.Value, row.Destination.Token = "", ""
	plan := tenancy.Plan{Rows: []tenancy.Row{row}}
	secrets := &mapSecrets{values: map[[2]string]string{
		{"tenant-a", "api-key"}:    "api-key-value-read-from-file",
		{"tenant-a", "otlp-token"}: "otlp-token-value-read-from-file",
	}}
	rec := &specRecorder{}
	a, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), plan, assemblyCheckpointer(t), coordinate.Noop{}, rec.factory, noopTelemetryFactory, Options{Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.built) != 1 || rec.built[0].spec.Token != "otlp-token-value-read-from-file" {
		t.Fatalf("the emitter factory must receive the token read from otlp-token, got %+v", rec.built)
	}
	for _, sp := range a.Specs() {
		cycleOnce(t, sp)
	}
	keys := pk.apiKeys()
	if len(keys) == 0 {
		t.Fatal("the source sent no request")
	}
	for i, k := range keys {
		if k != "api-key-value-read-from-file" {
			t.Errorf("request %d sent API key %q, want the value read from api-key", i, k)
		}
	}
	if plan.Rows[0].Source.Auth.Value != "" || plan.Rows[0].Destination.Token != "" {
		t.Error("assembly wrote a secret value into the plan's own row")
	}
}

// TestBuildFromPlanSecretErrorFails (AC5): a read error, or no reader for a row that names a file, fails
// BuildFromPlan with the tenant and the file named and no secret value in the message.
func TestBuildFromPlanSecretErrorFails(t *testing.T) {
	pk := newAssemblyPortkey(t)
	row := tenantRow(pk.URL, "tenant-a", "primary", "https://stack-a.example", "100")
	row.AuthValueFile, row.TokenFile = "api-key", "otlp-token"
	row.Source.Auth.Value, row.Destination.Token = "", ""
	build := func(opts Options) error {
		_, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: []tenancy.Row{row}}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory, opts)
		return err
	}
	const authValue, tokenValue = "api-key-value-must-not-leak", "otlp-token-value-must-not-leak"
	values := map[[2]string]string{{"tenant-a", "api-key"}: authValue, {"tenant-a", "otlp-token"}: tokenValue}
	cases := []struct {
		name, file string
		opts       Options
	}{
		{"read error", "otlp-token", Options{Secrets: &mapSecrets{values: values, fail: map[[2]string]bool{{"tenant-a", "otlp-token"}: true}}}},
		{"nil reader", "api-key", Options{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := build(c.opts)
			if err == nil {
				t.Fatal("BuildFromPlan must fail")
			}
			msg := err.Error()
			if !strings.Contains(msg, "tenant-a") || !strings.Contains(msg, c.file) {
				t.Errorf("error %q must name tenant-a and %s", msg, c.file)
			}
			if strings.Contains(msg, authValue) || strings.Contains(msg, tokenValue) {
				t.Errorf("error %q carries a secret value", msg)
			}
		})
	}
}

// seedTelemetry records the loops SeedLoop is called for.
type seedTelemetry struct {
	noopTelemetry
	mu  sync.Mutex
	ids []schedule.LoopID
}

func (s *seedTelemetry) SeedLoop(id schedule.LoopID, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, id)
}

// TestBuildFromPlanSeedsEachRow (AC6): each row's telemetry is seeded for that row's own loops only.
func TestBuildFromPlanSeedsEachRow(t *testing.T) {
	pk := newAssemblyPortkey(t)
	rows := []tenancy.Row{
		tenantRow(pk.URL, "tenant-a", "primary", "https://stack-a.example", "100"),
		tenantRow(pk.URL, "tenant-b", "primary", "https://stack-b.example", "200"),
	}
	var mu sync.Mutex
	tel := map[string]*seedTelemetry{}
	tf := func(_, pipeline string) PipelineTelemetry {
		mu.Lock()
		defer mu.Unlock()
		if tel[pipeline] == nil {
			tel[pipeline] = &seedTelemetry{}
		}
		return tel[pipeline]
	}
	a, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: rows}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, tf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		var want []schedule.LoopID
		for _, sp := range a.Specs() {
			if sp.Loop.Key().SourceInstance == row.Source.SourceInstance {
				want = append(want, schedule.MetricsLoopID(sp.Loop))
			}
		}
		s := tel[row.Pipeline]
		if s == nil {
			t.Fatalf("no telemetry was asked for pipeline %s", row.Pipeline)
		}
		if len(want) == 0 || fmt.Sprint(s.ids) != fmt.Sprint(want) {
			t.Errorf("pipeline %s seeded %v, want its own loops %v", row.Pipeline, s.ids, want)
		}
	}
}

// Tests for unit 4b (#526): assembly gives every row with a scope one scope gate, and refuses a scoped
// row whose source cannot assert its scope and any loop named scope-match.

// scopedPortkey is a fake Portkey for scoped rows: the analytics graphs as newAssemblyPortkey serves
// them, an empty page for every groups endpoint, and a workspace grouping whose only workspace is the
// one its API key belongs to (workspaces). Each workspace-grouping request's key and slug are recorded.
type scopedPortkey struct {
	*httptest.Server
	workspaces map[string]string
	mu         sync.Mutex
	asserts    [][2]string
}

func newScopedPortkey(t *testing.T, workspaces map[string]string) *scopedPortkey {
	t.Helper()
	f := &scopedPortkey{workspaces: workspaces}
	graphs := newAssemblyPortkey(t)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-portkey-api-key")
		switch {
		case strings.HasSuffix(r.URL.Path, "/analytics/groups/workspace"):
			f.mu.Lock()
			f.asserts = append(f.asserts, [2]string{key, r.URL.Query().Get("workspace_slug")})
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "total": 1, "is_quota_exceeded": false,
				"data": []map[string]any{{"workspace": f.workspaces[key], "requests": 1}}})
		case strings.Contains(r.URL.Path, "/analytics/groups/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "total": 0, "data": []any{}})
		default:
			graphs.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *scopedPortkey) assertions() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.asserts...)
}

// scopedRow is tenantRow with a scope, its own API key and the groups loop beside analytics, so the
// row has two loops to share one gate.
func scopedRow(pkURL, tenant, target, endpoint, instanceID, key string, scope config.ScopeConfig) tenancy.Row {
	r := tenantRow(pkURL, tenant, target, endpoint, instanceID)
	r.Source.Auth.Value = key
	r.Source.Scope = scope
	r.Source.Loops["groups"] = config.LoopConfig{Enabled: true, Cadence: config.Duration(5 * time.Minute), MetricPrefix: "portkey_api",
		Settings: map[string]string{"page_size": "100"}}
	return r
}

// scopeTelemetry records the two scope signals a gate reports through its row's telemetry.
type scopeTelemetry struct {
	noopTelemetry
	mu      sync.Mutex
	states  []string
	results []string
}

func (s *scopeTelemetry) ScopeState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, state)
}

func (s *scopeTelemetry) ScopeAssertion(result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, result)
}

func (s *scopeTelemetry) seen() (states, results []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.states...), append([]string(nil), s.results...)
}

// gatesBySource is every spec's gate, grouped by the spec's source instance.
func gatesBySource(a *App) map[string][]*source.ScopeGate {
	out := map[string][]*source.ScopeGate{}
	for _, sp := range a.Specs() {
		k := sp.Loop.Key().SourceInstance
		out[k] = append(out[k], sp.Gate)
	}
	return out
}

// TestScopedRowWithoutCapabilityFails (AC6): a row with a scope whose source cannot assert it (the
// langsmith source implements no source.ScopeAsserter) fails assembly, naming the row; the same row with
// no scope builds. Fail-closed: a scope that cannot be checked is never collected unchecked.
func TestScopedRowWithoutCapabilityFails(t *testing.T) {
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	t.Cleanup(ls.Close)
	row := tenantRow(ls.URL, "tenant-a", "primary", "https://stack-a.example", "100")
	row.Source = config.SourceConfig{
		Type: "langsmith", Enabled: true, BaseURL: ls.URL, SourceInstance: tenancy.SourceInstance("tenant-a", "primary"),
		Auth:      config.AuthConfig{Header: "x-api-key", Value: "k"},
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		HTTP:      config.HTTPConfig{AllowPrivate: true},
		Loops: map[string]config.LoopConfig{"sessions": {
			Enabled: true, Cadence: config.Duration(time.Minute), MetricPrefix: "langsmith",
		}},
	}
	build := func(r tenancy.Row) error {
		_, err := BuildFromPlan(context.Background(), minimalConfig(ls.URL), tenancy.Plan{Rows: []tenancy.Row{r}}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory, Options{})
		return err
	}
	if err := build(row); err != nil {
		t.Fatalf("the row with no scope must build: %v", err)
	}
	for _, scope := range []config.ScopeConfig{{Name: "ws-a", ID: "ws-id-a"}, {ID: "ws-id-a"}} {
		row.Source.Scope = scope
		err := build(row)
		if err == nil || !strings.Contains(err.Error(), "scope") || !strings.Contains(err.Error(), row.Source.SourceInstance) {
			t.Fatalf("scope %+v on a source that cannot assert it: want an error naming the scope and %s, got %v", scope, row.Source.SourceInstance, err)
		}
	}
}

// reservedSource is a fake source with one loop named loopName; it can assert its scope (always
// Matched), so only the loop name decides whether a row of it assembles.
type reservedSource struct{ loopName string }

func (s reservedSource) ID() string           { return "reserved" }
func (s reservedSource) Loops() []source.Loop { return []source.Loop{reservedLoop(s)} }
func (s reservedSource) AssertScope(context.Context) (source.ScopeResult, error) {
	return source.ScopeMatched, nil
}

type reservedLoop struct{ loopName string }

func (l reservedLoop) Key() model.CheckpointKey {
	return model.CheckpointKey{SourceInstance: "fake-src", Loop: l.loopName, OutputFingerprint: "f"}
}
func (l reservedLoop) Cadence() time.Duration { return time.Minute }
func (l reservedLoop) SeriesNames() []string  { return []string{} }
func (l reservedLoop) Collect(_ context.Context, since model.Watermark) (model.Batch, error) {
	return model.Batch{Key: l.Key(), Watermark: since}, nil
}

// TestScopeMatchLoopNameFails (AC6): scope-match is the reserved loop name of a pipeline's remembered
// match (design §7). Assembly refuses a source that builds a loop of that name, in a row with a scope
// and in a row without one, so no loop's watermark can overwrite or be read as the remembered match. The
// same source with another loop name builds.
func TestScopeMatchLoopNameFails(t *testing.T) {
	testSourceTypes = func(reg *source.Registry) {
		reg.Register("reserved-scope-match", func(config.SourceConfig, source.Deps) (source.Source, error) {
			return reservedSource{loopName: source.ScopeMatchLoop}, nil
		})
		reg.Register("reserved-other", func(config.SourceConfig, source.Deps) (source.Source, error) {
			return reservedSource{loopName: "other"}, nil
		})
	}
	t.Cleanup(func() { testSourceTypes = nil })
	row := func(typ, loop string, scope config.ScopeConfig) tenancy.Row {
		r := tenantRow("https://vendor.example", "tenant-a", "primary", "https://stack-a.example", "100")
		r.Source = config.SourceConfig{Type: typ, Enabled: true, SourceInstance: "fake-src", Scope: scope,
			Loops: map[string]config.LoopConfig{loop: {Enabled: true, Cadence: config.Duration(time.Minute)}}}
		return r
	}
	build := func(r tenancy.Row) error {
		_, err := BuildFromPlan(context.Background(), minimalConfig("https://vendor.example"), tenancy.Plan{Rows: []tenancy.Row{r}}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory, Options{})
		return err
	}
	scope := config.ScopeConfig{Name: "ws-a", ID: "ws-id-a"}
	for _, sc := range []config.ScopeConfig{{}, scope} {
		if err := build(row("reserved-scope-match", source.ScopeMatchLoop, sc)); err == nil || !strings.Contains(err.Error(), source.ScopeMatchLoop+" is a reserved loop name") {
			t.Errorf("scope %+v: a loop named %s must fail assembly as a reserved name, got %v", sc, source.ScopeMatchLoop, err)
		}
		if err := build(row("reserved-other", "other", sc)); err != nil {
			t.Errorf("scope %+v: the same source with another loop name must build: %v", sc, err)
		}
	}
}

// TestScopedRowSharesOneGate (AC6): every loop of a scoped row carries the same gate, and two rows have
// two gates. The gate is wired to its own row: it asserts with that row's key (read from its secret
// file) and slug, writes the remembered match to the checkpoint store under the row's scope-match key
// with the digest of that key's fingerprint, scope name and scope id, at the time of Options.Now, and
// reports through that row's telemetry only.
func TestScopedRowSharesOneGate(t *testing.T) {
	const keyA, keyB = "api-key-a-read-from-file", "api-key-b"
	pk := newScopedPortkey(t, map[string]string{keyA: "ws-a", keyB: "ws-someone-else"})
	scopeA := config.ScopeConfig{Name: "ws-a", ID: "ws-id-a"}
	scopeB := config.ScopeConfig{Name: "ws-b", ID: "ws-id-b"}
	rowA := scopedRow(pk.URL, "tenant-a", "primary", "https://stack-a.example", "100", "", scopeA)
	rowA.AuthValueFile = "api-key"
	rowB := scopedRow(pk.URL, "tenant-b", "primary", "https://stack-b.example", "200", keyB, scopeB)
	tel := map[string]*scopeTelemetry{}
	var mu sync.Mutex
	tf := func(_, pipeline string) PipelineTelemetry {
		mu.Lock()
		defer mu.Unlock()
		if tel[pipeline] == nil {
			tel[pipeline] = &scopeTelemetry{}
		}
		return tel[pipeline]
	}
	at := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	cp := assemblyCheckpointer(t)
	a, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: []tenancy.Row{rowA, rowB}}, cp, coordinate.Noop{}, (&specRecorder{}).factory, tf,
		Options{Secrets: &mapSecrets{values: map[[2]string]string{{"tenant-a", "api-key"}: keyA}}, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	if got := pk.assertions(); len(got) != 0 {
		t.Fatalf("assembly called the vendor: %v", got)
	}
	gates := gatesBySource(a)
	one := func(si string) *source.ScopeGate {
		t.Helper()
		gs := gates[si]
		if len(gs) != 2 {
			t.Fatalf("%s has %d specs, want its two loops", si, len(gs))
		}
		if gs[0] == nil || gs[0] != gs[1] {
			t.Fatalf("%s: the loops' gates are %p and %p, want one shared gate", si, gs[0], gs[1])
		}
		return gs[0]
	}
	gateA, gateB := one(rowA.Source.SourceInstance), one(rowB.Source.SourceInstance)
	if gateA == gateB {
		t.Fatal("two scoped rows share one gate")
	}

	ctx := coordinate.WithEpoch(context.Background(), 1)
	if d, err := gateA.Before(ctx); err != nil || d != source.ScopeProceed {
		t.Fatalf("row A's key reads ws-a, its scope: want proceed, got %d, %v", d, err)
	}
	if got := pk.assertions(); len(got) != 1 || got[0] != [2]string{keyA, "ws-a"} {
		t.Fatalf("row A's assertion sent (key, slug) %v, want [(the key read from its file, ws-a)]", got)
	}
	wm, err := cp.Load(ctx, source.ScopeMatchKey(rowA.Source.SourceInstance))
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := source.ScopeMatchDigest(source.KeyFingerprint(keyA), scopeA.Name, scopeA.ID)
	if wm.Cursor != wantDigest || !wm.Time.Equal(at) {
		t.Fatalf("remembered match = (%q, %s), want (%q, %s): the gate's store, digest or clock is not the row's", wm.Cursor, wm.Time, wantDigest, at)
	}
	states, results := tel[rowA.Pipeline].seen()
	if fmt.Sprint(states) != "[matched]" || fmt.Sprint(results) != "[matched]" {
		t.Fatalf("row A's telemetry saw states %v and results %v, want [matched] and [matched]", states, results)
	}
	if states, results := tel[rowB.Pipeline].seen(); len(states)+len(results) != 0 {
		t.Fatalf("row A's assertion reported through row B's telemetry: %v %v", states, results)
	}

	if d, err := gateB.Before(ctx); err != nil || d != source.ScopeMismatched {
		t.Fatalf("row B's key reads another workspace: want mismatched, got %d, %v", d, err)
	}
	if got := pk.assertions(); len(got) != 2 || got[1] != [2]string{keyB, "ws-b"} {
		t.Fatalf("row B's assertion sent (key, slug) %v, want (its own key, ws-b) second", got)
	}
	if _, results := tel[rowB.Pipeline].seen(); fmt.Sprint(results) != "[mismatch]" {
		t.Fatalf("row B's telemetry saw results %v, want [mismatch]", results)
	}
}

// TestUnscopedRowHasNoGate (AC6): a legacy row and a tenant row with no scope get no gate on any spec,
// beside a scoped row that does; a legacy config built through Build has no gate at all. A row with no
// scope is collected exactly as before.
func TestUnscopedRowHasNoGate(t *testing.T) {
	pk := newScopedPortkey(t, map[string]string{"k-scoped": "ws-a"})
	legacy := assemblyRow(pk.URL, "pk-legacy", "https://stack-l.example", "300", "team-l")
	unscoped := tenantRow(pk.URL, "tenant-u", "primary", "https://stack-u.example", "400")
	scoped := scopedRow(pk.URL, "tenant-s", "primary", "https://stack-s.example", "500", "k-scoped", config.ScopeConfig{Name: "ws-a", ID: "ws-id-a"})
	a, err := BuildFromPlan(context.Background(), minimalConfig(pk.URL), tenancy.Plan{Rows: []tenancy.Row{legacy, unscoped, scoped}}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	gates := gatesBySource(a)
	for _, r := range []tenancy.Row{legacy, unscoped} {
		gs := gates[r.Source.SourceInstance]
		if len(gs) == 0 {
			t.Fatalf("no spec for %s", r.Source.SourceInstance)
		}
		for _, g := range gs {
			if g != nil {
				t.Errorf("%s has no scope but its spec carries a gate", r.Source.SourceInstance)
			}
		}
	}
	for _, g := range gates[scoped.Source.SourceInstance] {
		if g == nil {
			t.Errorf("the scoped row %s has a spec with no gate", scoped.Source.SourceInstance)
		}
	}

	ef, tf := fixedFactories(noopEmitter{}, schedule.NoopMetrics{})
	b, err := Build(context.Background(), minimalConfig(pk.URL), assemblyCheckpointer(t), coordinate.Noop{}, ef, tf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range b.Specs() {
		if sp.Gate != nil {
			t.Errorf("legacy spec %s carries a gate", sp.Loop.Key().String())
		}
	}
	if got := pk.assertions(); len(got) != 0 {
		t.Fatalf("assembly called the vendor: %v", got)
	}
}

// scopeSeedTelemetry counts SeedScope calls.
type scopeSeedTelemetry struct {
	noopTelemetry
	mu    sync.Mutex
	seeds int
}

func (s *scopeSeedTelemetry) SeedScope() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeds++
}

// TestScopeAssertionSeededOnlyForGatedRows: a row with a scope gate has its telemetry seeded with the
// scope-assertion series (so increase() sees the first result after a run of errors); a row with no
// scope seeds nothing, which keeps a legacy install's series unchanged.
func TestScopeAssertionSeededOnlyForGatedRows(t *testing.T) {
	testSourceTypes = func(reg *source.Registry) {
		reg.Register("reserved-other", func(config.SourceConfig, source.Deps) (source.Source, error) {
			return reservedSource{loopName: "other"}, nil
		})
	}
	t.Cleanup(func() { testSourceTypes = nil })
	for _, tc := range []struct {
		name  string
		scope config.ScopeConfig
		want  int
	}{{"scoped", config.ScopeConfig{Name: "ws-a", ID: "ws-id-a"}, 1}, {"legacy", config.ScopeConfig{}, 0}} {
		r := tenantRow("https://vendor.example", "tenant-a", "primary", "https://stack-a.example", "100")
		r.Source = config.SourceConfig{Type: "reserved-other", Enabled: true, SourceInstance: "fake-src", Scope: tc.scope,
			Loops: map[string]config.LoopConfig{"other": {Enabled: true, Cadence: config.Duration(time.Minute)}}}
		tel := &scopeSeedTelemetry{}
		tf := func(_, _ string) PipelineTelemetry { return tel }
		if _, err := BuildFromPlan(context.Background(), minimalConfig("https://vendor.example"), tenancy.Plan{Rows: []tenancy.Row{r}}, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, tf, Options{}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tel.seeds != tc.want {
			t.Errorf("%s: SeedScope called %d times, want %d", tc.name, tel.seeds, tc.want)
		}
	}
}
