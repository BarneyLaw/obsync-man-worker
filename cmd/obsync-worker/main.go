// Command obsync-worker pulls Canvas course files into the store.
//
//	obsync-worker run      scheduled pass over every active course, or only those
//	                       -course names (what the CronJob runs)
//	obsync-worker pull     manual pull outside the schedule, optionally scoped to
//	                       courses, directories and files
//	obsync-worker courses  list active Canvas courses and whether their files are reachable
//
// With no command, run is assumed, so existing CronJob args keep working.
//
// Single writer. The CronJob's concurrencyPolicy: Forbid only covers runs the
// CronJob controller starts; a manual pull, or `kubectl create job
// --from=cronjob/obsync-worker`, bypasses it. manifests/<course>/latest is
// read-modify-write with no compare-and-swap, so every writing command takes
// the store lease first (internal/lease) and re-verifies it before each commit.
//
// Exit codes: 0 success, 1 failure, 2 usage or configuration error, 3 another
// run holds the lease, 4 Canvas rejected the token.
//
// Logs are JSON on stderr, one event per line; see internal/obs for the scheme.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/leifsen/obsync/internal/canvas"
	"github.com/leifsen/obsync/internal/lease"
	"github.com/leifsen/obsync/internal/obs"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/run"
	"github.com/leifsen/obsync/internal/scope"
	"github.com/leifsen/obsync/internal/store"
)

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	exitBusy   = 3
	exitToken  = 4
)

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run", "pull":
		return cmdPass(cmd, args)
	case "courses":
		return cmdCourses(args)
	case "help":
		usage()
		return exitOK
	default:
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: obsync-worker <command> [flags]

commands:
  run       scheduled pass over every active course (what the CronJob runs)
              -course CS3103,LAG1201  only these courses; one that does not resolve
                                      fails the run after the others have synced
              -dry-run                plan and log every decision, write nothing
  pull      manual pull outside the schedule
              -course CS3103          one or more courses, by code or numeric id
              -path "Week 1"          only these directories, files or globs (needs -course)
              -dry-run                plan and log every decision, write nothing
              -wait 10m               wait for a running pass instead of exiting 3
  courses   list active courses; -probe checks whether their files are reachable

