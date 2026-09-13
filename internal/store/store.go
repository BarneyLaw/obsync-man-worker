// Package store abstracts the object store. Deliberately small: content
// addressing means we never need rename, copy or conditional update, which is
// exactly what would have forced backend-specific behaviour into the interface.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
)

type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Put writes r to key. size is the expected byte count, or -1 if
	// unknown; a short or long stream against a known size is an error, so a
	// truncated download can never become an object.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
	// List calls fn for each object under prefix. Callback rather than a slice
	// because GC walks the whole blob namespace and must not materialise it.
	// Pass directory-style prefixes ending in "/": backends differ on bare
	// string prefixes.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
}

// RangeReader is an OPTIONAL capability. Type-assert for it; do not add it to
// Store. The Obsidian plugin's chunked download path needs it, the worker does
// not, and forcing every backend to implement it would be dishonest.
type RangeReader interface {
	GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
}

// Presigner is likewise optional, for a future mobile path that reads blobs
// without holding credentials.
type Presigner interface {
	Presign(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ExclusivePutter is optional: create key only if it does not exist, as one
// atomic operation, returning ErrExists otherwise. It is what makes write-once
// keys and the worker lease safe against a second writer.
type ExclusivePutter interface {
	PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error
}

// PutOnce writes a write-once key and reports whether the backend enforced
// that atomically. Without ExclusivePutter it falls back to Exists then Put,
// which is only safe with a single writer.
func PutOnce(ctx context.Context, s Store, key string, r io.Reader, size int64) (atomic bool, err error) {
	if ep, ok := s.(ExclusivePutter); ok {
		err := ep.PutIfAbsent(ctx, key, r, size)
		if !errors.Is(err, errors.ErrUnsupported) {
			return true, err
		}
	}
	exists, err := s.Exists(ctx, key)
	if err != nil {
		return false, err
	}
	if exists {
		return false, fmt.Errorf("%w: %s", ErrExists, key)
	}
	return false, s.Put(ctx, key, r, size)
}

// ValidKey rejects keys that could escape a filesystem root or that different
// backends would interpret differently.
func ValidKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return fmt.Errorf("store: invalid key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("store: invalid key %q", key)
		}
	}
	return nil
}

// countingReader enforces the size contract on Put.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func checkSize(key string, want, got int64) error {
	if want >= 0 && want != got {
		return fmt.Errorf("store: %s: expected %d bytes, stream had %d", key, want, got)
	}
	return nil
}
