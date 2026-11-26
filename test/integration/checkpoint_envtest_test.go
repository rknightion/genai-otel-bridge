// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	cpcm "github.com/rknightion/genai-otel-bridge/internal/checkpoint/configmap"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// One writer under a REAL apiserver: a forward Save persists and a backwards Save is refused with
// ErrStaleWrite. This proves the monotonic Time check and that the sanitised data key is accepted by
// a real API server (the fake clientset accepts any key). It has one writer and one epoch, so it says
// nothing about the epoch fence or a concurrent writer: TestRealApiserverCheckpointTwoWriterEpochFence
// covers those.
func TestRealApiserverCheckpointMonotonic(t *testing.T) {
	cs, ns := startEnv(t)
	store := cpcm.New(cs, ns, "genai-otel-bridge-checkpoints")
	ctx := context.Background()
	key := model.CheckpointKey{SourceInstance: "portkey-e2e", Loop: "analytics", OutputFingerprint: "testfp"}

	t0 := time.Now().UTC().Truncate(time.Second)
	if err := store.Save(ctx, key, model.Watermark{Time: t0, Epoch: 1}); err != nil {
		t.Fatalf("save t0: %v", err)
	}
	if err := store.Save(ctx, key, model.Watermark{Time: t0.Add(time.Minute), Epoch: 1}); err != nil {
		t.Fatalf("forward save: %v", err)
	}
	if err := store.Save(ctx, key, model.Watermark{Time: t0.Add(-time.Minute), Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("expected ErrStaleWrite on backward save, got %v", err)
	}
	got, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Time.Equal(t0.Add(time.Minute)) {
		t.Fatalf("watermark not at forward value: got %v want %v", got.Time, t0.Add(time.Minute))
	}
}

// Two writers, two epochs, one ConfigMap, under a REAL apiserver: the demoted leader (epoch 1) and
// the new leader (epoch 2) are separate Stores, as two pods are. The demoted leader reads the
// ConfigMap, the new leader's whole Save lands before its write, and its Update then carries a stale
// resourceVersion. The API server must reject it with 409; the retry re-reads the epoch-2 record and
// the epoch fence must refuse the write, even though its Time is FORWARD. Without the server's
// resourceVersion check the stale write would land unconditionally and clobber the epoch-2 frontier;
// without the epoch fence the retry would land it. A later stale-epoch forward write with no race is
// refused too, and the new leader keeps advancing.
func TestRealApiserverCheckpointTwoWriterEpochFence(t *testing.T) {
	cs, ns := startEnv(t)
	const name = "genai-otel-bridge-checkpoints"
	ctx := context.Background()
	key := model.CheckpointKey{SourceInstance: "portkey-e2e", Loop: "analytics", OutputFingerprint: "testfp"}
	t0 := time.Now().UTC().Truncate(time.Second)

	newLeader := cpcm.New(cs, ns, name)
	race := &racingClient{Interface: cs}
	oldLeader := cpcm.New(race, ns, name)

	if err := oldLeader.Save(ctx, key, model.Watermark{Time: t0, Epoch: 1}); err != nil {
		t.Fatalf("old leader save t0 at epoch 1: %v", err)
	}

	race.afterGet = func() {
		if err := newLeader.Save(ctx, key, model.Watermark{Time: t0.Add(time.Minute), Epoch: 2}); err != nil {
			t.Errorf("new leader save t0+1m at epoch 2 (between the old leader's read and write): %v", err)
		}
	}
	err := oldLeader.Save(ctx, key, model.Watermark{Time: t0.Add(2 * time.Minute), Epoch: 1})
	if !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("stale-epoch write racing the new leader: want ErrStaleWrite, got %v", err)
	}
	if race.conflicts != 1 {
		t.Fatalf("the API server rejected %d stale-resourceVersion updates, want 1 (the race did not happen as staged)", race.conflicts)
	}
	requireWatermark(t, newLeader, key, t0.Add(time.Minute), 2)

	if err := oldLeader.Save(ctx, key, model.Watermark{Time: t0.Add(3 * time.Minute), Epoch: 1}); !errors.Is(err, checkpoint.ErrStaleWrite) {
		t.Fatalf("stale-epoch forward write: want ErrStaleWrite, got %v", err)
	}
	if err := newLeader.Save(ctx, key, model.Watermark{Time: t0.Add(2 * time.Minute), Epoch: 2}); err != nil {
		t.Fatalf("new leader forward save at epoch 2: %v", err)
	}
	requireWatermark(t, oldLeader, key, t0.Add(2*time.Minute), 2)
}

func requireWatermark(t *testing.T, s *cpcm.Store, key model.CheckpointKey, at time.Time, epoch int64) {
	t.Helper()
	got, err := s.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Time.Equal(at) || got.Epoch != epoch {
		t.Fatalf("durable watermark = %v at epoch %d, want %v at epoch %d", got.Time, got.Epoch, at, epoch)
	}
}

// racingClient is a real clientset whose ConfigMap Get runs afterGet once, after the read returns,
// and which counts the Updates the API server rejects with 409 Conflict.
type racingClient struct {
	kubernetes.Interface
	afterGet  func()
	conflicts int
}

func (c *racingClient) CoreV1() corev1client.CoreV1Interface {
	return racingCore{CoreV1Interface: c.Interface.CoreV1(), c: c}
}

type racingCore struct {
	corev1client.CoreV1Interface
	c *racingClient
}

func (r racingCore) ConfigMaps(namespace string) corev1client.ConfigMapInterface {
	return racingConfigMaps{ConfigMapInterface: r.CoreV1Interface.ConfigMaps(namespace), c: r.c}
}

type racingConfigMaps struct {
	corev1client.ConfigMapInterface
	c *racingClient
}

func (m racingConfigMaps) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	cm, err := m.ConfigMapInterface.Get(ctx, name, opts)
	if hook := m.c.afterGet; hook != nil {
		m.c.afterGet = nil
		hook()
	}
	return cm, err
}

func (m racingConfigMaps) Update(ctx context.Context, cm *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	out, err := m.ConfigMapInterface.Update(ctx, cm, opts)
	if apierrors.IsConflict(err) {
		m.c.conflicts++
	}
	return out, err
}
