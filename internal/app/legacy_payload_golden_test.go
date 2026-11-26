// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/file"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/logging"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

var updateLegacyPayload = flag.Bool("update", false, "rewrite testdata/legacy-payload.bin from the current composition root")

const (
	legacyPayloadGolden   = "testdata/legacy-payload.bin"
	legacyPayloadRequests = 6
)

// TestLegacyPayloadGolden [unit 3b-2, G6] pins the bytes a legacy install sends to its telemetry
// destination. minimalConfig with the fixed-anchor fake Portkey, built through Build with the production
// OTLPEmitterFactory, the loop clock pinned and one scheduler tick. The runner emits one request per
// settled bucket (six here: nine buckets less the three-minute settle), in ascending bucket order, so the
// golden file holds every decompressed /v1/metrics body in arrival order, each behind its length as a
// 4-byte big-endian prefix. testdata/legacy-payload.bin was generated with -update on the unit 3b-1
// commit, before assembly moved to plan rows. A legacy row that gained an identity attribute (the
// pipeline attribute, say), a reordered resource or a changed point would change these bytes.
func TestLegacyPayloadGolden(t *testing.T) {
	anchor := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	pk := newFakePortkey(t, anchor, 9)

	var (
		mu     sync.Mutex
		bodies [][]byte
		paths  []string
	)
	got := make(chan struct{}, 16)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		body := raw
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Errorf("gzip: %v", err)
			} else if body, err = io.ReadAll(zr); err != nil {
				t.Errorf("gunzip: %v", err)
			}
		}
		mu.Lock()
		bodies = append(bodies, body)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		got <- struct{}{}
	}))
	t.Cleanup(gw.Close)

	cfg := minimalConfig(pk.URL)
	cfg.Emit.Telemetry.OTLP.Endpoint = gw.URL
	cp, err := file.New(filepath.Join(t.TempDir(), "cp.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	fast := otlp.RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 30 * time.Millisecond, Jitter: 0}
	ef := OTLPEmitterFactory(cfg.Queue.MaxBatchBytes, fast, logging.NewLimiter(time.Minute))
	a, err := Build(context.Background(), cfg, cp, coordinate.Noop{}, ef, noopTelemetryFactory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Specs()) != 1 {
		t.Fatalf("specs = %d, want 1", len(a.Specs()))
	}
	sp := a.Specs()[0]
	if !portkey.SetLoopClockForTest(sp.Loop, func() time.Time { return anchor }) {
		t.Fatal("failed to inject clock into portkey loop")
	}

	ctx, cancel := context.WithCancel(coordinate.WithEpoch(context.Background(), 1))
	done := make(chan struct{})
	go func() { defer close(done); sp.Runner.Run(ctx) }()
	a.sched.RunOnceForTest(ctx, sp, anchor)
	for i := 0; i < legacyPayloadRequests; i++ {
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Fatalf("only %d of %d emits reached the receiver within 10s", i, legacyPayloadRequests)
		}
	}
	// The emit worker commits after the request returns; wait for the batch to finish before reading.
	deadline := time.Now().Add(10 * time.Second)
	for sp.Runner.Busy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != legacyPayloadRequests {
		t.Fatalf("receiver saw %d requests, want %d (one per settled bucket)", len(bodies), legacyPayloadRequests)
	}
	var all bytes.Buffer
	for i, b := range bodies {
		if paths[i] != "/v1/metrics" {
			t.Fatalf("request %d went to %s, want /v1/metrics", i, paths[i])
		}
		_ = binary.Write(&all, binary.BigEndian, uint32(len(b))) // a bytes.Buffer write never fails
		all.Write(b)
	}
	if *updateLegacyPayload {
		if err := os.WriteFile(legacyPayloadGolden, all.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", legacyPayloadGolden, all.Len())
		return
	}
	want, err := os.ReadFile(legacyPayloadGolden)
	if err != nil {
		t.Fatalf("read golden (generate it with -update on the unit 3b-1 commit): %v", err)
	}
	if !bytes.Equal(all.Bytes(), want) {
		t.Fatalf("legacy OTLP bodies changed: got %d bytes, want %d bytes (testdata/legacy-payload.bin); a legacy row's emitted bytes must not change", all.Len(), len(want))
	}
}
