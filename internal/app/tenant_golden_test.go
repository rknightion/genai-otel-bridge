// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/docs/signal"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/loopid"
	"github.com/rknightion/genai-otel-bridge/internal/selfobs"
	"github.com/rknightion/genai-otel-bridge/internal/source"
	"github.com/rknightion/genai-otel-bridge/internal/tenancy"
)

// The G8 goldens of the tenant shape (unit 6b-2, #532; design G8): the formats that reach durable state
// or a tenant's stack and must not change after the first tenant-shape version. Each is a literal, taken
// from config/testdata/tenants.yaml through config.Load and tenancy.Resolve: the pipeline id, the checkpoint
// keys (their ConfigMap data keys are pinned in checkpoint/configmap's datakey_test.go), the OTLP bytes
// carrying the pipeline attribute (testdata/tenant-payload.bin) and the label set of every self-metric a
// pipeline records. A change to any of them is a migration, not a test update.

var updateTenantPayload = flag.Bool("update-tenant-payload", false, "rewrite testdata/tenant-payload.bin from the current composition root")

const tenantPayloadGolden = "testdata/tenant-payload.bin"

// G8 literals.
const (
	goldenPipeline      = "team-a.prod"
	goldenAnalyticsKey  = "mt.team-a.prod/analytics/a2e79cb406df867d"
	goldenGroupsKey     = "mt.team-a.prod/groups/1a0c9dff442c8892"
	goldenScopeMatchKey = "mt.team-a.prod/scope-match/v1"
)

// fixtureSecrets is a value for every secret file tenants.yaml names, distinct per tenant and file.
func fixtureSecrets() *mapSecrets {
	return &mapSecrets{values: map[[2]string]string{
		{"team-a", "otlp-token"}: "fixture-token-team-a", {"team-a", "prod-key"}: "fixture-key-team-a-prod",
		{"team-a", "staging-key"}: "fixture-key-team-a-staging",
		{"team-b", "otlp-token"}:  "fixture-token-team-b", {"team-b", "source-key"}: "fixture-key-team-b-prod",
	}}
}

// loadTenantFixture loads a tenant fixture through config.Load and resolves it.
func loadTenantFixture(t *testing.T, path string) (*config.Config, tenancy.Plan) {
	t.Helper()
	t.Setenv("TENANT_FIXTURE_SELF_TOKEN", "fixture-self-token")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := tenancy.Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.TenantShape {
		t.Fatalf("%s did not resolve to the tenant shape", path)
	}
	return cfg, plan
}

// fixtureRow is the plan row of one pipeline.
func fixtureRow(t *testing.T, plan tenancy.Plan, pipeline string) tenancy.Row {
	t.Helper()
	for _, r := range plan.Rows {
		if r.Pipeline == pipeline {
			return r
		}
	}
	t.Fatalf("no plan row for pipeline %q", pipeline)
	return tenancy.Row{}
}

// TestTenantGoldenPipelineAndCheckpointKeys (AC6): the fixture's team-a target prod is pipeline
// team-a.prod, and its loops and its remembered scope match keep their checkpoint keys.
func TestTenantGoldenPipelineAndCheckpointKeys(t *testing.T) {
	cfg, plan := loadTenantFixture(t, "../config/testdata/tenants.yaml")
	row := fixtureRow(t, plan, goldenPipeline)
	if row.Pipeline != goldenPipeline || tenancy.PipelineID("team-a", "prod") != goldenPipeline {
		t.Errorf("pipeline id %q, want %q", row.Pipeline, goldenPipeline)
	}
	a, err := BuildFromPlan(context.Background(), cfg, plan, assemblyCheckpointer(t), coordinate.Noop{}, (&specRecorder{}).factory, noopTelemetryFactory, Options{Secrets: fixtureSecrets()})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, sp := range a.Specs() {
		if sp.Pipeline.ID == goldenPipeline {
			keys = append(keys, sp.Loop.Key().String())
		}
	}
	sort.Strings(keys)
	if want := []string{goldenAnalyticsKey, goldenGroupsKey}; !slices.Equal(keys, want) {
		t.Errorf("checkpoint keys of %s = %q, want %q", goldenPipeline, keys, want)
	}
	if got := source.ScopeMatchKey(row.Source.SourceInstance).String(); got != goldenScopeMatchKey {
		t.Errorf("scope-match key = %q, want %q", got, goldenScopeMatchKey)
	}
}

