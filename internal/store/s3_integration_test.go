package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests talk to a real S3-compatible server. They run when GARAGE_* is
// set, which scripts/garage-dev.sh prints for a local Garage:
//
//	eval "$(scripts/garage-dev.sh up)"
//	go test ./internal/store -run S3 -v
//
// Without it they skip, unless OBSYNC_REQUIRE_S3=1 (CI), where skipping would
// hide a broken setup, so they fail instead.
func s3ForTest(t *testing.T) (*S3, Store) {
	t.Helper()
	cfg := S3ConfigFromEnv(os.Getenv)
	if cfg.Endpoint == "" {
		if os.Getenv("OBSYNC_REQUIRE_S3") == "1" {
			t.Fatal("OBSYNC_REQUIRE_S3=1 but GARAGE_ENDPOINT is not set")
		}
		t.Skip("set GARAGE_* to run S3 integration tests (scripts/garage-dev.sh up)")
	}
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	prefix := fmt.Sprintf("it/%s-%s/", time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
	t.Cleanup(func() {
		ctx := context.Background()
		_ = s.List(ctx, prefix, func(o ObjectInfo) error { return s.Delete(ctx, o.Key) })
	})
	return s, &prefixed{inner: s, prefix: prefix}
}

// Range GET is the first S3 integration test because the plugin's whole mobile
// path rests on it: chunks must come back exact, truncated at the end, and
// empty past it.
func TestS3RangeGET(t *testing.T) {
	_, st := s3ForTest(t)
	ctx := context.Background()
	blob := make([]byte, 3*1024*1024+17) // several chunks plus a ragged tail
	_, _ = rand.Read(blob)
	if err := st.Put(ctx, "blobs/big", bytes.NewReader(blob), int64(len(blob))); err != nil {
		t.Fatal(err)
	}
	rr := st.(RangeReader)

	read := func(off, n int64) []byte {
		t.Helper()
		rc, err := rr.GetRange(ctx, "blobs/big", off, n)
		if err != nil {
			t.Fatalf("GetRange(%d,%d): %v", off, n, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	size := int64(len(blob))
	for _, c := range []struct{ off, n int64 }{
		{0, 1}, {1, 1}, {1024 * 1024, 4096}, {size - 1, 1}, {size - 10, 100}, {0, size},
	} {
		want := blob[c.off:min(c.off+c.n, size)]
		if got := read(c.off, c.n); !bytes.Equal(got, want) {
			t.Fatalf("GetRange(%d,%d): got %d bytes, want %d", c.off, c.n, len(got), len(want))
		}
	}
	if got := read(size, 10); len(got) != 0 {
		t.Fatalf("range past the end returned %d bytes", len(got))
	}

	// Reassemble in 1 MiB chunks exactly as the plugin does, and verify the hash.
	h := sha256.New()
	for off := int64(0); off < size; off += 1 << 20 {
		h.Write(read(off, min(1<<20, size-off)))
	}
	want := sha256.Sum256(blob)
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("chunked reassembly does not hash to the original")
	}
}

func TestS3Conformance(t *testing.T) {
	_, st := s3ForTest(t)
	conformance(t, st)
}

// The lease is only exclusive on S3 if the server honours If-None-Match.
// Record which it is, and when it does, prove exactly one creator wins.
func TestS3ConditionalWrites(t *testing.T) {
	s, st := s3ForTest(t)
	ctx := context.Background()
	ok, err := s.ConditionalWrites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("server honours If-None-Match on PutObject: %v", ok)
	if !ok {
		if err := st.(ExclusivePutter).PutIfAbsent(ctx, "x", strings.NewReader("1"), 1); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("PutIfAbsent without server support = %v, want ErrUnsupported", err)
		}
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := st.(ExclusivePutter).PutIfAbsent(ctx, "locks/worker.json", strings.NewReader("me"), 2)
			switch {
			case err == nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case !errors.Is(err, ErrExists):
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d creators won, want exactly 1", wins)
	}
}

// A missing bucket is misconfiguration, never "first run".
func TestS3MissingBucketIsNotNotFound(t *testing.T) {
	s3ForTest(t)
	cfg := S3ConfigFromEnv(os.Getenv)
	cfg.Bucket = "obsync-does-not-exist-" + fmt.Sprint(time.Now().UnixNano())
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(context.Background(), "manifests/1/latest")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get from a missing bucket = %v, want a configuration error", err)
	}
}

func TestS3Presign(t *testing.T) {
	s, st := s3ForTest(t)
	ctx := context.Background()
	if err := st.Put(ctx, "p", strings.NewReader("presigned"), 9); err != nil {
		t.Fatal(err)
	}
	url, err := s.Presign(ctx, st.(*prefixed).prefix+"p", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "presigned" {
		t.Fatalf("presigned GET: %d %q", resp.StatusCode, b)
	}
}

// prefixed isolates each test run inside a shared bucket.
type prefixed struct {
	inner  *S3
	prefix string
}

func (p *prefixed) Get(ctx context.Context, k string) (io.ReadCloser, error) {
	if err := ValidKey(k); err != nil {
		return nil, err
	}
	return p.inner.Get(ctx, p.prefix+k)
}
func (p *prefixed) Put(ctx context.Context, k string, r io.Reader, n int64) error {
	if err := ValidKey(k); err != nil {
		return err
	}
	return p.inner.Put(ctx, p.prefix+k, r, n)
}
func (p *prefixed) PutIfAbsent(ctx context.Context, k string, r io.Reader, n int64) error {
	if err := ValidKey(k); err != nil {
		return err
	}
	return p.inner.PutIfAbsent(ctx, p.prefix+k, r, n)
}
func (p *prefixed) Exists(ctx context.Context, k string) (bool, error) {
	if err := ValidKey(k); err != nil {
		return false, err
	}
	return p.inner.Exists(ctx, p.prefix+k)
}
func (p *prefixed) Stat(ctx context.Context, k string) (ObjectInfo, error) {
	if err := ValidKey(k); err != nil {
		return ObjectInfo{}, err
	}
	info, err := p.inner.Stat(ctx, p.prefix+k)
	info.Key = k
	return info, err
}
func (p *prefixed) Delete(ctx context.Context, k string) error {
	if err := ValidKey(k); err != nil {
		return err
	}
	return p.inner.Delete(ctx, p.prefix+k)
}
func (p *prefixed) GetRange(ctx context.Context, k string, off, n int64) (io.ReadCloser, error) {
	if err := ValidKey(k); err != nil {
		return nil, err
	}
	return p.inner.GetRange(ctx, p.prefix+k, off, n)
}
func (p *prefixed) List(ctx context.Context, pre string, fn func(ObjectInfo) error) error {
	return p.inner.List(ctx, p.prefix+pre, func(o ObjectInfo) error {
		o.Key = strings.TrimPrefix(o.Key, p.prefix)
		return fn(o)
	})
}
