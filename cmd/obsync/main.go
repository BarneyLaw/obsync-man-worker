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
//	obsync preview <course>        what a consumer gets, and everything withheld with its reason
//	obsync log <course>            runs, with added/changed/removed counts
//	obsync diff <course> <a> <b>   what changed between two runs ("latest" works as a run)
//	obsync cat <course> <path>     resolve path to hash, stream the blob, verify the hash
//	obsync gc [-apply]             delete unreachable blobs (SEPARATE, never in the worker)
//	obsync serve [-addr]           read-only HTTP view of the store for the plugin (dev)
//
// Reads print to stdout. Logs are JSON on stderr; gc and serve log every
// deletion and every request.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/leifsen/obsync/internal/gc"
	"github.com/leifsen/obsync/internal/lease"
	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/obs"
	"github.com/leifsen/obsync/internal/store"
)

func main() { os.Exit(realMain()) }

func realMain() int {
	root := flag.String("fs-store", "./.obsync-store", "store root (dev)")
	level := flag.String("log-level", "info", "debug, info, warn or error")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		return 2
	}
	log, err := obs.NewLogger(os.Stderr, *level, "obsync")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	log = log.With("command", args[0])
	if info, err := os.Stat(*root); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: store %s is not a directory (run the worker first, or pass -fs-store)\n", *root)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fsStore := store.NewFS(*root)
	st := store.WithLogging(fsStore, log)

	switch args[0] {
	case "ls":
		err = cmdLS(ctx, st, args[1:])
	case "preview":
		err = cmdPreview(ctx, st, args[1:])
	case "log":
		err = cmdLog(ctx, st, args[1:])
	case "diff":
		err = cmdDiff(ctx, st, args[1:])
	case "cat":
		err = cmdCat(ctx, st, args[1:])
	case "gc":
		err = cmdGC(ctx, st, log, args[1:])
	case "serve":
		err = cmdServe(ctx, fsStore, log, args[1:])
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: obsync [-fs-store DIR] [-log-level LEVEL] <ls|preview|log|diff|cat|gc|serve> [args]")
}

func cmdLS(ctx context.Context, st store.Store, args []string) error {
	if len(args) == 0 {
		return listCourses(ctx, st)
	}
	id, err := courseID(args[0])
	if err != nil {
		return err
	}
	m, err := loadRun(ctx, st, id, "latest")
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "STATE\tSIZE\tPATH\tNOTE\n")
	for _, e := range m.Live() {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.State, humanBytes(e.Size), e.Path, e.Reason)
	}
	return w.Flush()
}

func listCourses(ctx context.Context, st store.Store) error {
	var ids []int64
	if err := st.List(ctx, "manifests/", func(o store.ObjectInfo) error {
		parts := strings.Split(o.Key, "/")
		if len(parts) == 3 && parts[2] == "latest" {
			if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("store is empty: no course has a published manifest yet")
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "COURSE\tNAME\tRUN\tGENERATED\tSTORED\tSKIPPED\tLOCKED\tFAILED\tDELETED\tSIZE\n")
	for _, id := range ids {
		m, err := loadRun(ctx, st, id, "latest")
		if err != nil {
			fmt.Fprintf(w, "%d\t<unreadable: %v>\n", id, err)
			continue
		}
		c, bytes := m.Counts()
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n", id, m.CourseName, m.RunID,
			m.GeneratedAt.Local().Format("2006-01-02 15:04"),
			c[manifest.StateStored], c[manifest.StateSkipped], c[manifest.StateLocked],
			c[manifest.StateFailed], c[manifest.StateDeleted], humanBytes(bytes))
	}
	return w.Flush()
}

// cmdPreview is the CLI twin of the plugin's preview modal. Both are pure
// functions over the manifest, which is the payoff of cataloguing skipped files
// instead of dropping them. Nothing withheld is left out of this output.
func cmdPreview(ctx context.Context, st store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: obsync preview <course-id>")
	}
	id, err := courseID(args[0])
	if err != nil {
		return err
	}
	m, err := loadRun(ctx, st, id, "latest")
	if err != nil {
		return err
	}
	counts, bytes := m.Counts()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	var withheld, stale int
	for _, e := range m.Live() {
		switch {
		case e.State != manifest.StateStored:
			withheld++
			fmt.Fprintf(w, "%s\t%s\t%s\n", e.State, e.Path, e.Reason)
		case e.Reason != "":
			stale++
			fmt.Fprintf(w, "note\t%s\t%s\n", e.Path, e.Reason)
		}
	}
	if withheld+stale > 0 {
		fmt.Println("not included, or included with a note:")
		w.Flush()
		fmt.Println()
	}
	fmt.Printf("%s (run %s)\n%d available (%s), %d skipped, %d locked, %d failed, %d deleted in Canvas\n",
		m.CourseName, m.RunID, counts[manifest.StateStored], humanBytes(bytes),
		counts[manifest.StateSkipped], counts[manifest.StateLocked],
		counts[manifest.StateFailed], counts[manifest.StateDeleted])
	return nil
}

