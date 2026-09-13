package gc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/store"
)

var (
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
	now   = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	old   = now.Add(-72 * time.Hour)
)

const (
	hashKept   = "aaaa000000000000000000000000000000000000000000000000000000000001"
	hashOrphan = "bbbb000000000000000000000000000000000000000000000000000000000002"
	hashYoung  = "cccc000000000000000000000000000000000000000000000000000000000003"
)

func seed(t *testing.T) *store.Memory {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	put := func(key, body string, at time.Time) {
		st.Now = func() time.Time { return at }
		if err := st.Put(ctx, key, strings.NewReader(body), -1); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	m := &manifest.Manifest{SchemaVersion: manifest.SchemaVersion, CourseID: 1, RunID: "r1",
		Entries: []manifest.Entry{{Path: "a.pdf", State: manifest.StateStored, SHA256: hashKept}}}
	if err := manifest.Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	put(manifest.ManifestKey(1, "r1"), buf.String(), old)
	put(manifest.LatestKey(1), "r1", old)
	put(manifest.BlobKey(hashKept), "kept", old)
	put(manifest.BlobKey(hashOrphan), "orphan", old)
	put(manifest.BlobKey(hashYoung), "young", now.Add(-time.Hour))
	put("blobs/sha256/zz/notes.txt", "not a blob", old)
	return st
}

func exists(st store.Store, key string) bool {
	ok, _ := st.Exists(context.Background(), key)
	return ok
}

func TestDryRunDeletesNothing(t *testing.T) {
	st := seed(t)
	rep, err := Run(context.Background(), st, Options{MinAge: 24 * time.Hour, Now: now, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unreferenced != 2 || rep.TooYoung != 1 || rep.Deleted != 0 || rep.ReclaimableBytes != 6 {
		t.Fatalf("report = %+v", rep)
	}
	if !exists(st, manifest.BlobKey(hashOrphan)) {
		t.Fatal("dry run deleted")
	}
}

func TestApplyDeletesOnlyOldOrphans(t *testing.T) {
	st := seed(t)
	rep, err := Run(context.Background(), st, Options{MinAge: 24 * time.Hour, Now: now, Apply: true, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if exists(st, manifest.BlobKey(hashOrphan)) {
		t.Fatal("old orphan survived")
	}
	for _, k := range []string{manifest.BlobKey(hashKept), manifest.BlobKey(hashYoung), "blobs/sha256/zz/notes.txt"} {
		if !exists(st, k) {
			t.Fatalf("%s must not be deleted", k)
		}
	}
}

// If a manifest cannot be read, its blobs look unreferenced. Deleting them
// would destroy live content, so GC must stop.
func TestUnreadableManifestAbortsGC(t *testing.T) {
	st := seed(t)
	_ = st.Put(context.Background(), manifest.ManifestKey(2, "r9"), strings.NewReader("{corrupt"), -1)
	if _, err := Run(context.Background(), st, Options{MinAge: 0, Now: now, Apply: true, Log: quiet}); err == nil {
		t.Fatal("expected refusal")
	}
	if !exists(st, manifest.BlobKey(hashOrphan)) {
		t.Fatal("GC deleted despite an unreadable manifest")
	}
}
