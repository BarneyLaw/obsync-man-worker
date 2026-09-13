package run

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/leifsen/obsync/internal/lease"
	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/store"
)

// A full worker pass against a real S3-compatible server: the commit sequence,
// write-once manifests, the lease and tombstones, over the wire. Runs when
// GARAGE_* is set (scripts/garage-dev.sh up); fails instead of skipping under
// OBSYNC_REQUIRE_S3=1.
func TestS3CourseEndToEnd(t *testing.T) {
	cfg := store.S3ConfigFromEnv(os.Getenv)
	if cfg.Endpoint == "" {
		if os.Getenv("OBSYNC_REQUIRE_S3") == "1" {
			t.Fatal("OBSYNC_REQUIRE_S3=1 but GARAGE_ENDPOINT is not set")
		}
		t.Skip("set GARAGE_* to run S3 integration tests (scripts/garage-dev.sh up)")
	}
	s3, err := store.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	st := store.WithLogging(s3, log)

	// A course id no real run will use, so tests share a bucket safely.
	c := course
	c.ID = 900_000_000 + time.Now().UnixNano()%99_999_999
	t.Cleanup(func() { cleanupCourse(ctx, s3, c.ID) })

	src := newSource()
	src.add(1, 2, "slides.pdf", "week one slides")
	src.add(2, 2, "notes.pdf", "notes to be deleted")
	src.add(3, 1, "Q1: what.pdf", "first")
	src.add(4, 1, "Q1? what.pdf", "second")
	big := strings.Repeat("0123456789abcdef", 1<<18) // 4 MiB, above the plugin's chunk threshold
	src.add(5, 3, "recording-transcript.txt", big)

	// The lease is held for real, so the pre-commit verify runs against S3.
	leaseStore := &keyPrefix{Store: st, prefix: fmt.Sprintf("it/lease-%d/", c.ID)}
	l, err := lease.Acquire(ctx, leaseStore, lease.Record{Holder: "it", RunID: "r1", Trigger: "test"},
		lease.Options{TTL: time.Minute, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release(context.Background()) })

	r := runner(src, st, nil)
	r.Log = log
	r.Guard = l
	s, err := r.Course(ctx, c, Options{RunID: "r1"})
	if err != nil {
		t.Fatalf("first pass: %v\n%s", err, logs.String())
	}
	if s.Fetched != 5 || s.Failed != 0 || !s.Committed {
		t.Fatalf("first pass stats = %+v", s)
	}
	m := readLatest(t, s3, c.ID)
	if m.RunID != "r1" || len(m.Entries) != 5 {
		t.Fatalf("manifest = %s with %d entries", m.RunID, len(m.Entries))
	}
	e := m.ByPath()["Week 2/recording-transcript.txt"]
	rc, err := s3.GetRange(ctx, manifest.BlobKey(e.SHA256), 1<<20, 16)
	if err != nil {
		t.Fatal(err)
	}
	chunk, _ := io.ReadAll(rc)
	rc.Close()
	if string(chunk) != "0123456789abcdef" {
		t.Fatalf("range read of a stored blob = %q", chunk)
	}

	// Second pass: one file edited, one deleted in Canvas.
	src.files[0].UpdatedAt = now
	src.content["u1"] = "week one slides, revised"
	src.files[0].Size = int64(len(src.content["u1"]))
	src.files = append(src.files[:1], src.files[2:]...)
	s, err = r.Course(ctx, c, Options{RunID: "r2"})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if s.Fetched != 1 || s.Tombstoned != 1 || s.Carried != 3 {
		t.Fatalf("second pass stats = %+v", s)
	}
	m = readLatest(t, s3, c.ID)
	if m.PrevRunID != "r1" || m.ByPath()["Week 1/notes.pdf"].State != manifest.StateDeleted {
		t.Fatalf("second manifest: prev=%s notes=%+v", m.PrevRunID, m.ByPath()["Week 1/notes.pdf"])
	}

	// Manifests are write-once on S3 too.
	if _, err := r.Course(ctx, c, Options{RunID: "r2"}); err == nil {
		t.Fatal("reusing a run id overwrote its manifest")
	}
}

func readLatest(t *testing.T, st store.Store, courseID int64) *manifest.Manifest {
	t.Helper()
	ctx := context.Background()
	rc, err := st.Get(ctx, manifest.LatestKey(courseID))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := io.ReadAll(rc)
	rc.Close()
	mrc, err := st.Get(ctx, manifest.ManifestKey(courseID, string(id)))
	if err != nil {
		t.Fatal(err)
	}
	defer mrc.Close()
	m, err := manifest.Decode(mrc)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// cleanupCourse removes a test course's manifests and the blobs they
// reference. Blobs are content-addressed and these contents are unique to the
// test, so nothing real is shared.
func cleanupCourse(ctx context.Context, st store.Store, courseID int64) {
	var keys []string
	_ = st.List(ctx, fmt.Sprintf("manifests/%d/", courseID), func(o store.ObjectInfo) error {
		keys = append(keys, o.Key)
		if strings.HasSuffix(o.Key, ".json") {
			if rc, err := st.Get(ctx, o.Key); err == nil {
				if m, err := manifest.Decode(rc); err == nil {
					for _, e := range m.Entries {
						if e.SHA256 != "" {
							keys = append(keys, manifest.BlobKey(e.SHA256))
						}
					}
				}
				rc.Close()
			}
		}
		return nil
	})
	_ = st.List(ctx, fmt.Sprintf("it/lease-%d/", courseID), func(o store.ObjectInfo) error {
		keys = append(keys, o.Key)
		return nil
	})
	for _, k := range keys {
		_ = st.Delete(ctx, k)
	}
}

// keyPrefix keeps the test's lease away from a real worker's lease in a shared
// bucket, forwarding the exclusive create the lease depends on.
type keyPrefix struct {
	store.Store
	prefix string
}

func (k *keyPrefix) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return k.Store.Get(ctx, k.prefix+key)
}
func (k *keyPrefix) Put(ctx context.Context, key string, r io.Reader, n int64) error {
	return k.Store.Put(ctx, k.prefix+key, r, n)
}
func (k *keyPrefix) PutIfAbsent(ctx context.Context, key string, r io.Reader, n int64) error {
	return k.Store.(store.ExclusivePutter).PutIfAbsent(ctx, k.prefix+key, r, n)
}
func (k *keyPrefix) Exists(ctx context.Context, key string) (bool, error) {
	return k.Store.Exists(ctx, k.prefix+key)
}
func (k *keyPrefix) Delete(ctx context.Context, key string) error {
	return k.Store.Delete(ctx, k.prefix+key)
}