func cmdLog(ctx context.Context, st store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: obsync log <course-id>")
	}
	id, err := courseID(args[0])
	if err != nil {
		return err
	}
	runs, err := runIDs(ctx, st, id)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("course %d has no manifests", id)
	}
	latestRun, _ := readLatest(ctx, st, id)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "\tRUN\tGENERATED\tSTORED\tADDED\tREMOVED\tCONTENT\tSTATE\n")
	var prev *manifest.Manifest
	for _, r := range runs {
		mark := ""
		if r == latestRun {
			mark = "*"
		}
		m, err := loadRun(ctx, st, id, r)
		if err != nil {
			fmt.Fprintf(w, "%s\t%s\t<unreadable: %v>\n", mark, r, err)
			continue
		}
		counts, _ := m.Counts()
		ch := map[manifest.ChangeKind]int{}
		for _, c := range manifest.Diff(prev, m) {
			ch[c.Kind]++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t+%d\t-%d\t~%d\t%d\n", mark, r,
			m.GeneratedAt.Local().Format("2006-01-02 15:04"), counts[manifest.StateStored],
			ch[manifest.ChangeAdded], ch[manifest.ChangeRemoved], ch[manifest.ChangeContent], ch[manifest.ChangeState])
		prev = m
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\n* = latest (published). Runs without * wrote a manifest but did not commit.")
	return nil
}

func cmdDiff(ctx context.Context, st store.Store, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: obsync diff <course-id> <run-a> <run-b>")
	}
	id, err := courseID(args[0])
	if err != nil {
		return err
	}
	a, err := loadRun(ctx, st, id, args[1])
	if err != nil {
		return err
	}
	b, err := loadRun(ctx, st, id, args[2])
	if err != nil {
		return err
	}
	changes := manifest.Diff(a, b)
	for _, c := range changes {
		switch c.Kind {
		case manifest.ChangeAdded:
			fmt.Printf("+ %s  (%s)\n", c.Path, c.To)
		case manifest.ChangeRemoved:
			fmt.Printf("- %s  (was %s)\n", c.Path, c.From)
		case manifest.ChangeContent:
			fmt.Printf("~ %s  %s -> %s\n", c.Path, short(c.FromSHA256), short(c.ToSHA256))
		case manifest.ChangeState:
			fmt.Printf("s %s  %s -> %s\n", c.Path, c.From, c.To)
		}
	}
	fmt.Printf("%d changes between %s and %s\n", len(changes), a.RunID, b.RunID)
	return nil
}

func cmdCat(ctx context.Context, st store.Store, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: obsync cat <course-id> <path>")
	}
	id, err := courseID(args[0])
	if err != nil {
		return err
	}
	m, err := loadRun(ctx, st, id, "latest")
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
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(os.Stdout, h), rc); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Errorf("blob for %s is corrupt: manifest says %s, content hashes to %s", e.Path, e.SHA256, got)
	}
	return nil
}

func cmdGC(ctx context.Context, st store.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("obsync gc", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "actually delete; without it gc only reports")
	minAge := fs.Duration("min-age", 24*time.Hour, "never delete blobs younger than this")
	lockTTL := fs.Duration("lock-ttl", time.Hour, "lease lifetime while deleting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apply && *minAge < time.Hour {
		return fmt.Errorf("-min-age %s is below 1h and could delete blobs of a run in progress", *minAge)
	}
	if *apply {
		// Deleting races a run that references an old orphan blob. Hold the
		// same lease the worker takes.
		host, _ := os.Hostname()
		l, err := lease.Acquire(ctx, st, lease.Record{
			Holder: fmt.Sprintf("%s/%d/gc", host, os.Getpid()), RunID: "gc-" + time.Now().UTC().Format("20060102T150405Z"),
			Trigger: "manual", Command: "gc",
		}, lease.Options{TTL: *lockTTL, Log: log})
		if err != nil {
			return err
		}
		defer l.Release(context.Background())
	}
	rep, err := gc.Run(ctx, st, gc.Options{MinAge: *minAge, Now: time.Now(), Apply: *apply, Log: log})
	if err != nil {
		return err
	}
	verb := "would free"
	if *apply {
		verb = "freed"
	}
	fmt.Printf("%d manifests reference %d blobs; %d blobs in store, %d unreferenced (%d younger than %s kept)\n",
		rep.Manifests, rep.ReferencedBlobs, rep.Blobs, rep.Unreferenced, rep.TooYoung, *minAge)
	fmt.Printf("%d deleted, %s %s\n", rep.Deleted, verb, humanBytes(max(rep.FreedBytes, rep.ReclaimableBytes)))
	if !*apply && rep.Unreferenced > rep.TooYoung {
		fmt.Println("dry run: pass -apply to delete")
	}
	return nil
}