// goldenVendor is a fake Portkey analytics API for the payload golden: twenty one-minute buckets before
// assemblyAnchor for every graph, each with fixed count, token and latency fields, so every graph of the
// fixture's analytics loop emits and buckets older than its ten-minute settle exist.
func goldenVendor(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/analytics/graphs/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var dps []map[string]any
		for i := 20; i >= 1; i-- {
			ts := assemblyAnchor.Add(-time.Duration(i) * time.Minute)
			dps = append(dps, map[string]any{"timestamp": ts.Format(time.RFC3339), "total": float64(100 + i),
				"total_request_units": float64(60 + i), "total_response_units": float64(40), "avg": 1500.0, "p50": 1000.0, "p90": 2000.0, "p99": 3000.0})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": map[string]any{"total": 1}, "data_points": dps, "is_quota_exceeded": false})
	}))
	t.Cleanup(s.Close)
	return s
}

// TestTenantPayloadGolden (AC6): the OTLP bytes the fixture's pipeline team-a.prod sends to its
// destination, through BuildFromPlan and the production OTLPEmitterFactory, one analytics cycle at the
// anchor. Each resource carries genai_otel_bridge.pipeline=team-a.prod and the target's identity, and the
// bodies, each behind its length as a 4-byte big-endian prefix, equal testdata/tenant-payload.bin. The
// row's base URL and endpoint are rewritten to the fakes after Resolve, with private addresses allowed and
// a fast rate limit; nothing else about the row is.
func TestTenantPayloadGolden(t *testing.T) {
	cfg, plan := loadTenantFixture(t, "../config/testdata/tenants.yaml")
	row := fixtureRow(t, plan, goldenPipeline)
	vendor := goldenVendor(t)
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err == nil && r.Header.Get("Content-Encoding") == "gzip" {
			var zr *gzip.Reader
			if zr, err = gzip.NewReader(bytes.NewReader(body)); err == nil {
				body, err = io.ReadAll(zr)
			}
		}
		if err != nil || r.URL.Path != "/v1/metrics" {
			t.Errorf("receiver: %s: %v", r.URL.Path, err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gw.Close)
	row.Source.BaseURL, row.Destination.Endpoint = vendor.URL, gw.URL
	row.Source.HTTP.AllowPrivate = true
	row.Source.RateLimit = config.RateLimitConfig{RPS: 1000, Burst: 1000}
	ef := OTLPEmitterFactory(cfg.Queue.MaxBatchBytes, fastRetry(), logging.NewLimiter(time.Minute))
	a, err := BuildFromPlan(context.Background(), cfg, tenancy.Plan{Rows: []tenancy.Row{row}, TenantShape: true}, assemblyCheckpointer(t), coordinate.Noop{}, ef, noopTelemetryFactory, Options{Secrets: fixtureSecrets()})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	for _, sp := range a.Specs() {
		if sp.Loop.Key().Loop == "analytics" {
			cycleOnce(t, sp)
			ran = true
		}
	}
	if !ran {
		t.Fatal("no analytics loop for team-a.prod")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("the receiver saw no request")
	}
	wantIdentity := map[string]string{"genai_otel_bridge.pipeline": "team-a.prod", "service.name": "genai-otel-bridge", "service.namespace": "team-a-llm", "deployment.environment.name": "prod"}
	var all bytes.Buffer
	for i, b := range bodies {
		var msg collectormetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(b, &msg); err != nil {
			t.Fatalf("body %d: %v", i, err)
		}
		for _, rm := range msg.ResourceMetrics {
			got := map[string]string{}
			for _, kv := range rm.GetResource().GetAttributes() {
				got[kv.Key] = kv.Value.GetStringValue()
			}
			if !maps.Equal(got, wantIdentity) {
				t.Errorf("body %d: resource attributes %v, want %v", i, got, wantIdentity)
			}
		}
		_ = binary.Write(&all, binary.BigEndian, uint32(len(b))) // a bytes.Buffer write never fails
		all.Write(b)
	}
	if *updateTenantPayload {
		if err := os.WriteFile(tenantPayloadGolden, all.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes, %d requests)", tenantPayloadGolden, all.Len(), len(bodies))
		return
	}
	want, err := os.ReadFile(tenantPayloadGolden)
	if err != nil {
		t.Fatalf("read golden (generate it with -update-tenant-payload): %v", err)
	}
	if !bytes.Equal(all.Bytes(), want) {
		t.Fatalf("tenant OTLP bodies changed: got %d bytes in %d requests, want %d bytes (%s); a tenant pipeline's emitted bytes must not change", all.Len(), len(bodies), len(want), tenantPayloadGolden)
	}
}

// goldenSelfLabelSets is every self-metric series a pipeline's telemetry (selfobs.Metrics.With("team-a",
// "team-a.prod")) records when every recording method is called once, by instrument, as sorted encoded
// attribute sets with values. leader, the two memory gauges and tenant_blocked carry no pipeline, and the
// two unlabelled partial-reject series are NewMetrics' own seeds.
var goldenSelfLabelSets = map[string][]string{
	"genai_otel_bridge_auth_errors_total":                       {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_bucket_revised_after_settle_age_seconds": {"loop=analytics,pipeline=team-a.prod,tenant=team-a"},
	"genai_otel_bridge_bucket_revised_after_settle_total":       {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_emit_errors_total": {
		"kind=bad_encoding,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
		"kind=checkpoint_fenced,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
		"kind=checkpoint_load,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
		"kind=checkpoint_save,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
		"kind=collect,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
		"kind=granularity_unexpected,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
		"kind=retryable_exhausted,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
		"kind=unknown_4xx,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a",
	},
	"genai_otel_bridge_emit_partial_success_rejected_total": {
		"pipeline=team-a.prod,plane=logs,tenant=team-a", "pipeline=team-a.prod,plane=metrics,tenant=team-a", "plane=logs", "plane=metrics",
	},
	"genai_otel_bridge_emit_request_duration_seconds":  {"pipeline=team-a.prod,plane=metrics,status_class=2xx,tenant=team-a"},
	"genai_otel_bridge_emitted_logs_total":             {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_emitted_total":                  {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_go_heap_inuse_bytes":            {""},
	"genai_otel_bridge_go_memory_limit_bytes":          {""},
	"genai_otel_bridge_guard_dropped_total":            {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_last_success_timestamp_seconds": {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_leader":                         {""},
	"genai_otel_bridge_loop_degraded":                  {"loop=analytics,pipeline=team-a.prod,reason=scope_mismatch,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_new_label_values_total":         {"pipeline=team-a.prod,series=portkey_api_requests,tenant=team-a"},
	"genai_otel_bridge_queue_depth":                    {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_samples_capped_total":           {"loop=analytics,pipeline=team-a.prod,reason=dpm,source=mt.team-a.prod,tenant=team-a"},
	"genai_otel_bridge_samples_skipped_total": {
		"loop=analytics,pipeline=team-a.prod,reason=duplicate_timestamp,source=mt.team-a.prod,tenant=team-a",
		"loop=analytics,pipeline=team-a.prod,reason=payload_too_large,source=mt.team-a.prod,tenant=team-a",
		"loop=analytics,pipeline=team-a.prod,reason=too_old,source=mt.team-a.prod,tenant=team-a",
	},
	"genai_otel_bridge_scope_assertion_total": {"pipeline=team-a.prod,result=matched,tenant=team-a"},
	"genai_otel_bridge_scope_state": {
		"pipeline=team-a.prod,state=matched,tenant=team-a", "pipeline=team-a.prod,state=mismatched,tenant=team-a", "pipeline=team-a.prod,state=unverified,tenant=team-a",
	},
	"genai_otel_bridge_source_capability_total": {"graph=requests,loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,state=no-data,tenant=team-a"},
	"genai_otel_bridge_source_data_incomplete_total": {
		"loop=analytics,pipeline=team-a.prod,reason=export_failed,source=mt.team-a.prod,tenant=team-a", "loop=analytics,pipeline=team-a.prod,reason=export_stuck,source=mt.team-a.prod,tenant=team-a",
		"loop=analytics,pipeline=team-a.prod,reason=window_oversize,source=mt.team-a.prod,tenant=team-a", "loop=analytics,pipeline=team-a.prod,reason=window_truncated,source=mt.team-a.prod,tenant=team-a",
	},
	"genai_otel_bridge_tenant_blocked":                    {"reason=duplicate_key,tenant=team-a"},
	"genai_otel_bridge_upstream_request_duration_seconds": {"method=GET,pipeline=team-a.prod,status_class=2xx,target=api.portkey.ai,tenant=team-a"},
	"genai_otel_bridge_window_lag_seconds":                {"loop=analytics,pipeline=team-a.prod,source=mt.team-a.prod,tenant=team-a"},
}

// TestTenantSelfMetricLabelSetsGolden (AC6): the label set, key and value, of every self-metric recorded
// through With("team-a", "team-a.prod"), the scope_state, scope_assertion_total and tenant_blocked
// instruments included; every self-metric in selfobs.Signals() is in the literal.
func TestTenantSelfMetricLabelSetsGolden(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	base, err := selfobs.NewMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	m := base.With("team-a", goldenPipeline)
	id := loopid.ID{Source: "mt.team-a.prod", Loop: "analytics"}
	m.SeedLoop(id, "analytics")
	m.EmittedSamples(id, 1)
	m.EmittedLogs(id, 1)
	m.SamplesSkipped(id, "too_old", 1)
	m.EmitError(id, "collect")
	m.GuardDropped(id, 1)
	m.BucketRevisedAfterSettle(id.Loop, id.Source, time.Minute)
	m.NewLabelValue("portkey_api_requests")
	m.SamplesCapped(id, 1)
	m.SourceCapability(id.Loop, id.Source, "requests", "no-data")
	m.SourceDataIncomplete(id.Loop, id.Source, "window_truncated")
	m.AuthError("analytics", "mt.team-a.prod")
	m.QueueDepth(id, 1)
	m.LastSuccess(id, time.Unix(1700, 0))
	m.WindowLag(id, time.Minute)
	m.LoopDegraded(id, "scope_mismatch", true)
	m.ObserveEmitRequest("metrics", 200, nil, time.Second)
	m.ObserveEmitPartialReject("metrics", 1)
	m.ObserveUpstreamRequest("api.portkey.ai", "GET", 200, nil, time.Second)
	m.SetLeader(true)
	m.ScopeState("matched")
	m.ScopeAssertion("matched")
	m.TenantBlocked("team-a", "duplicate_key", true)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	add := func(name string, s attribute.Set) {
		got[name] = append(got[name], s.Encoded(attribute.DefaultEncoder()))
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			switch d := mm.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					add(mm.Name, dp.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					add(mm.Name, dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					add(mm.Name, dp.Attributes)
				}
			default:
				t.Fatalf("%s: unexpected data type %T", mm.Name, mm.Data)
			}
		}
	}
	for name := range got {
		sort.Strings(got[name])
	}
	if !maps.EqualFunc(got, goldenSelfLabelSets, slices.Equal[[]string]) {
		var b strings.Builder
		for _, n := range slices.Sorted(maps.Keys(got)) {
			q := make([]string, 0, len(got[n]))
			for _, s := range got[n] {
				q = append(q, strconv.Quote(s))
			}
			b.WriteString("\t" + strconv.Quote(n) + ": {" + strings.Join(q, ", ") + "},\n")
		}
		t.Errorf("self-metric label sets through With(%q, %q) changed; got:\n%s", "team-a", goldenPipeline, b.String())
	}
	for _, s := range selfobs.Signals() {
		if s.Type == signal.KindMetric {
			if _, ok := goldenSelfLabelSets[s.Name]; !ok {
				t.Errorf("self-metric %s is not in the golden", s.Name)
			}
		}
	}
}
