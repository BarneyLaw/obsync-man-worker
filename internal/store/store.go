// Package store abstracts the object store. Deliberately small: content
// addressing means we never need rename, copy or conditional update, which is
// exactly what would have forced backend-specific behaviour into the interface.
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("store: not found")

type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
	// List calls fn for each object under prefix. Callback rather than a slice
	// because GC walks the whole blob namespace and must not materialise it.
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
