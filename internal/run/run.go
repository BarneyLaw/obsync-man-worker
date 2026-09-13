// Package run sequences a single worker pass. It is the only package that
// cares about ordering, and it holds no state of its own: everything durable
// lives in the store, so a fresh pod is identical to a resumed one.
//
// It is also where the audit trail is written. The pure packages return every
// decision with its reason as data; this package logs each one.
package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/leifsen/obsync/internal/canvas"
	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/plan"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/portable"
	"github.com/leifsen/obsync/internal/scope"
	"github.com/leifsen/obsync/internal/store"
)

type Source interface {
	Courses(ctx context.Context) ([]canvas.Course, error)
	Files(ctx context.Context, courseID int64) ([]canvas.File, error)
	Folders(ctx context.Context, courseID int64) (map[int64]canvas.Folder, error)
	Open(ctx context.Context, f canvas.File) (io.ReadCloser, error)
}

// Guard is re-checked immediately before the commit. The worker lease
// implements it; a guard that fails means another writer may own latest.
type Guard interface {
	Verify(ctx context.Context) error
}

type Runner struct {
	Source Source
	Store  store.Store
	Policy *policy.Policy
	Log    *slog.Logger
	Now    func() time.Time

	// TempDir holds downloads while they are hashed, so a large file never
	// sits in memory. Empty means os.TempDir().
	TempDir string
	// Guard, when set, must pass before latest is published.
	Guard Guard
	// DryRun lists and plans, logs every decision, and writes nothing.
	DryRun bool
}

type Options struct {
	RunID string
	// Scope restricts downloads for a manual pull. Zero value is everything.
	Scope scope.Scope
}

type Stats struct {
	Seen         int   `json:"seen"`
	Fetched      int   `json:"fetched"`
	Deduplicated int   `json:"deduplicated"`
	Carried      int   `json:"carried"`
	Skipped      int   `json:"skipped"`
	Locked       int   `json:"locked"`
	Tombstoned   int   `json:"tombstoned"`
	OutOfScope   int   `json:"out_of_scope"`
	Failed       int   `json:"failed"`
	KeptPrevious int   `json:"kept_previous"`
	Duplicates   int   `json:"duplicates"`
	Renamed      int   `json:"renamed_unportable"`
	Bytes        int64 `json:"bytes"`
	// UnmatchedScope are scope patterns that selected nothing in this course.
	UnmatchedScope []string `json:"unmatched_scope,omitempty"`
	Committed      bool     `json:"committed"`
	ManifestKey    string   `json:"manifest_key,omitempty"`
}

