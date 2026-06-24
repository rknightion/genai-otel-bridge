// SPDX-License-Identifier: AGPL-3.0-only

package configmap

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/grafana-ps/aip-oi/internal/checkpoint"
	"github.com/grafana-ps/aip-oi/internal/model"
)

func TestConfigMapRoundTripAndFence(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := New(cs, "aip-oi", "aip-oi-checkpoints")
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
	s := New(cs, "aip-oi", "aip-oi-checkpoints")
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

func TestConfigMapConflictRetry(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "aip-oi-checkpoints", Namespace: "aip-oi"}, Data: map[string]string{}})
	// Inject one Conflict on the first update, then let it succeed.
	var n int
	cs.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		n++
		if n == 1 {
			return true, nil, apiConflict()
		}
		return false, nil, nil // fall through to default tracker
	})
	s := New(cs, "aip-oi", "aip-oi-checkpoints")
	key := model.CheckpointKey{SourceInstance: "pk", Loop: "analytics", OutputFingerprint: "fp"}
	if err := s.Save(context.Background(), key, model.Watermark{Time: time.Unix(100, 0).UTC(), Epoch: 1}); err != nil {
		t.Fatalf("RMW should retry past one conflict: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected a retry, updates=%d", n)
	}
}
