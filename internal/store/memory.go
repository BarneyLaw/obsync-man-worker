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
	mu       sync.RWMutex
	data     map[string][]byte
	modified map[string]time.Time
	// PutErr, if set, fails every Put whose key it returns an error for. Used
	// to test that a crashed run never publishes a torn manifest.
	PutErr func(key string) error
	// Now stamps object modification times. Defaults to time.Now.
	Now func() time.Time
}

func NewMemory() *Memory {
	return &Memory{data: map[string][]byte{}, modified: map[string]time.Time{}, Now: time.Now}
}

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

func (m *Memory) read(key string, r io.Reader, size int64) ([]byte, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	if m.PutErr != nil {
		if err := m.PutErr(key); err != nil {
			return nil, err
		}
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return b, checkSize(key, size, int64(len(b)))
}

func (m *Memory) Put(_ context.Context, key string, r io.Reader, size int64) error {
	b, err := m.read(key, r, size)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
	m.modified[key] = m.Now()
	return nil
}

func (m *Memory) PutIfAbsent(_ context.Context, key string, r io.Reader, size int64) error {
	b, err := m.read(key, r, size)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[key]; ok {
		return ErrExists
	}
	m.data[key] = b
	m.modified[key] = m.Now()
	return nil
}

func (m *Memory) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.data[key]
	return ok, nil
}

func (m *Memory) Stat(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: int64(len(b)), Modified: m.modified[key]}, nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	delete(m.modified, key)
	return nil
}

func (m *Memory) List(_ context.Context, prefix string, fn func(ObjectInfo) error) error {
	m.mu.RLock()
	infos := make([]ObjectInfo, 0, len(m.data))
	for k, b := range m.data {
		if strings.HasPrefix(k, prefix) {
			infos = append(infos, ObjectInfo{Key: k, Size: int64(len(b)), Modified: m.modified[k]})
		}
	}
	m.mu.RUnlock()
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	for _, info := range infos {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}
