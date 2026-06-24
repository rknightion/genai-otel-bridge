// SPDX-License-Identifier: AGPL-3.0-only

package selfobs

import (
	"context"
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

func TestBuildPyroscopeConfig(t *testing.T) {
	cfg := ProfilingConfig{
		Mode: "push", PushEndpoint: "https://profiles.example", PushInstanceID: "42", PushToken: "tok",
		ServiceNamespace: "aip-oi-meta", DeploymentEnvironment: "prod", Instance: "pod-abc",
	}
	pc := buildPyroscopeConfig(cfg)
	if pc.ServerAddress != "https://profiles.example" {
		t.Errorf("ServerAddress = %q", pc.ServerAddress)
	}
	if pc.BasicAuthUser != "42" || pc.BasicAuthPassword != "tok" {
		t.Errorf("basic auth = %q/%q", pc.BasicAuthUser, pc.BasicAuthPassword)
	}
	if pc.Tags["service_namespace"] != "aip-oi-meta" || pc.Tags["deployment_environment_name"] != "prod" || pc.Tags["service_instance_id"] != "pod-abc" {
		t.Errorf("tags = %#v", pc.Tags)
	}
	if len(pc.ProfileTypes) == 0 {
		t.Error("expected ProfileTypes to be set")
	}
}
