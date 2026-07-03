// SPDX-License-Identifier: AGPL-3.0-only

package configmap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func TestConfigMapRoundTripAndFence(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := New(cs, "genai-otel-bridge", "genai-otel-bridge-checkpoints")
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	ctx := context.Background()

	if w, err := s.Load(ctx, key); err != nil || !w.Time.IsZero() {
		t.Fatalf("absent: %+v %v", w, err)
	}
	w1 := model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 1}
	if err := s.Save(ctx, key, w1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx, key); !got.Time.Equal(w1.Time) {
		t.Fatalf("reload %+v", got)
	}
	if err := s.Save(ctx, key, model.Watermark{Time: time.Unix(50, 0).UTC(), Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("want stale, got %v", err)
	}
}

// TestConfigMapCursorFence proves the cursor-relaxed fence holds through the PROD store (review-H1):
// a same-Time cursor advance is accepted (logs-export job step), a same-Time/same-cursor re-write is
// rejected, and Time still cannot regress.
func TestConfigMapCursorFence(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := New(cs, "genai-otel-bridge", "genai-otel-bridge-checkpoints")
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "logs_export", OutputFingerprint: "fp"}
	ctx := context.Background()
	t100 := time.Unix(100, 0).UTC()
	if err := s.Save(ctx, key, model.Watermark{Time: t100, Cursor: "a", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, key, model.Watermark{Time: t100, Cursor: "a", Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("same-Time/same-cursor must be ErrStaleWrite, got %v", err)
	}
	if err := s.Save(ctx, key, model.Watermark{Time: t100, Cursor: "b", Epoch: 1}); err != nil {
		t.Fatalf("same-Time/new-cursor must be accepted, got %v", err)
	}
	if got, _ := s.Load(ctx, key); got.Cursor != "b" || !got.Time.Equal(t100) {
		t.Fatalf("load after cursor advance: %+v", got)
	}
	if err := s.Save(ctx, key, model.Watermark{Time: time.Unix(50, 0).UTC(), Cursor: "c", Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("backward Time must be ErrStaleWrite even with a cursor change, got %v", err)
	}
}

// TestConfigMapRejectsUnencodableTime [#81]: a watermark whose Time has an out-of-range year
// (>9999) would json.Marshal to "" and durably poison the key (every later Load errors, every Save
// refuses to clobber). Save must reject it loudly, write NOTHING, and leave the prior key readable.
func TestConfigMapRejectsUnencodableTime(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := New(cs, "genai-otel-bridge", "genai-otel-bridge-checkpoints")
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	ctx := context.Background()

	good := model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 1}
	if err := s.Save(ctx, key, good); err != nil {
		t.Fatalf("seed good watermark: %v", err)
	}
	bad := model.Watermark{Time: time.Date(10001, 1, 1, 0, 0, 0, 0, time.UTC), Epoch: 2}
	if err := s.Save(ctx, key, bad); !errors.Is(err, checkpoint.ErrUnencodable) {
		t.Fatalf("year-10001 Save must be ErrUnencodable, got %v", err)
	}
	// The stored key stays READABLE and unchanged (not poisoned with "").
	got, err := s.Load(ctx, key)
	if err != nil {
		t.Fatalf("stored key must stay readable after a rejected Save, got %v", err)
	}
	if !got.Time.Equal(good.Time) {
		t.Fatalf("stored value was clobbered: %+v", got)
	}
}

// TestConfigMapRMWAttemptCountOnExhaustion [#116]: under sustained resourceVersion contention the
// configmap backend performs 1 initial attempt + retries re-tries = retries+1 total attempts, and the
// exhaustion error must state the actual number of attempts made (not just the retry count) — the
// number the dynamodb backend must mirror exactly.
func TestConfigMapRMWAttemptCountOnExhaustion(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "genai-otel-bridge-checkpoints", Namespace: "genai-otel-bridge"}, Data: map[string]string{}})
	var n int
	cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		n++
		return true, nil, apiConflict() // every update conflicts → RMW exhausts
	})
	s := New(cs, "genai-otel-bridge", "genai-otel-bridge-checkpoints")
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	err := s.Save(context.Background(), key, model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 1})
	if err == nil {
		t.Fatal("sustained resourceVersion conflict must exhaust and error")
	}
	if n != 6 {
		t.Fatalf("RMW made %d update attempts, want 6 (retries=5 → 1 initial + 5 retries)", n)
	}
	if !strings.Contains(err.Error(), "6 attempts") {
		t.Fatalf("exhaustion error must state the actual attempt count; got %q", err.Error())
	}
}

// TestConfigMapCorruptValueRefused [CP-C10 / #117]: a ConfigMap data key holding a non-JSON value must
// make Load ERROR (present-but-unreadable ⇒ refuse, never bootstrap a zero watermark over a real
// frontier) and Save REFUSE to overwrite it while writing NOTHING to the API (never clobber). Mirrors
// dynamodb's TestSaveRefusesCorruptStored; the package CLAUDE.md claimed corruption was covered but the
// configmap backend had no test for either path.
func TestConfigMapCorruptValueRefused(t *testing.T) {
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	seed := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "genai-otel-bridge-checkpoints", Namespace: "genai-otel-bridge"},
		Data:       map[string]string{dataKey(key): "{not-json"}, // corrupt value at the REAL data key
	}
	cs := fake.NewSimpleClientset(seed)
	var writes int
	countWrite := func(k8stesting.Action) (bool, runtime.Object, error) { writes++; return false, nil, nil }
	cs.PrependReactor("update", "configmaps", countWrite)
	cs.PrependReactor("create", "configmaps", countWrite)
	s := New(cs, "genai-otel-bridge", "genai-otel-bridge-checkpoints")
	ctx := context.Background()

	// Load must refuse a present-but-unreadable value (not silently bootstrap a zero watermark, CP-C1).
	if _, err := s.Load(ctx, key); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Load of a corrupt data-key value must error; got %v", err)
	}
	// Save must refuse to overwrite it AND make zero API writes (CP-C10, never clobber).
	err := s.Save(ctx, key, model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 1})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite corrupt") {
		t.Fatalf("Save over a corrupt data-key value must refuse with a CP-C10 error; got %v", err)
	}
	if writes != 0 {
		t.Fatalf("Save refused the corrupt overwrite but made %d API writes (must be zero)", writes)
	}
}

func TestConfigMapConflictRetry(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "genai-otel-bridge-checkpoints", Namespace: "genai-otel-bridge"}, Data: map[string]string{}})
	// Inject one Conflict on the first update, then let it succeed.
	var n int
	cs.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		n++
		if n == 1 {
			return true, nil, apiConflict()
		}
		return false, nil, nil // fall through to default tracker
	})
	s := New(cs, "genai-otel-bridge", "genai-otel-bridge-checkpoints")
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	if err := s.Save(context.Background(), key, model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 1}); err != nil {
		t.Fatalf("RMW should retry past one conflict: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected a retry, updates=%d", n)
	}
}
