// Command obsync-worker performs one pull pass and exits.
//
// Runs as a CronJob with concurrencyPolicy: Forbid. That Forbid is LOAD
// BEARING, not a nicety: the whole design assumes exactly one writer, and
// manifests/<course>/latest is a read-modify-write with no compare-and-swap.
// Two concurrent workers will clobber each other.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leifsen/obsync/internal/canvas"
	"github.com/leifsen/obsync/internal/obs"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/run"
	"github.com/leifsen/obsync/internal/store"
)

func main() {
	var (
		rulesPath = flag.String("rules", "/etc/obsync/rules.json", "path to worker rules")
		outDir    = flag.String("fs-store", "", "use a local directory as the store (dev)")
		gateway   = flag.String("pushgateway", "", "prometheus pushgateway url")
	)
	flag.Parse()

	log := obs.Logger()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pol, err := loadRules(*rulesPath)
	if err != nil {
		log.Error("rules", "err", err)
		os.Exit(1)
	}

	base := os.Getenv("CANVAS_BASE_URL")
	token := os.Getenv("CANVAS_TOKEN")
	if base == "" || token == "" {
		log.Error("CANVAS_BASE_URL and CANVAS_TOKEN are required")
		os.Exit(1)
	}
	src := canvas.New(base, token)

	var st store.Store
	if *outDir != "" {
		st = store.NewFS(*outDir)
	} else {
		// TODO(step 5): construct store.S3 from GARAGE_* env.
		log.Error("no store configured; pass -fs-store while S3 is still a stub")
		os.Exit(1)
	}

	r := &run.Runner{
		Source: src, Store: st, Policy: pol, Log: log,
		Now: time.Now, DownloadConcurrency: 8,
	}

	runID := time.Now().UTC().Format("20060102T150405Z")
	started := time.Now()
	var m obs.Metrics

	courses, err := src.Courses(ctx)
	if err != nil {
		log.Error("list courses", "err", err)
		obs.Push(ctx, *gateway, m)
		os.Exit(1)
	}

	failed := false
	for _, c := range courses {
		s, err := r.Course(ctx, c, runID)
		if err != nil {
			// One bad course must not abort the others. Its previous manifest
			// stays live, which is the correct degraded state.
			log.Error("course failed", "course", c.Name, "err", err)
			failed = true
			continue
		}
		log.Info("course done", "course", c.Name,
			"seen", s.Seen, "fetched", s.Fetched, "skipped", s.Skipped,
			"locked", s.Locked, "tombstoned", s.Tombstoned, "bytes", s.Bytes)
		m.FilesSeen += s.Seen
		m.FilesFetched += s.Fetched
		m.FilesSkipped += s.Skipped
		m.FilesFailed += s.Failed
		m.BytesDownloaded += s.Bytes
	}

	m.RunDuration = time.Since(started)
	m.RateLimitRemain = src.RateLimitRemaining()
	m.Success = !failed
	obs.Push(ctx, *gateway, m)

	if failed {
		os.Exit(1)
	}
}

func loadRules(path string) (*policy.Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p policy.Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse rules: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}
