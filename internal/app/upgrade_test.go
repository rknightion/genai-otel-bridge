// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint/configmap"
	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
	"github.com/rknightion/genai-otel-bridge/internal/emit/otlp"
	"github.com/rknightion/genai-otel-bridge/internal/schedule"
	"github.com/rknightion/genai-otel-bridge/internal/source/portkey"
)

// TestUpgradeFromCurrentCheckpoints [unit 1, G6] starts the composition root on a ConfigMap written by
// the current version and checks that the first tick RESUMES from it. The fixture,
// testdata/checkpoints-current.json, is the ConfigMap data one tick of minimalConfig wrote at the
// commit before the multi-tenancy work (a legacy install, anchor 2026-01-15T12:00:00Z): its data key
// carries the analytics loop's real output fingerprint and its value is the literal JSON watermark. The
// test never derives a key from code, so a change to the key rules, the fingerprint or the encoding
// makes the first tick bootstrap (a zero watermark) and fails here.
func TestUpgradeFromCurrentCheckpoints(t *testing.T) {
	raw, err := os.ReadFile("testdata/checkpoints-current.json")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	seeded := time.Date(2026, 1, 15, 11, 57, 0, 0, time.UTC) // the fixture's watermark, as a literal
	anchor := time.Date(2026, 1, 15, 12, 10, 0, 0, time.UTC) // ten minutes after the fixture was written

	const ns, name = "genai-otel-bridge", "genai-otel-bridge-checkpoints"
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data})
	cp := configmap.New(cs, ns, name)

	pk := newFakePortkey(t, anchor, 30)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(gw.Close)
	em := otlp.New(otlp.Config{Endpoint: gw.URL, InstanceID: "1", Token: "t", Identity: map[string]string{"service.namespace": "genai-otel-bridge"}, MaxBytes: 1 << 20})
	ef, tf := fixedFactories(em, schedule.NoopMetrics{})
	a, err := Build(context.Background(), minimalConfig(pk.URL), cp, coordinate.Noop{}, ef, tf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sp := a.Specs()[0]
	if !portkey.SetLoopClockForTest(sp.Loop, func() time.Time { return anchor }) {
		t.Fatal("failed to inject clock into portkey loop")
	}

	lc := coordinate.WithEpoch(context.Background(), 1)
	since, err := sp.Runner.Since(lc)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if !since.Time.Equal(seeded) {
		t.Fatalf("first tick since = %v, want the seeded watermark %v (a zero value means the tick bootstrapped and the key no longer matches)", since.Time, seeded)
	}
	b, err := sp.Loop.Collect(lc, since)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	sp.Runner.ProcessBatch(lc, b)
	after, err := cp.Load(context.Background(), sp.Loop.Key())
	if err != nil {
		t.Fatal(err)
	}
	if !after.Time.After(seeded) {
		t.Fatalf("first save did not advance the seeded watermark: %v (seeded %v)", after.Time, seeded)
	}
	// The save went to the SAME data key: the ConfigMap still holds exactly one entry.
	got, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("ConfigMap data has %d keys after the first save, want 1 (the seeded key): %v", len(got.Data), got.Data)
	}
}
