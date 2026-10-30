package snapshot

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

// ErrNotFound is returned when a snapshot, manifest or tag does not exist.
var ErrNotFound = errors.New("not found")

// Store is where snapshots live. S3 is the first implementation. The layout is
//
//	snapshots/<name>/data.tar.zst
//	snapshots/<name>/manifest.json
//	tags/<tag>                      text file with the snapshot name
type Store interface {
	// Resolve returns the snapshot name a tag points to.
	Resolve(ctx context.Context, tag string) (string, error)
	// Manifest reads the manifest of a snapshot.
	Manifest(ctx context.Context, name string) (manifest.Manifest, error)
	// Fetch opens the data archive of a snapshot.
	Fetch(ctx context.Context, name string) (io.ReadCloser, error)
	// PutData uploads the data archive from r.
	PutData(ctx context.Context, name string, r io.Reader) error
	// PutManifest writes the manifest. It must be called after PutData.
	PutManifest(ctx context.Context, m manifest.Manifest) error
	// PutTag points a tag at a snapshot. It must be called after PutManifest.
	PutTag(ctx context.Context, tag, name string) error
	// Delete removes a snapshot and every tag that points to it.
	Delete(ctx context.Context, name string) error
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu        sync.Mutex
	Data      map[string][]byte
	Manifests map[string][]byte
	Tags      map[string]string
	// Order records the calls to the Put methods, for tests of the write order.
	Order []string
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{Data: map[string][]byte{}, Manifests: map[string][]byte{}, Tags: map[string]string{}}
}

func (s *MemStore) Resolve(_ context.Context, tag string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.Tags[tag]
	if !ok {
		return "", ErrNotFound
	}
	return n, nil
}

func (s *MemStore) Manifest(_ context.Context, name string) (manifest.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Manifests[name]
	if !ok {
		return manifest.Manifest{}, ErrNotFound
	}
	return manifest.Parse(b)
}

func (s *MemStore) Fetch(_ context.Context, name string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Data[name]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytesReader(b)), nil
}

func (s *MemStore) PutData(_ context.Context, name string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Data[name] = b
	s.Order = append(s.Order, "data")
	return nil
}

func (s *MemStore) PutManifest(_ context.Context, m manifest.Manifest) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Manifests[m.Name] = b
	s.Order = append(s.Order, "manifest")
	return nil
}

func (s *MemStore) PutTag(_ context.Context, tag, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tags[tag] = name
	s.Order = append(s.Order, "tag")
	return nil
}

func (s *MemStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Data, name)
	delete(s.Manifests, name)
	for t, n := range s.Tags {
		if n == name {
			delete(s.Tags, t)
		}
	}
	return nil
}