// Course executes one course end to end.
//
//  1. read latest -> prev manifest (falling back to the newest intact one)
//  2. list Canvas files and folders, build portable paths
//  3. plan.Compute, log every decision
//  4. download to a temp file -> hash -> Put blob if absent
//  5. assemble and validate the manifest
//  6. Put manifests/<course>/<run>.json   write-once
//  7. verify the lease, Put manifests/<course>/latest   <-- THE COMMIT
//
// A crash, cancellation or lost lease anywhere before step 7 leaves orphan
// blobs and at most an unreferenced manifest, both invisible to every
// consumer, because latest still points at the previous run.
func (r *Runner) Course(ctx context.Context, c canvas.Course, opt Options) (Stats, error) {
	log := r.Log.With("course_id", c.ID, "course_code", c.Code)
	start := time.Now()
	var st Stats
	log.Info("course.start", "course_name", c.Name, "dry_run", r.DryRun, "scope", opt.Scope.Patterns())

	prev, err := r.readPrevious(ctx, log, c.ID)
	if err != nil {
		return st, fmt.Errorf("read previous manifest: %w", err)
	}

	raw, err := r.Source.Files(ctx, c.ID)
	if err != nil {
		return st, fmt.Errorf("list files: %w", err)
	}
	folders, err := r.Source.Folders(ctx, c.ID)
	if err != nil {
		return st, fmt.Errorf("list folders: %w", err)
	}
	st.Seen = len(raw)

	files, byID := toPlanFiles(log, raw, folders, &st)

	rulesHash := r.Policy.Hash()
	if prev != nil && prev.RulesHash != rulesHash {
		log.Info("course.rules_changed", "prev_rules_hash", prev.RulesHash, "rules_hash", rulesHash)
	}

	p := plan.Compute(plan.Input{
		CourseID: c.ID, Prev: prev, Files: files, Policy: r.Policy,
		Scope: opt.Scope, Now: r.Now(),
	})
	st.Carried, st.Skipped, st.Locked = len(p.Carry), len(p.Skipped), len(p.Locked)
	st.Tombstoned, st.OutOfScope, st.Duplicates = len(p.Tombstone), len(p.OutOfScope), len(p.Duplicates)
	for _, id := range p.Duplicates {
		log.Warn("canvas.duplicate_listing", "canvas_id", id, "effect", "kept the most recently updated copy")
	}
	log.Info("plan.computed", "fetch", len(p.Fetch), "carry", len(p.Carry), "skip", len(p.Skipped),
		"locked", len(p.Locked), "tombstone", len(p.Tombstone), "out_of_scope", len(p.OutOfScope))
	logPlan(log, p)

	if !opt.Scope.All() {
		st.UnmatchedScope = opt.Scope.Unmatched(p.Paths())
		for _, u := range st.UnmatchedScope {
			log.Warn("scope.pattern_unmatched", "pattern", u)
		}
	}

	if r.DryRun {
		log.Info("course.dry_run_complete", "would_fetch", len(p.Fetch),
			"would_tombstone", len(p.Tombstone), "duration_ms", time.Since(start).Milliseconds())
		return st, nil
	}

	prevRunID := ""
	if prev != nil {
		prevRunID = prev.RunID
	}

	entries := make([]manifest.Entry, 0, len(files)+len(p.Tombstone))
	for _, group := range [][]manifest.Entry{p.Carry, p.Skipped, p.Locked, p.Tombstone, p.OutOfScope} {
		entries = append(entries, group...)
	}

	for _, f := range p.Fetch {
		if err := ctx.Err(); err != nil {
			return st, abort(log, "interrupted", err)
		}
		e, n, dedup, err := r.fetch(ctx, log, byID[f.CanvasID], f)
		if err != nil {
			if ctx.Err() != nil {
				return st, abort(log, "interrupted", ctx.Err())
			}
			st.Failed++
			if f.Prev != nil && f.Prev.State == manifest.StateStored {
				// Never let a transient failure hide a file the consumer has.
				e = *f.Prev
				e.Reason = fmt.Sprintf("refresh failed in run %s (%v); serving the version from run %s or earlier",
					opt.RunID, err, prevRunID)
				st.KeptPrevious++
				log.Warn("file.fetch_failed", "path", f.Path, "canvas_id", f.CanvasID, "err", err,
					"fallback", "kept previous version", "sha256", e.SHA256)
			} else {
				e = failedEntry(f.File, err)
				log.Warn("file.fetch_failed", "path", f.Path, "canvas_id", f.CanvasID, "err", err,
					"fallback", "none, catalogued as failed")
			}
		} else {
			st.Fetched++
			st.Bytes += n
			if dedup {
				st.Deduplicated++
			}
		}
		entries = append(entries, e)
	}

	m := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		CourseID:      c.ID,
		CourseName:    c.Name,
		RunID:         opt.RunID,
		PrevRunID:     prevRunID,
		GeneratedAt:   r.Now().UTC(),
		RulesHash:     rulesHash,
		Entries:       entries,
	}
	var buf bytes.Buffer
	if err := manifest.Encode(&buf, m); err != nil {
		log.Error("manifest.invalid", "err", err)
		return st, fmt.Errorf("assembled manifest is invalid, nothing published: %w", err)
	}
	key := manifest.ManifestKey(c.ID, opt.RunID)
	if _, err := store.PutOnce(ctx, r.Store, key, bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		return st, abort(log, "manifest write failed", err)
	}
	st.ManifestKey = key
	log.Info("manifest.written", "key", key, "entries", len(m.Entries), "bytes", buf.Len())

	if err := ctx.Err(); err != nil {
		return st, abort(log, "interrupted", err)
	}
	if r.Guard != nil {
		if err := r.Guard.Verify(ctx); err != nil {
			return st, abort(log, "lease lost", err)
		}
	}

	// THE COMMIT. One of two mutable keys in the store, and the only one
	// consumers read.
	if err := r.Store.Put(ctx, manifest.LatestKey(c.ID), strings.NewReader(opt.RunID), int64(len(opt.RunID))); err != nil {
		return st, abort(log, "publishing latest failed", err)
	}
	st.Committed = true
	changes := changeCounts(manifest.Diff(prev, m))
	log.Info("latest.published", "run_id", opt.RunID, "prev_run_id", prevRunID, "manifest_key", key,
		"added", changes[manifest.ChangeAdded], "removed", changes[manifest.ChangeRemoved],
		"content_changed", changes[manifest.ChangeContent], "state_changed", changes[manifest.ChangeState])

	log.Info("course.done", "fetched", st.Fetched, "deduplicated", st.Deduplicated, "carried", st.Carried,
		"skipped", st.Skipped, "locked", st.Locked, "tombstoned", st.Tombstoned,
		"out_of_scope", st.OutOfScope, "failed", st.Failed, "kept_previous", st.KeptPrevious,
		"bytes", st.Bytes, "duration_ms", time.Since(start).Milliseconds())
	return st, nil
}

