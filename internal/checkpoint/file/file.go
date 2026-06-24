// SPDX-License-Identifier: AGPL-3.0-only

// Package file is the dev/non-k8s Checkpointer: a YAML map persisted with atomic temp-then-
// rename. DISCOURAGED for HA/critical prod (Cdx-M5) — prefer the configmap impl.
package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/grafana-ps/aip-oi/internal/checkpoint"
	"github.com/grafana-ps/aip-oi/internal/model"
)

type record struct {
	Time   time.Time `yaml:"time"`
	Cursor string    `yaml:"cursor"`
	Epoch  int64     `yaml:"epoch"`
}

// Store is the file-backed Checkpointer. All access is mutex-guarded; writes are atomic
// via temp-then-rename so a crash mid-write leaves the old file intact.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string]record
}

// New loads the file. Absent ⇒ empty store. Corrupt ⇒ error unless ignoreInvalid (loud bootstrap).
func New(path string, ignoreInvalid bool) (*Store, error) {
	s := &Store{path: path, data: map[string]record{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint/file: read: %w", err)
	}
	if err := yaml.Unmarshal(b, &s.data); err != nil {
		if ignoreInvalid {
			fmt.Fprintf(os.Stderr, "WARN checkpoint/file: corrupt %s ignored, bootstrapping: %v\n", path, err)
			s.data = map[string]record{}
			return s, nil
		}
		return nil, fmt.Errorf("checkpoint/file: corrupt %s (refusing start): %w", path, err)
	}
	return s, nil
}

func (s *Store) Load(_ context.Context, key model.CheckpointKey) (model.Watermark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[key.String()]
	if !ok {
		return model.Watermark{}, nil
	}
	return model.Watermark{Time: r.Time.UTC(), Cursor: r.Cursor, Epoch: r.Epoch}, nil
}

func (s *Store) Save(_ context.Context, key model.CheckpointKey, w model.Watermark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.data[key.String()]
	if err := checkpoint.CheckMonotonic(model.Watermark{Time: stored.Time, Cursor: stored.Cursor, Epoch: stored.Epoch}, w); err != nil {
		return err
	}
	s.data[key.String()] = record{Time: w.Time.UTC(), Cursor: w.Cursor, Epoch: w.Epoch}
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	b, err := yaml.Marshal(s.data)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path) // atomic on the same filesystem
}

var _ checkpoint.Checkpointer = (*Store)(nil)
