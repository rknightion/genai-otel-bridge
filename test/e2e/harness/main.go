// SPDX-License-Identifier: AGPL-3.0-only

// Command e2e-helper is a tiny dual-mode test double used only by the k3d failover e2e.
//
//	-mode=sink    : an OTLP/HTTP metrics sink on 127.0.0.1:4318 (loopback — matches the emitter's
//	                https-or-loopback rule; runs as a sidecar in the integrator pod) that always
//	                returns 200 so emit succeeds and the watermark commits; plus a query server on
//	                0.0.0.0:8090 reporting how many exports it absorbed.
//	-mode=portkey : a mock Portkey Analytics API on 0.0.0.0:8080 that returns an empty (but valid)
//	                graph envelope for every /analytics/graphs/* request. An empty window is a
//	                SUCCESSFUL poll: the analytics loop advances the watermark to `until` regardless
//	                of data (internal/source/portkey Collect, CP-C2), so the checkpoint advances each
//	                tick with zero data-shape coupling. Emit-path fidelity is covered by unit tests
//	                and the live PoC — this e2e proves HA (lease + checkpoint failover).
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"sync/atomic"
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

func runSink() {
	var exports int64
	ingest := http.NewServeMux()
	ingest.HandleFunc("/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		atomic.AddInt64(&exports, 1)
		w.WriteHeader(http.StatusOK)
	})
	go func() { log.Fatal(http.ListenAndServe("127.0.0.1:4318", ingest)) }()

	q := http.NewServeMux()
	q.HandleFunc("/count", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{"exports": atomic.LoadInt64(&exports)})
	})
	q.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Fatal(http.ListenAndServe("0.0.0.0:8090", q))
}

func runPortkey() {
	mux := http.NewServeMux()
	// Valid empty envelope for any graph request → a successful poll → watermark advances.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("mock-portkey %s %s", r.Method, r.URL.Path) // visibility for e2e debugging (low volume)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data_points":[],"is_quota_exceeded":false,"object":"analytics_graph"}`))
	})
	// 8081, NOT 8080 — the integrator's own health server binds :8080 in the shared pod netns.
	log.Fatal(http.ListenAndServe("0.0.0.0:8081", mux))
}