func abort(log *slog.Logger, reason string, err error) error {
	log.Error("course.aborted_before_commit", "reason", reason, "err", err,
		"effect", "latest not published; previous manifest stays live")
	return fmt.Errorf("%s before commit, previous manifest stays live: %w", reason, err)
}

// fetch streams a download through the hasher into a temp file, then stores
// the blob only if it is not already there. Content addressing makes dedup
// free: the same PDF posted in two courses is one blob.
func (r *Runner) fetch(ctx context.Context, log *slog.Logger, cf canvas.File, f plan.Fetch) (manifest.Entry, int64, bool, error) {
	start := time.Now()
	log.Info("file.fetch_start", "path", f.Path, "canvas_id", f.CanvasID, "expected_bytes", f.Size, "why", f.Why)

	rc, err := r.Source.Open(ctx, cf)
	if err != nil {
		return manifest.Entry{}, 0, false, err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(r.TempDir, "obsync-download-*")
	if err != nil {
		return manifest.Entry{}, 0, false, fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), rc)
	if err != nil {
		return manifest.Entry{}, n, false, fmt.Errorf("download interrupted after %d bytes: %w", n, err)
	}
	// A short read that ends cleanly would otherwise become the authoritative
	// content for this path until Canvas metadata next changes.
	if n != f.Size {
		return manifest.Entry{}, n, false, fmt.Errorf("size mismatch: Canvas reports %d bytes, received %d", f.Size, n)
	}
	hash := hex.EncodeToString(h.Sum(nil))
	key := manifest.BlobKey(hash)

	exists, err := r.Store.Exists(ctx, key)
	if err != nil {
		return manifest.Entry{}, n, false, fmt.Errorf("check blob: %w", err)
	}
	if !exists {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return manifest.Entry{}, n, false, err
		}
		if err := r.Store.Put(ctx, key, tmp, n); err != nil {
			return manifest.Entry{}, n, false, fmt.Errorf("store blob: %w", err)
		}
	}
	log.Info("file.fetched", "path", f.Path, "canvas_id", f.CanvasID, "bytes", n, "sha256", hash,
		"blob_key", key, "deduplicated", exists, "duration_ms", time.Since(start).Milliseconds())

	return manifest.Entry{
		Path: f.Path, State: manifest.StateStored,
		Size: n, MIME: f.MIME,
		CanvasID: f.CanvasID, CanvasUUID: f.CanvasUUID,
		UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
		SHA256: hash,
		Reason: f.Note,
	}, n, exists, nil
}

func failedEntry(f plan.File, err error) manifest.Entry {
	return manifest.Entry{
		Path: f.Path, State: manifest.StateFailed,
		Size: f.Size, MIME: f.MIME,
		CanvasID: f.CanvasID, CanvasUUID: f.CanvasUUID,
		UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
		Reason: err.Error(),
	}
}

// toPlanFiles builds portable paths. Nothing is dropped: a name that cannot be
// made portable is catalogued under a stand-in name, and a file in a folder
// the listing did not return is placed under a marker directory.
func toPlanFiles(log *slog.Logger, raw []canvas.File, folders map[int64]canvas.Folder, st *Stats) ([]plan.File, map[int64]canvas.File) {
	out := make([]plan.File, 0, len(raw))
	byID := make(map[int64]canvas.File, len(raw))
	for _, f := range raw {
		dir := ""
		if f.FolderID != 0 {
			if fo, ok := folders[f.FolderID]; ok {
				dir = fo.FullName
			} else {
				dir = fmt.Sprintf("_unlisted-folder-%d", f.FolderID)
				log.Warn("file.folder_unlisted", "canvas_id", f.ID, "folder_id", f.FolderID, "placed_under", dir)
			}
		}
		p, note := resolvePath(dir, f.DisplayName, f.ID)
		if note != "" {
			st.Renamed++
			log.Warn("file.name_not_portable", "canvas_id", f.ID, "display_name", f.DisplayName,
				"folder", dir, "path", p, "note", note)
		}
		out = append(out, plan.File{
			Path: p, CanvasID: f.ID, CanvasUUID: f.UUID,
			Size: f.Size, MIME: f.ContentType,
			UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
			Locked: f.LockedForUser || f.Locked, UnlockAt: f.UnlockAt,
			Note: note,
		})
		// Keyed by Canvas id, never by path: the planner may rename paths to
		// resolve collisions, and a path-keyed lookup then finds nothing.
		byID[f.ID] = f
	}
	return out, byID
}