Run "obsync-worker <command> -h" for all flags.
Exit codes: 0 ok, 1 failed, 2 usage, 3 lease held by another run, 4 token rejected.
`)
}

type commonFlags struct {
	rules    string
	fsStore  string
	gateway  string
	logLevel string
	tmpDir   string
	lockTTL  time.Duration
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.rules, "rules", "/etc/obsync/rules.json", "path to worker rules")
	fs.StringVar(&c.fsStore, "fs-store", "", "use a local directory as the store; without it GARAGE_* selects S3")
	fs.StringVar(&c.gateway, "pushgateway", "", "prometheus pushgateway url")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&c.tmpDir, "tmp-dir", "", "directory for in-flight downloads (default: OS temp dir)")
	fs.DurationVar(&c.lockTTL, "lock-ttl", 6*time.Hour, "lease lifetime; a crashed run's lease is stealable after this")
}

// listFlag is a repeatable flag. With split, each value is also split on commas.
type listFlag struct {
	vals  []string
	split bool
}

func (l *listFlag) String() string { return strings.Join(l.vals, ",") }

func (l *listFlag) Set(v string) error {
	parts := []string{v}
	if l.split {
		parts = strings.Split(v, ",")
	}
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			l.vals = append(l.vals, p)
		}
	}
	return nil
}

func cmdPass(cmd string, args []string) int {
	fs := flag.NewFlagSet("obsync-worker "+cmd, flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	courses := &listFlag{split: true}
	paths := &listFlag{}
	var dryRun bool
	var wait time.Duration
	fs.Var(courses, "course", "only these courses, by course code or numeric id (repeatable, comma-separated)")
	fs.BoolVar(&dryRun, "dry-run", false, "list and plan, log every decision, download and write nothing")
	if cmd == "pull" {
		fs.Var(paths, "path", "restrict the pull to a directory, file or glob inside the course (repeatable)")
		fs.DurationVar(&wait, "wait", 0, "if another run holds the lease, wait up to this long")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %q\n", fs.Args())
		return exitUsage
	}

	trigger := "cron"
	if cmd == "pull" {
		trigger = "manual"
	}
	runID := newRunID()
	base, err := obs.NewLogger(os.Stderr, c.logLevel, "obsync-worker")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}
	log := base.With("run_id", runID, "command", cmd, "trigger", trigger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	host, _ := os.Hostname()
	started := time.Now()
	p := &pass{
		cmd: cmd, log: log, gateway: c.gateway, started: started,
		summary: run.Summary{
			RunID: runID, Command: cmd, Trigger: trigger, Host: host, PID: os.Getpid(),
			DryRun: dryRun, CourseSelectors: courses.vals, Scope: paths.vals,
			StartedAt: started.UTC(),
		},
	}

	log.Info("run.start", "host", host, "pid", os.Getpid(), "go_version", runtime.Version(),
		"rules", c.rules, "fs_store", c.fsStore, "dry_run", dryRun,
		"courses", courses.vals, "scope", paths.vals, "lock_ttl", c.lockTTL.String(), "wait", wait.String())

	// Configuration. Every failure here is exit 2 and touches nothing.
	sc, err := scope.Parse(paths.vals)
	if err == nil && len(paths.vals) > 0 && len(courses.vals) == 0 {
		err = errors.New("-path needs -course: directory names differ between courses")
	}
	if err != nil {
		return p.configError(err)
	}
	pol, err := loadRules(c.rules)
	if err != nil {
		return p.configError(err)
	}
	p.summary.RulesHash = pol.Hash()
	log.Info("rules.loaded", "path", c.rules, "rules_hash", p.summary.RulesHash,
		"rules", len(pol.Rules), "default", pol.Default)

	baseURL, token := os.Getenv("CANVAS_BASE_URL"), os.Getenv("CANVAS_TOKEN")
	if baseURL == "" || token == "" {
		return p.configError(errors.New("CANVAS_BASE_URL and CANVAS_TOKEN are required"))
	}
	if !strings.HasPrefix(baseURL, "https://") {
		log.Warn("canvas.insecure_base_url", "base_url", baseURL)
	}
	src := canvas.New(baseURL, token).WithLogger(log)
	p.src = src
	log.Info("canvas.configured", "base_url", src.BaseURL)

	raw, desc, err := store.Open(c.fsStore, os.Getenv)
	if err != nil {
		return p.configError(err)
	}
	log.Info("store.configured", "kind", desc.Kind, "location", desc.Location)
	st := store.WithLogging(raw, log)
	p.store = st

	r := &run.Runner{
		Source: src, Store: st, Policy: pol, Log: log,
		Now: time.Now, TempDir: c.tmpDir, DryRun: dryRun,
	}

	// A dry run writes nothing, so it needs no lease and cannot block a real run.
	if !dryRun {
		l, err := lease.Acquire(ctx, st, lease.Record{
			Holder: fmt.Sprintf("%s/%d/%s", host, os.Getpid(), runID),
			RunID:  runID, Trigger: trigger, Command: cmd,
		}, lease.Options{TTL: c.lockTTL, Wait: wait, Log: log})
		if errors.Is(err, lease.ErrHeld) {
			log.Warn("run.lease_held", "err", err, "hint", "retry later or pass -wait")
			return p.finish("lease_held", exitBusy, err)
		}
		if err != nil {
			log.Error("run.lease_failed", "err", err)
			return p.finish("failed", exitFailed, err)
		}
		defer func() {
			// The run context may already be cancelled; releasing must still happen.
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := l.Release(rctx); err != nil {
				log.Error("lease.release_failed", "err", err, "effect", "next run must wait for -lock-ttl")
			}
		}()
		r.Guard = l
	}

	all, err := src.Courses(ctx)
	if err != nil {
		if errors.Is(err, canvas.ErrUnauthorized) {
			p.metrics.TokenRejected = true
			log.Error("run.token_rejected", "err", err, "hint", "generate a new Canvas access token")
			return p.finish("token_rejected", exitToken, err)
		}
		log.Error("run.list_courses_failed", "err", err)
		return p.finish("failed", exitFailed, err)
	}
	// A manual pull names courses someone just typed, so a selector that does not
	// resolve is a usage error and nothing runs. A scheduled run's list is config
	// that goes stale as semesters end: it syncs the courses that do resolve,
	// then exits failed so ObsyncWorkerLastRunFailed says the list needs editing.
	var selected []canvas.Course
	selectorsUnresolved := false
	if cmd == "pull" {
		selected, err = canvas.SelectCourses(all, courses.vals)
		if err != nil {
			log.Error("run.course_selection_invalid", "err", err)
			return p.finish("failed", exitUsage, err)
		}
	} else {
		var problems []error
		selected, problems = canvas.ResolveCourses(all, courses.vals)
		for _, perr := range problems {
			log.Error("run.course_selector_unresolved", "err", perr,
				"effect", "the courses that resolved still run; this run exits failed",
				"hint", "edit -course in the CronJob args")
		}
		selectorsUnresolved = len(problems) > 0
	}
	codes := make([]string, len(selected))
	for i, co := range selected {
		codes[i] = co.Code
	}
	log.Info("run.courses_selected", "count", len(selected), "courses", codes)

	var (
		failed, interrupted, tokenDead, stopping bool
		planned                                  int
		unmatched                                = map[string]int{}
	)
	failed = selectorsUnresolved
	for _, co := range selected {
		res := run.CourseResult{CourseID: co.ID, Code: co.Code, Name: co.Name}
		if stopping || ctx.Err() != nil {
			if ctx.Err() != nil {
				interrupted = true
			}
			res.Status = run.StatusNotRun
			log.Warn("course.not_run", "course_id", co.ID, "course_code", co.Code, "reason", "run stopping")
			p.summary.Courses = append(p.summary.Courses, res)
			continue
		}

		s, err := r.Course(ctx, co, run.Options{RunID: runID, Scope: sc})
		res.Stats = s
		clog := log.With("course_id", co.ID, "course_code", co.Code)
		switch {
		case err == nil:
			planned++
			res.Status = run.StatusCommitted
			if dryRun {
				res.Status = run.StatusDryRun
			}
			for _, u := range s.UnmatchedScope {
				unmatched[u]++
			}
		case errors.Is(err, canvas.ErrUnauthorized):
			res.Status, res.Error = run.StatusFailed, err.Error()
			failed, tokenDead, stopping = true, true, true
			p.metrics.TokenRejected = true
			clog.Error("run.token_rejected", "err", err, "effect", "remaining courses not attempted")
		case errors.Is(err, canvas.ErrForbidden):
			res.Status, res.Error = run.StatusForbidden, err.Error()
			p.metrics.CoursesForbidden++
			// Scheduled runs treat a hidden Files tab as a permanent fact about
			// the course. An explicit manual request for it is a failure.
			if cmd == "pull" {
				failed = true
			}
			clog.Warn("course.forbidden", "err", err, "effect", "previous manifest, if any, stays live")
		case errors.Is(err, lease.ErrLost):
			res.Status, res.Error = run.StatusFailed, err.Error()
			failed, stopping = true, true
			clog.Error("run.lease_lost", "err", err, "effect", "remaining courses not attempted")
		case ctx.Err() != nil:
			res.Status, res.Error = run.StatusFailed, err.Error()
			interrupted, stopping = true, true
			clog.Error("course.interrupted", "err", err)
		default:
			res.Status, res.Error = run.StatusFailed, err.Error()
			failed = true
			p.metrics.CoursesFailed++
			clog.Error("course.failed", "err", err)
		}
		p.add(s)
		p.summary.Courses = append(p.summary.Courses, res)
	}

	for pattern, n := range unmatched {
		if planned > 0 && n == planned {
			failed = true
			log.Error("scope.pattern_matched_nothing", "pattern", pattern,
				"hint", "check the path with a -dry-run or `obsync ls <course>`")
		}
	}

	switch {
	case tokenDead:
		return p.finish("token_rejected", exitToken, nil)
	case interrupted:
		return p.finish("interrupted", exitFailed, ctx.Err())
	case failed && p.committed() > 0:
		return p.finish("partial", exitFailed, nil)
	case failed:
		return p.finish("failed", exitFailed, nil)
	}
	return p.finish("success", exitOK, nil)
}

// pass accumulates what cmdPass needs to report at the end.
type pass struct {
	cmd     string
	log     *slog.Logger
	gateway string
	started time.Time
	src     *canvas.Client
	store   store.Store
	summary run.Summary
	metrics obs.Metrics
}

func (p *pass) add(s run.Stats) {
	p.metrics.FilesSeen += s.Seen
	p.metrics.FilesFetched += s.Fetched
	p.metrics.FilesSkipped += s.Skipped
	p.metrics.FilesFailed += s.Failed
	p.metrics.BytesDownloaded += s.Bytes
}

func (p *pass) committed() int {
	n := 0
	for _, c := range p.summary.Courses {
		if c.Status == run.StatusCommitted {
			n++
		}
	}
	return n
}

func (p *pass) configError(err error) int {
	p.log.Error("run.config_invalid", "err", err)
	return p.finish("config_invalid", exitUsage, err)
}

// finish writes the run summary and metrics and logs the final event. It
// always runs, whatever the outcome, so every invocation leaves a record.
func (p *pass) finish(outcome string, code int, err error) int {
	p.summary.FinishedAt = time.Now().UTC()
	p.summary.Outcome = outcome
	if err != nil {
		p.summary.Error = err.Error()
	}

	// Cancellation must not stop the record of the cancellation being written.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case p.store == nil:
		p.log.Info("run.summary", "summary", p.summary, "stored", false, "reason", "no store configured")
	case p.summary.DryRun:
		p.log.Info("run.summary", "summary", p.summary, "stored", false, "reason", "dry run writes nothing")
	default:
		if werr := run.WriteSummary(ctx, p.store, p.summary); werr != nil {
			p.log.Error("run.summary_write_failed", "err", werr)
		} else {
			p.log.Info("run.summary_written", "key", run.SummaryKey(p.summary.RunID))
		}
	}

	p.metrics.RunDuration = time.Since(p.started)
	p.metrics.Success = code == exitOK
	if p.src != nil {
		p.metrics.CanvasRequests = p.src.Requests()
		p.metrics.CanvasDownloads = p.src.Downloads()
		p.metrics.ThrottleEvents = p.src.Limiter().Throttles()
		p.metrics.QuotaStalls = p.src.Limiter().Stalls()
		p.metrics.RateLimitRemain = p.src.RateLimitRemaining()
	}
	_ = obs.Push(ctx, p.log, p.gateway, p.metrics)

	p.log.Info("run.done", "outcome", outcome, "exit_code", code,
		"duration_ms", p.metrics.RunDuration.Milliseconds(), "courses", len(p.summary.Courses),
		"courses_committed", p.committed(), "files_fetched", p.metrics.FilesFetched,
		"bytes_downloaded", p.metrics.BytesDownloaded)
	return code
}

func cmdCourses(args []string) int {
	fs := flag.NewFlagSet("obsync-worker courses", flag.ContinueOnError)
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	probe := fs.Bool("probe", false, "check whether each course's files are readable (one request per course)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	log, err := obs.NewLogger(os.Stderr, *logLevel, "obsync-worker")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}
	log = log.With("command", "courses")

	baseURL, token := os.Getenv("CANVAS_BASE_URL"), os.Getenv("CANVAS_TOKEN")
	if baseURL == "" || token == "" {
		log.Error("run.config_invalid", "err", "CANVAS_BASE_URL and CANVAS_TOKEN are required")
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	src := canvas.New(baseURL, token).WithLogger(log)
	all, err := src.Courses(ctx)
	if err != nil {
		log.Error("courses.list_failed", "err", err)
		if errors.Is(err, canvas.ErrUnauthorized) {
			return exitToken
		}
		return exitFailed
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := "ID\tCODE\tNAME"
	if *probe {
		header += "\tFILES"
	}
	fmt.Fprintln(w, header)
	for _, co := range all {
		row := fmt.Sprintf("%d\t%s\t%s", co.ID, co.Code, co.Name)
		if *probe {
			status := "ok"
			switch perr := src.ProbeFiles(ctx, co.ID); {
			case perr == nil:
			case errors.Is(perr, canvas.ErrForbidden):
				status = "forbidden (Files tab hidden)"
			default:
				status = "error: " + perr.Error()
			}
			log.Info("courses.probe", "course_id", co.ID, "course_code", co.Code, "files", status)
			row += "\t" + status
		}
		fmt.Fprintln(w, row)
	}
	w.Flush()
	return exitOK
}

func loadRules(path string) (*policy.Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	return policy.Parse(b)
}

// newRunID sorts by time and cannot collide between two passes started in the
// same second, which a manual pull next to the CronJob makes plausible.
func newRunID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}
