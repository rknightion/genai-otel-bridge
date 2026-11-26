// SPDX-License-Identifier: Apache-2.0

// Command e2e-helper is a tiny dual-mode test double used only by the k3d failover e2e.
//
//	-mode=sink    : an OTLP/HTTP metrics sink on 127.0.0.1:4318 (loopback — matches the emitter's
//	                https-or-loopback rule; runs as a sidecar in the integrator pod) that always
//	                returns 200 so emit succeeds and the watermark commits. It decodes every export and
//	                records each gauge point it receives as (series, labels, time, value), tagged with
//	                the export's sequence number. A query server on 0.0.0.0:8090 serves /count (exports
//	                absorbed) and /points (every recorded reception, duplicates kept).
//	-mode=portkey : a mock Portkey Analytics API on 0.0.0.0:8081 that answers every
//	                /analytics/graphs/<graph> request with one data point per minute bucket the
//	                requested window touches, valued by pointValue. The values depend only on the
//	                bucket time, so every pod's mock agrees and the suite can check that what reached
//	                the sink is what the source served, exactly once and without gaps.
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func main() {
	mode := flag.String("mode", "sink", "sink|portkey")
	flag.Parse()
	switch *mode {
	case "sink":
		runSink()
	case "portkey":
		runPortkey()
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

// pointValue is the mock's value for the minute bucket starting at bucketStart. The e2e suite
// (test/e2e/main_test.go, wantRequestsValue) repeats this formula to check the emitted values.
func pointValue(bucketStart time.Time) float64 {
	return float64(bucketStart.Unix()/60%1000 + 1)
}

// maxPoints bounds the sink's memory. A suite run records a few thousand points; past the cap the
// sink stops recording and reports it, so a check never reads a silently truncated record.
const maxPoints = 200000

// point is one gauge data point as one export delivered it.
type point struct {
	Series string    `json:"series"`
	Labels string    `json:"labels"` // sorted k=v pairs joined by ","
	Time   time.Time `json:"time"`
	Value  float64   `json:"value"`
	Seq    int64     `json:"seq"` // 1-based sequence number of the export that carried it
}

type sink struct {
	mu        sync.Mutex
	exports   int64
	points    []point
	truncated bool
	badBodies int64
}

func runSink() {
	s := &sink{}
	ingest := http.NewServeMux()
	ingest.HandleFunc("/v1/metrics", s.ingest)
	go func() { log.Fatal(http.ListenAndServe("127.0.0.1:4318", ingest)) }()

	q := http.NewServeMux()
	q.HandleFunc("/count", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int64{"exports": s.exports})
	})
	q.HandleFunc("/points", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"exports": s.exports, "truncated": s.truncated, "bad_bodies": s.badBodies, "points": s.points,
		})
	})
	q.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Fatal(http.ListenAndServe("0.0.0.0:8090", q))
}

// ingest always answers 200, so the emitter never retries and a reception is one emit. A body it
// cannot decode is counted in bad_bodies, which the suite asserts is zero.
func (s *sink) ingest(w http.ResponseWriter, r *http.Request) {
	defer w.WriteHeader(http.StatusOK)
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			s.bad("gzip", err)
			return
		}
		defer func() { _ = zr.Close() }()
		body = zr
	}
	raw, err := io.ReadAll(io.LimitReader(body, 16<<20))
	if err != nil {
		s.bad("read", err)
		return
	}
	pts, err := decodeExport(raw)
	if err != nil {
		s.bad("decode", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exports++
	for _, p := range pts {
		if len(s.points) >= maxPoints {
			s.truncated = true
			break
		}
		p.Seq = s.exports
		s.points = append(s.points, p)
	}
}

func (s *sink) bad(stage string, err error) {
	log.Printf("otlp-sink: %s: %v", stage, err)
	s.mu.Lock()
	s.badBodies++
	s.mu.Unlock()
}

// decodeExport reads an ExportMetricsServiceRequest (field 1: repeated ResourceMetrics) and returns
// its gauge points. The service wrapper is walked by hand, as internal/emit/otlp writes it.
func decodeExport(b []byte) ([]point, error) {
	var out []point
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		if num != 1 || typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			b = b[m:]
			continue
		}
		v, m := protowire.ConsumeBytes(b)
		if m < 0 {
			return nil, protowire.ParseError(m)
		}
		b = b[m:]
		var rm metricspb.ResourceMetrics
		if err := proto.Unmarshal(v, &rm); err != nil {
			return nil, fmt.Errorf("ResourceMetrics: %w", err)
		}
		for _, sm := range rm.GetScopeMetrics() {
			for _, met := range sm.GetMetrics() {
				for _, dp := range met.GetGauge().GetDataPoints() {
					out = append(out, point{
						Series: met.GetName(),
						Labels: labelString(dp),
						Time:   time.Unix(0, int64(dp.GetTimeUnixNano())).UTC(),
						Value:  dp.GetAsDouble() + float64(dp.GetAsInt()),
					})
				}
			}
		}
	}
	return out, nil
}

func labelString(dp *metricspb.NumberDataPoint) string {
	kv := make([]string, 0, len(dp.GetAttributes()))
	for _, a := range dp.GetAttributes() {
		kv = append(kv, a.GetKey()+"="+a.GetValue().GetStringValue())
	}
	sort.Strings(kv)
	return strings.Join(kv, ",")
}

func runPortkey() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("mock-portkey %s %s", r.Method, r.URL.String()) // visibility for e2e debugging (low volume)
		graph := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		lo, errLo := time.Parse(time.RFC3339, r.URL.Query().Get("time_of_generation_min"))
		hi, errHi := time.Parse(time.RFC3339, r.URL.Query().Get("time_of_generation_max"))
		if errLo != nil || errHi != nil || hi.Before(lo) {
			http.Error(w, "bad time_of_generation_min/max", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data_points":       graphPoints(graph, lo, hi),
			"is_quota_exceeded": false,
			"object":            "analytics-graph",
		})
	})
	// 8081, NOT 8080 — the integrator's own health server binds :8080 in the shared pod netns.
	log.Fatal(http.ListenAndServe("0.0.0.0:8081", mux))
}

// graphPoints returns one bucket-START-stamped point per minute bucket touching [lo, hi], in the
// shape of the named graph: latency carries avg/p50/p90/p99 (milliseconds), tokens carries total with
// its request/response split, every other graph carries total.
func graphPoints(graph string, lo, hi time.Time) []map[string]any {
	var out []map[string]any
	for b := lo.UTC().Truncate(time.Minute); !b.After(hi); b = b.Add(time.Minute) {
		v := pointValue(b)
		p := map[string]any{"timestamp": b.Format("2006-01-02T15:04:05.000Z")}
		switch graph {
		case "latency":
			p["avg"], p["p50"], p["p90"], p["p99"] = v, v, v+1, v+2
		case "tokens":
			p["total"], p["total_request_units"], p["total_response_units"] = 3*v, v, 2*v
		default:
			p["total"] = v
		}
		out = append(out, p)
	}
	return out
}
