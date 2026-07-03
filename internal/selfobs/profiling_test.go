// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestStartProfilingDisabledIsNoop(t *testing.T) {
	stop, err := StartProfiling(ProfilingConfig{Enabled: false})
	if err != nil {
		t.Fatalf("disabled should not error: %v", err)
	}
	if stop == nil {
		t.Fatal("stop must be non-nil even when disabled")
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("no-op stop should not error: %v", err)
	}
}

func TestStartProfilingPullServesPprof(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := servePprof(ln) // internal helper that owns the listener
	defer func() { _ = stop(context.Background()) }()

	url := "http://" + ln.Addr().String() + "/debug/pprof/goroutine?debug=1"
	var resp *http.Response
	for range 50 { // server goroutine may not be ready instantly
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET pprof: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if len(b) == 0 {
		t.Fatal("expected non-empty goroutine profile")
	}
}

// [#129] shutdownServer must stay bounded even with a request in flight: a pull-mode
// /debug/pprof/profile?seconds=N holds its connection open for N seconds, so an unbounded drain would
// block process exit (and the final self-metrics flush) until SIGKILL. The drain is bounded by ctx and
// force-closes on deadline, so it returns promptly instead of waiting for the request to finish.
func TestShutdownServerForceClosesHungRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	mux := http.NewServeMux()
	mux.HandleFunc("/block", func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release // simulate a long-running /debug/pprof/profile pull
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	go func() { _, _ = http.Get("http://" + ln.Addr().String() + "/block") }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request never reached the handler")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = shutdownServer(ctx, srv)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("shutdown blocked on the in-flight request for %v — not bounded", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded from the bounded drain, got %v", err)
	}
}

func TestBuildPyroscopeConfig(t *testing.T) {
	cfg := ProfilingConfig{
		Mode: "push", PushEndpoint: "https://profiles.example", PushInstanceID: "42", PushToken: "tok",
		ServiceNamespace: "genai-otel-bridge-meta", DeploymentEnvironment: "prod", Instance: "pod-abc",
	}
	pc := buildPyroscopeConfig(cfg)
	if pc.ServerAddress != "https://profiles.example" {
		t.Errorf("ServerAddress = %q", pc.ServerAddress)
	}
	if pc.BasicAuthUser != "42" || pc.BasicAuthPassword != "tok" {
		t.Errorf("basic auth = %q/%q", pc.BasicAuthUser, pc.BasicAuthPassword)
	}
	if pc.Tags["service_namespace"] != "genai-otel-bridge-meta" || pc.Tags["deployment_environment_name"] != "prod" || pc.Tags["service_instance_id"] != "pod-abc" {
		t.Errorf("tags = %#v", pc.Tags)
	}
	if len(pc.ProfileTypes) == 0 {
		t.Error("expected ProfileTypes to be set")
	}
}
