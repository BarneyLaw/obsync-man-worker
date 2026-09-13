// Package gc deletes blobs no manifest references.
//
// Separate from the worker, always. It reads every manifest (not only the
// latest ones, so `obsync log` and `diff` keep working over history), lists
// blobs/, and deletes the difference, skipping anything younger than MinAge so
// it cannot race a run that has written blobs but not yet its manifest.
//
// MinAge alone does not cover one race: a run can find an old orphan blob via
// Exists, skip re-uploading it, and reference it in a manifest written after
// GC listed manifests. Callers that delete must hold the worker lease.
package gc

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/store"
)

type Options struct {
	MinAge time.Duration
	Now    time.Time
	// Apply deletes. Without it GC only reports.
	Apply bool
	Log   *slog.Logger
}

type Report struct {
	Manifests        int   `json:"manifests"`
	ReferencedBlobs  int   `json:"referenced_blobs"`
	Blobs            int   `json:"blobs"`
	Unreferenced     int   `json:"unreferenced"`
	TooYoung         int   `json:"too_young"`
	Deleted          int   `json:"deleted"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	FreedBytes       int64 `json:"freed_bytes"`
}

func Run(ctx context.Context, st store.Store, opt Options) (Report, error) {
	var rep Report
	log := opt.Log
	log.Info("gc.start", "apply", opt.Apply, "min_age", opt.MinAge.String())

	var manifestKeys []string
	if err := st.List(ctx, "manifests/", func(o store.ObjectInfo) error {
		if strings.HasSuffix(o.Key, ".json") {
			manifestKeys = append(manifestKeys, o.Key)
		}
		return nil
	}); err != nil {
		return rep, err
	}

	referenced := map[string]bool{}
	for _, k := range manifestKeys {
		m, err := load(ctx, st, k)
		if err != nil {
			log.Error("gc.aborted", "reason", "unreadable manifest", "key", k, "err", err)
			return rep, fmt.Errorf("gc: refusing to run, manifest %s is unreadable and its blobs would look unreferenced: %w", k, err)
		}
		rep.Manifests++
		for _, e := range m.Entries {
			if e.SHA256 != "" {
				referenced[e.SHA256] = true
			}
		}
	}
	rep.ReferencedBlobs = len(referenced)
	log.Info("gc.referenced", "manifests", rep.Manifests, "referenced_blobs", rep.ReferencedBlobs)

	var candidates []store.ObjectInfo
	if err := st.List(ctx, "blobs/sha256/", func(o store.ObjectInfo) error {
		rep.Blobs++
		hash := path.Base(o.Key)
		if manifest.BlobKey(hash) != o.Key {
			// Never delete something whose name we do not understand.
			log.Warn("gc.unexpected_key", "key", o.Key)
			return nil
		}
		if referenced[hash] {
			return nil
		}
		rep.Unreferenced++
		if age := opt.Now.Sub(o.Modified); age < opt.MinAge {
			rep.TooYoung++
			log.Info("gc.kept_young", "key", o.Key, "age", age.Round(time.Second).String())
			return nil
		}
		candidates = append(candidates, o)
		return nil
	}); err != nil {
		return rep, err
	}

	for _, o := range candidates {
		rep.ReclaimableBytes += o.Size
		if !opt.Apply {
			log.Info("gc.would_delete", "key", o.Key, "bytes", o.Size, "modified", o.Modified)
			continue
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if err := st.Delete(ctx, o.Key); err != nil {
			return rep, fmt.Errorf("gc: delete %s: %w", o.Key, err)
		}
		rep.Deleted++
		rep.FreedBytes += o.Size
		log.Info("gc.deleted", "key", o.Key, "bytes", o.Size, "modified", o.Modified)
	}

	log.Info("gc.done", "apply", opt.Apply, "blobs", rep.Blobs, "unreferenced", rep.Unreferenced,
		"too_young", rep.TooYoung, "deleted", rep.Deleted, "reclaimable_bytes", rep.ReclaimableBytes,
		"freed_bytes", rep.FreedBytes)
	return rep, nil
}

func load(ctx context.Context, st store.Store, key string) (*manifest.Manifest, error) {
	rc, err := st.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return manifest.Decode(rc)
}
