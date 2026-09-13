package store

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is the test double. It implements RangeReader so the plugin's chunked
// path can be exercised without a live Garage.
type Memory struct {
	mu   sync.RWMutex
	data map[string][]byte
	// PutErr, if set, fails the next Put. Used to test that a crashed run
	// never publishes a torn manifest.
	PutErr error
}

func NewMemory() *Memory { return &Memory{data: map[string][]byte{}} }

func (m *Memory) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *Memory) GetRange(_ context.Context, key string, off, n int64) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if off >= int64(len(b)) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	end := off + n
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	return io.NopCloser(bytes.NewReader(b[off:end])), nil
}

func (m *Memory) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	if m.PutErr != nil {
		return m.PutErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
	return nil
}

func (m *Memory) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.data[key]
	return ok, nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *Memory) List(_ context.Context, prefix string, fn func(ObjectInfo) error) error {
	m.mu.RLock()
	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	m.mu.RUnlock()
	sort.Strings(keys)
	for _, k := range keys {
		m.mu.RLock()
		size := int64(len(m.data[k]))
		m.mu.RUnlock()
		if err := fn(ObjectInfo{Key: k, Size: size, Modified: time.Now()}); err != nil {
			return err
		}
	}
	return nil
}
