// SPDX-License-Identifier: AGPL-3.0-only

// Package configmap is the default prod Checkpointer: a single ConfigMap, one data key per
// CheckpointKey, RMW with resource-version optimistic concurrency, serialized through one
// writer (M1), monotonic+epoch fenced (Cdx-C14). Size-guarded against the 1 MiB ConfigMap cap.
package configmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"github.com/rknightion/genai-otel-bridge/internal/checkpoint"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

const maxConfigMapBytes = 900 * 1024 // headroom under the 1 MiB API limit

type record struct {
	Time   time.Time `json:"time"`
	Cursor string    `json:"cursor"`
	Epoch  int64     `json:"epoch"`
}

// Store is the ConfigMap-backed Checkpointer. Access is serialized through a mutex (M1 —
// single-writer) which also guards the RMW retry loop against self-contention.
type Store struct {
	cs      kubernetes.Interface
	ns      string
	name    string
	mu      sync.Mutex // serializes RMW (single-writer, M1)
	retries int
}

// New creates a Store that reads/writes the named ConfigMap in the given namespace.
func New(cs kubernetes.Interface, namespace, name string) *Store {
	return &Store{cs: cs, ns: namespace, name: name, retries: 5}
}

func (s *Store) Load(ctx context.Context, key model.CheckpointKey) (model.Watermark, error) {
	cm, err := s.cs.CoreV1().ConfigMaps(s.ns).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return model.Watermark{}, nil
	}
	if err != nil {
		return model.Watermark{}, fmt.Errorf("checkpoint/configmap: get: %w", err)
	}
	raw, ok := cm.Data[dataKey(key)]
	if !ok {
		return model.Watermark{}, nil
	}
	var r record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		// present-but-unreadable ⇒ refuse (do not bootstrap over a real watermark, C1).
		return model.Watermark{}, fmt.Errorf("checkpoint/configmap: corrupt key %s (refusing): %w", key, err)
	}
	return model.Watermark{Time: r.Time.UTC(), Cursor: r.Cursor, Epoch: r.Epoch}, nil
}

func (s *Store) Save(ctx context.Context, key model.CheckpointKey, w model.Watermark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt <= s.retries; attempt++ {
		cm, err := s.cs.CoreV1().ConfigMaps(s.ns).Get(ctx, s.name, metav1.GetOptions{})
		create := apierrors.IsNotFound(err)
		if err != nil && !create {
			return fmt.Errorf("checkpoint/configmap: get: %w", err)
		}
		if create {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.ns}, Data: map[string]string{}}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		var stored record
		if raw, ok := cm.Data[dataKey(key)]; ok {
			if err := json.Unmarshal([]byte(raw), &stored); err != nil {
				// [CP-C10] do NOT overwrite a present-but-unreadable checkpoint (matches Load's refusal):
				// clobbering it would hide corruption and risk re-emitting over a real frontier.
				return fmt.Errorf("checkpoint/configmap: refusing to overwrite corrupt key %s: %w", key, err)
			}
		}
		if err := checkpoint.CheckMonotonic(model.Watermark{Time: stored.Time, Cursor: stored.Cursor, Epoch: stored.Epoch}, w); err != nil {
			return err // ErrStaleWrite — benign to caller
		}
		enc, _ := json.Marshal(record{Time: w.Time.UTC(), Cursor: w.Cursor, Epoch: w.Epoch})
		cm.Data[dataKey(key)] = string(enc)
		if size := dataBytes(cm.Data); size > maxConfigMapBytes {
			return fmt.Errorf("checkpoint/configmap: %d bytes exceeds cap %d (too many keys — use an external store)", size, maxConfigMapBytes)
		}
		if create {
			_, err = s.cs.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{})
		} else {
			_, err = s.cs.CoreV1().ConfigMaps(s.ns).Update(ctx, cm, metav1.UpdateOptions{})
		}
		if err == nil {
			return nil
		}
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			continue // resource-version race — re-read and retry
		}
		return fmt.Errorf("checkpoint/configmap: write: %w", err)
	}
	return fmt.Errorf("checkpoint/configmap: exhausted %d RMW retries for %s", s.retries, key)
}

// dataKey maps a CheckpointKey to a VALID ConfigMap data key. [ext-review-1] CheckpointKey.String()
// joins fields with '/' — fine as a YAML map key (file store) and readable in logs, but k8s rejects
// '/' in a ConfigMap data key (must match [-._a-zA-Z0-9]+). The fake clientset does NOT enforce this,
// so the invalid key only surfaces against a real API server. We sanitise the readable form (any
// disallowed byte → '_') and append a short hash of the FULL logical key, so two logical keys can
// never collide after sanitisation. Deterministic + stable across restarts (the durable identity).
func dataKey(k model.CheckpointKey) string {
	s := k.String()
	sum := sha256.Sum256([]byte(s))
	var b strings.Builder
	b.Grow(len(s) + 13)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	prefix := b.String()
	if len(prefix) > 200 { // keep well under the 253-char key cap; the hash suffix preserves uniqueness
		prefix = prefix[:200]
	}
	return prefix + "." + hex.EncodeToString(sum[:])[:12]
}

func dataBytes(d map[string]string) int {
	n := 0
	for k, v := range d {
		n += len(k) + len(v)
	}
	return n
}

// apiConflict returns a Conflict API error for use in tests that inject a conflict to
// exercise the RMW retry loop.
func apiConflict() error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "genai-otel-bridge-checkpoints", fmt.Errorf("resourceVersion mismatch"))
}

var _ checkpoint.Checkpointer = (*Store)(nil)