func cmdServe(ctx context.Context, fsStore *store.FS, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("obsync serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if host, _, err := net.SplitHostPort(*addr); err != nil {
		return err
	} else if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Warn("serve.exposed", "addr", *addr,
			"risk", "the store is served without authentication to anything that can reach this address")
	}

	srv := &http.Server{Addr: *addr, Handler: &storeHandler{fs: fsStore, log: log}, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	root, _ := filepath.Abs(fsStore.Root)
	log.Info("serve.start", "addr", *addr, "root", root)
	fmt.Fprintf(os.Stderr, "serving %s read-only at http://%s  (plugin base URL)\n", root, *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("serve.stopped")
	return nil
}

// storeHandler exposes exactly the keys a consumer reads, the same shape the
// plugin expects from Garage behind an auth proxy: GET {base}/{key} with Range.
// Read-only, no directory listings, nothing outside manifests/ and blobs/.
type storeHandler struct {
	fs  *store.FS
	log *slog.Logger
}

func (h *storeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		h.log.Info("serve.request", "method", r.Method, "path", r.URL.Path, "range", r.Header.Get("Range"),
			"status", rec.status, "bytes", rec.bytes, "remote", r.RemoteAddr,
			"duration_ms", time.Since(start).Milliseconds())
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		rec.Header().Set("Allow", "GET, HEAD")
		http.Error(rec, "read-only", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	if !strings.HasPrefix(key, "manifests/") && !strings.HasPrefix(key, "blobs/") {
		http.NotFound(rec, r)
		return
	}
	for _, seg := range strings.Split(key, "/") {
		if strings.HasPrefix(seg, ".") {
			http.NotFound(rec, r)
			return
		}
	}
	p, err := h.fs.Path(key)
	if err != nil {
		http.NotFound(rec, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(rec, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(rec, r)
		return
	}

	switch {
	case strings.HasSuffix(key, "/latest"):
		// The only mutable key a consumer reads. Never cache it.
		rec.Header().Set("Cache-Control", "no-store")
		rec.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case strings.HasSuffix(key, ".json"):
		rec.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		rec.Header().Set("Content-Type", "application/json")
	default:
		rec.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		rec.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(rec, r, "", info.ModTime(), f)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func courseID(arg string) (int64, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("course must be a numeric id (see `obsync ls`): %q", arg)
	}
	return id, nil
}

func readLatest(ctx context.Context, st store.Store, id int64) (string, error) {
	rc, err := st.Get(ctx, manifest.LatestKey(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("course %d has no published manifest (see `obsync ls`)", id)
		}
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1024))
	return strings.TrimSpace(string(b)), err
}

// loadRun loads a manifest by run id; "latest" resolves the pointer.
func loadRun(ctx context.Context, st store.Store, id int64, runID string) (*manifest.Manifest, error) {
	if runID == "latest" {
		r, err := readLatest(ctx, st, id)
		if err != nil {
			return nil, err
		}
		runID = r
	}
	rc, err := st.Get(ctx, manifest.ManifestKey(id, runID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("course %d has no manifest for run %s (see `obsync log %d`)", id, runID, id)
		}
		return nil, err
	}
	defer rc.Close()
	return manifest.Decode(rc)
}

func runIDs(ctx context.Context, st store.Store, id int64) ([]string, error) {
	var out []string
	err := st.List(ctx, fmt.Sprintf("manifests/%d/", id), func(o store.ObjectInfo) error {
		if strings.HasSuffix(o.Key, ".json") {
			out = append(out, strings.TrimSuffix(filepath.Base(o.Key), ".json"))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