func resolvePath(dir, name string, id int64) (string, string) {
	joined := joinPath(dir, name)
	p, err := portable.Path(joined)
	if err == nil {
		return p, ""
	}
	fb := portable.Fallback(name, id)
	if p2, err2 := portable.Path(joinPath(dir, fb)); err2 == nil {
		return p2, fmt.Sprintf("Canvas name %q could not be made portable (%v); stored as %q", name, err, fb)
	}
	return fb, fmt.Sprintf("Canvas path %q could not be made portable (%v); stored at the course root as %q", joined, err, fb)
}

func logPlan(log *slog.Logger, p plan.Plan) {
	for _, f := range p.Fetch {
		log.Info("plan.fetch", "path", f.Path, "canvas_id", f.CanvasID, "size", f.Size, "why", f.Why)
	}
	for _, e := range p.Skipped {
		log.Info("plan.skip", "path", e.Path, "canvas_id", e.CanvasID, "size", e.Size,
			"rule", e.RuleName, "reason", e.Reason)
	}
	for _, e := range p.Locked {
		log.Info("plan.locked", "path", e.Path, "canvas_id", e.CanvasID, "unlock_at", e.UnlockAt)
	}
	for _, e := range p.Tombstone {
		log.Info("plan.tombstone", "path", e.Path, "canvas_id", e.CanvasID, "reason", e.Reason)
	}
	for _, e := range p.OutOfScope {
		effect := "kept previous entry unchanged"
		if e.RuleName == plan.ScopeRule {
			effect = "catalogued, deferred to the next full pull"
		}
		log.Info("plan.out_of_scope", "path", e.Path, "canvas_id", e.CanvasID, "state", e.State, "effect", effect)
	}
	for _, e := range p.Carry {
		log.Debug("plan.carry", "path", e.Path, "state", e.State)
	}
}

func changeCounts(cs []manifest.Change) map[manifest.ChangeKind]int {
	out := map[manifest.ChangeKind]int{}
	for _, c := range cs {
		out[c.Kind]++
	}
	return out
}

// readPrevious resolves latest to a manifest.
//
// If latest is missing the course is new. If latest points at a manifest
// that is gone, the newest intact manifest is used instead: treating that as
// a first run would silently drop tombstones for everything deleted from
// Canvas in between. A manifest that exists but is corrupt is an error, never
// a guess.
func (r *Runner) readPrevious(ctx context.Context, log *slog.Logger, courseID int64) (*manifest.Manifest, error) {
	rc, err := r.Store.Get(ctx, manifest.LatestKey(courseID))
	if errors.Is(err, store.ErrNotFound) {
		log.Info("course.first_run", "reason", "no latest pointer")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, 1024))
	rc.Close()
	if err != nil {
		return nil, err
	}

	if runID := strings.TrimSpace(string(b)); runID != "" {
		m, err := r.loadManifest(ctx, courseID, runID)
		if err == nil {
			log.Info("course.previous_manifest", "prev_run_id", m.RunID, "entries", len(m.Entries),
				"rules_hash", m.RulesHash)
			return m, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		log.Warn("latest.dangling", "points_at", runID)
	} else {
		log.Warn("latest.empty")
	}

	var keys []string
	if err := r.Store.List(ctx, fmt.Sprintf("manifests/%d/", courseID), func(o store.ObjectInfo) error {
		if strings.HasSuffix(o.Key, ".json") {
			keys = append(keys, o.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, k := range keys {
		m, err := r.loadManifest(ctx, courseID, strings.TrimSuffix(path.Base(k), ".json"))
		if err != nil {
			log.Warn("manifest.unusable", "key", k, "err", err)
			continue
		}
		log.Warn("course.previous_manifest_recovered", "prev_run_id", m.RunID, "entries", len(m.Entries))
		return m, nil
	}
	log.Warn("course.first_run", "reason", "latest unusable and no intact manifest found")
	return nil, nil
}

func (r *Runner) loadManifest(ctx context.Context, courseID int64, runID string) (*manifest.Manifest, error) {
	key := manifest.ManifestKey(courseID, runID)
	rc, err := r.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	m, err := manifest.Decode(rc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	if m.CourseID != courseID || m.RunID != runID {
		return nil, fmt.Errorf("%s: claims course %d run %s", key, m.CourseID, m.RunID)
	}
	return m, nil
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
