// Package run sequences a single worker pass. It is the only package that
// cares about ordering, and it holds no state of its own: everything durable
// lives in the store, so a fresh pod is identical to a resumed one.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/leifsen/obsync/internal/canvas"
	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/plan"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/portable"
	"github.com/leifsen/obsync/internal/store"
)

type Source interface {
	Courses(ctx context.Context) ([]canvas.Course, error)
	Files(ctx context.Context, courseID int64) ([]canvas.File, error)
	Folders(ctx context.Context, courseID int64) (map[int64]canvas.Folder, error)
	Open(ctx context.Context, f canvas.File) (io.ReadCloser, error)
}

type Runner struct {
	Source Source
	Store  store.Store
	Policy *policy.Policy
	Log    *slog.Logger
	Now    func() time.Time

	// Concurrency for blob downloads. These hit presigned storage URLs, not the
	// throttled API host, so 8+ is fine.
	DownloadConcurrency int
}

type Stats struct {
	Seen, Fetched, Skipped, Locked, Tombstoned, Failed int
	Bytes                                              int64
}

// Course executes one course end to end.
//
//	1. read latest -> prev manifest
//	2. list Canvas files, build portable paths
//	3. plan.Compute
//	4. download -> hash -> Put blob if absent
//	5. assemble manifest
//	6. Put manifests/<course>/<run>.json
//	7. Put manifests/<course>/latest   <-- THE COMMIT
//
// A crash anywhere before step 7 leaves orphan blobs and an unreferenced
// manifest, both invisible to every consumer, because latest still points at
// the previous run.
func (r *Runner) Course(ctx context.Context, c canvas.Course, runID string) (Stats, error) {
	var st Stats

	prev, err := r.readLatest(ctx, c.ID)
	if err != nil {
		return st, err
	}

	raw, err := r.Source.Files(ctx, c.ID)
	if err != nil {
		return st, err
	}
	folders, err := r.Source.Folders(ctx, c.ID)
	if err != nil {
		return st, err
	}
	st.Seen = len(raw)

	files, byPath := r.toPlanFiles(raw, folders)

	// TODO(step 7): drain the request bucket and fold user overrides in here.
	p := plan.Compute(plan.Input{
		Prev: prev, Files: files, Policy: r.Policy, Now: r.Now(),
	})

	entries := make([]manifest.Entry, 0, len(files))
	entries = append(entries, p.Carry...)
	entries = append(entries, p.Skipped...)
	entries = append(entries, p.Locked...)
	entries = append(entries, p.Tombstone...)
	st.Skipped, st.Locked, st.Tombstoned = len(p.Skipped), len(p.Locked), len(p.Tombstone)

	// TODO(step 2): bound this with a semaphore of DownloadConcurrency and run
	// it in an errgroup. Sequential is correct, just slow, so ship it first.
	for _, f := range p.Fetch {
		e, n, err := r.fetch(ctx, byPath[f.Path], f)
		if err != nil {
			r.Log.Warn("fetch failed", "path", f.Path, "err", err)
			e = manifest.Entry{
				Path: f.Path, State: manifest.StateFailed,
				Size: f.Size, MIME: f.MIME, CanvasID: f.CanvasID,
				UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
				Reason: err.Error(),
			}
			st.Failed++
		} else {
			st.Fetched++
			st.Bytes += n
		}
		entries = append(entries, e)
	}

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		CourseID:      c.ID,
		CourseName:    c.Name,
		RunID:         runID,
		GeneratedAt:   r.Now(),
		RulesHash:     r.Policy.Hash(),
		Entries:       entries,
	}
	if prev != nil {
		m.PrevRunID = prev.RunID
	}

	if err := r.putManifest(ctx, m); err != nil {
		return st, err
	}
	// THE COMMIT. Only mutable key in the store.
	if err := r.Store.Put(ctx, manifest.LatestKey(c.ID),
		stringReader(runID), int64(len(runID))); err != nil {
		return st, fmt.Errorf("publish latest: %w", err)
	}
	return st, nil
}

// fetch downloads, hashes, and stores the blob only if it is not already there.
// Content addressing makes dedup free: the same PDF posted in two courses is
// one blob.
func (r *Runner) fetch(ctx context.Context, cf canvas.File, f plan.File) (manifest.Entry, int64, error) {
	rc, err := r.Source.Open(ctx, cf)
	if err != nil {
		return manifest.Entry{}, 0, err
	}
	defer rc.Close()

	// TODO(step 2): stream to a temp file instead of memory once you care about
	// files larger than a few hundred MB on the worker side.
	buf, err := io.ReadAll(rc)
	if err != nil {
		return manifest.Entry{}, 0, err
	}
	sum := sha256.Sum256(buf)
	hash := hex.EncodeToString(sum[:])
	key := manifest.BlobKey(hash)

	exists, err := r.Store.Exists(ctx, key)
	if err != nil {
		return manifest.Entry{}, 0, err
	}
	if !exists {
		if err := r.Store.Put(ctx, key, bytesReader(buf), int64(len(buf))); err != nil {
			return manifest.Entry{}, 0, err
		}
	}
	return manifest.Entry{
		Path: f.Path, State: manifest.StateStored,
		Size: int64(len(buf)), MIME: f.MIME,
		CanvasID: f.CanvasID, CanvasUUID: f.CanvasUUID,
		UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
		SHA256: hash,
	}, int64(len(buf)), nil
}

func (r *Runner) toPlanFiles(raw []canvas.File, folders map[int64]canvas.Folder) ([]plan.File, map[string]canvas.File) {
	out := make([]plan.File, 0, len(raw))
	by := make(map[string]canvas.File, len(raw))
	for _, f := range raw {
		dir := ""
		if fo, ok := folders[f.FolderID]; ok {
			dir = fo.FullName
		}
		p, err := portable.Path(joinPath(dir, f.DisplayName))
		if err != nil {
			r.Log.Warn("unusable filename, dropped", "canvas_id", f.ID, "name", f.DisplayName, "err", err)
			continue
		}
		pf := plan.File{
			Path: p, CanvasID: f.ID, CanvasUUID: f.UUID,
			Size: f.Size, MIME: f.ContentType,
			UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
			Locked: f.LockedForUser || f.Locked, UnlockAt: f.UnlockAt,
		}
		out = append(out, pf)
		by[p] = f
	}
	return out, by
}

func (r *Runner) readLatest(ctx context.Context, courseID int64) (*manifest.Manifest, error) {
	rc, err := r.Store.Get(ctx, manifest.LatestKey(courseID))
	if err == store.ErrNotFound {
		return nil, nil // first run
	}
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}
	mrc, err := r.Store.Get(ctx, manifest.ManifestKey(courseID, string(b)))
	if err != nil {
		// latest points at a manifest that is not there. Treat as first run
		// rather than failing: the worst case is one redundant full pull.
		r.Log.Warn("latest points at a missing manifest, treating as first run",
			"course", courseID, "run", string(b))
		return nil, nil
	}
	defer mrc.Close()
	return manifest.Decode(mrc)
}

func (r *Runner) putManifest(ctx context.Context, m *manifest.Manifest) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(manifest.Encode(pw, m)) }()
	return r.Store.Put(ctx, manifest.ManifestKey(m.CourseID, m.RunID), pr, -1)
}

// joinPath strips Canvas's synthetic root. Folder.full_name comes back as
// "course files/Week 1", and nobody wants a "course files" directory in their
// vault.
func joinPath(dir, name string) string {
	dir = strings.TrimPrefix(dir, "course files/")
	if dir == "course files" {
		dir = ""
	}
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
