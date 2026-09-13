package store

import (
	"context"
	"io"
	"time"
)

// S3 talks to Garage (or R2, or anything S3-compatible).
//
// STUB. Fill this in at build step 5, after the whole pipeline already works
// against FS. Nothing above this line changes when you do.
//
// Implementation notes for when you get here:
//   - github.com/aws/aws-sdk-go-v2 with BaseEndpoint set and
//     UsePathStyle: true. Garage does not do virtual-host addressing.
//   - Region is required by the signer but meaningless to Garage. Use
//     "garage" consistently on both sides.
//   - Garage has no object versioning, so every Put to an existing key is
//     destructive. That is safe here only because every key except
//     manifests/<course>/latest is write-once.
//   - GetRange maps to GetObject with a Range header. This is the single most
//     load-bearing S3 feature in the whole design (the plugin's mobile path
//     depends on it), so make it the FIRST integration test you write.
type S3 struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) { panic("TODO: step 5") }
func (s *S3) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	panic("TODO: step 5")
}
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	panic("TODO: step 5")
}
func (s *S3) Exists(ctx context.Context, key string) (bool, error) { panic("TODO: step 5") }
func (s *S3) Delete(ctx context.Context, key string) error         { panic("TODO: step 5") }
func (s *S3) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	panic("TODO: step 5")
}
func (s *S3) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	panic("TODO: step 5")
}

var (
	_ Store       = (*S3)(nil)
	_ RangeReader = (*S3)(nil)
	_ Presigner   = (*S3)(nil)
	_ Store       = (*FS)(nil)
	_ RangeReader = (*FS)(nil)
	_ Store       = (*Memory)(nil)
	_ RangeReader = (*Memory)(nil)
)
