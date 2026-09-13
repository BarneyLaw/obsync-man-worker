// Command obsync inspects the store.
//
// This exists because your blob keys are HASHES. No generic S3 browser will
// ever show you anything meaningful, because there are no filenames in the
// store. The manifest is the only human-readable index and reading it is this
// tool's job. Build it at the same time as the worker, not after: you cannot
// debug the differ without it.
//
//	obsync ls                      courses, counts, sizes
//	obsync ls <course>             logical tree with real filenames
//	obsync preview <course>        what a pull would do, with skip reasons
//	obsync log <course>            runs, with added/changed/removed counts
//	obsync diff <course> <a> <b>   what changed between two runs
//	obsync cat <course> <path>     resolve path to hash, stream the blob
//	obsync gc                      delete unreachable blobs (SEPARATE, never in the worker)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/store"
)

func main() {
	root := flag.String("fs-store", "./.obsync-store", "store root (dev)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	st := store.NewFS(*root)
	ctx := context.Background()

	var err error
	switch args[0] {
	case "ls":
		err = cmdLS(ctx, st, args[1:])
	case "preview":
		err = cmdPreview(ctx, st, args[1:])
	case "cat":
		err = cmdCat(ctx, st, args[1:])
	case "log", "diff", "gc":
		err = fmt.Errorf("%s: TODO, build order step 4/7", args[0])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: obsync [-fs-store DIR] <ls|preview|log|diff|cat|gc> [args]")
}

func cmdLS(ctx context.Context, st store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("TODO: enumerate courses by listing manifests/ prefixes")
	}
	m, err := loadLatest(ctx, st, args[0])
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "STATE\tSIZE\tPATH\n")
	for _, e := range m.Live() {
		fmt.Fprintf(w, "%s\t%d\t%s\n", e.State, e.Size, e.Path)
	}
	return w.Flush()
}

// cmdPreview is the CLI twin of the plugin's preview modal. Both are pure
// functions over the manifest, which is the payoff of cataloguing skipped files
// instead of dropping them.
func cmdPreview(ctx context.Context, st store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: obsync preview <course-id>")
	}
	m, err := loadLatest(ctx, st, args[0])
	if err != nil {
		return err
	}
	var stored, skipped, locked int
	var bytes int64
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, e := range m.Live() {
		switch e.State {
		case manifest.StateStored:
			stored++
			bytes += e.Size
		case manifest.StateSkipped:
			skipped++
			fmt.Fprintf(w, "skip\t%s\t%s\n", e.Path, e.Reason)
		case manifest.StateLocked:
			locked++
			fmt.Fprintf(w, "lock\t%s\t%s\n", e.Path, e.Reason)
		}
	}
	w.Flush()
	fmt.Printf("\n%d available (%d bytes), %d skipped, %d locked\n", stored, bytes, skipped, locked)
	return nil
}

func cmdCat(ctx context.Context, st store.Store, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: obsync cat <course-id> <path>")
	}
	m, err := loadLatest(ctx, st, args[0])
	if err != nil {
		return err
	}
	e, ok := m.ByPath()[args[1]]
	if !ok {
		return fmt.Errorf("no such path in manifest: %s", args[1])
	}
	if e.State != manifest.StateStored {
		return fmt.Errorf("%s is %s: %s", e.Path, e.State, e.Reason)
	}
	rc, err := st.Get(ctx, manifest.BlobKey(e.SHA256))
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(os.Stdout, rc)
	return err
}

func loadLatest(ctx context.Context, st store.Store, courseArg string) (*manifest.Manifest, error) {
	id, err := strconv.ParseInt(courseArg, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("course must be a numeric id for now: %w", err)
	}
	rc, err := st.Get(ctx, manifest.LatestKey(id))
	if err != nil {
		return nil, err
	}
	runID, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}
	mrc, err := st.Get(ctx, manifest.ManifestKey(id, string(runID)))
	if err != nil {
		return nil, err
	}
	defer mrc.Close()
	return manifest.Decode(mrc)
}
