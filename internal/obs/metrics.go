// Package obs holds metrics and logging setup.
//
// Logging is the audit trail. Every non-trivial action is one JSON line whose
// message is a stable dotted event name, so the log can be queried like a
// table:
//
//	jq 'select(.msg=="latest.published")'      every commit, with its diff counts
//	jq 'select(.run_id=="20260913T221952Z-1a2b")' one run end to end
//	jq 'select(.msg|startswith("store."))'      every write and delete
//
// Levels: Info for anything that decides, changes state or talks to a remote;
// Warn for degraded outcomes that still completed; Error for aborts; Debug for
// no-ops such as unchanged files and store reads. Secrets are never logged:
// the Canvas token is only ever in a header and download verifiers are
// redacted by the canvas package.
//
// A CronJob's pod exits, so Prometheus will never scrape it. Metrics go to a
// Pushgateway instead. Batch jobs are the one case Pushgateway is genuinely
// designed for, and obsync_last_success_timestamp_seconds persisting between
// runs is exactly what a staleness alert needs.
//
// The alternative is flipping the worker to a Deployment with an internal
// ticker so it can be scraped directly. Simpler monitoring, but you lose
// Kubernetes-managed retry and you have to rebuild the single-writer guarantee
// that concurrencyPolicy: Forbid gives you for free. Keep Forbid, take
// Pushgateway.
package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

type Metrics struct {
	FilesSeen        int
	FilesFetched     int
	FilesSkipped     int
	FilesFailed      int
	BytesDownloaded  int64
	CanvasRequests   int64
	CanvasDownloads  int64
	ThrottleEvents   int64
	QuotaStalls      int64
	CoursesFailed    int
	CoursesForbidden int
	TokenRejected    bool
	RateLimitRemain  float64
	RunDuration      time.Duration
	Success          bool
}

// NewLogger builds the JSON audit logger. level is debug, info, warn or error.
func NewLogger(w io.Writer, level, app string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv})
	return slog.New(h).With("app", app), nil
}

// Push writes to a Prometheus Pushgateway.
//
// STUB, step 6. Use github.com/prometheus/client_golang/prometheus/push with
// job="obsync-worker". The only metric that really matters is:
//
//	obsync_last_success_timestamp_seconds
//
// and the only alert that really matters is:
//
//	expr: time() - obsync_last_success_timestamp_seconds > 86400
//	for:  1h
//
// Everything else is a counter you look at once that fires. Until then the
// metrics are logged so nothing is lost.
func Push(ctx context.Context, log *slog.Logger, gateway string, m Metrics) error {
	attrs := slog.Group("metrics",
		"files_seen", m.FilesSeen, "files_fetched", m.FilesFetched,
		"files_skipped", m.FilesSkipped, "files_failed", m.FilesFailed,
		"bytes_downloaded", m.BytesDownloaded,
		"canvas_requests", m.CanvasRequests, "canvas_downloads", m.CanvasDownloads,
		"throttle_events", m.ThrottleEvents, "quota_stalls", m.QuotaStalls,
		"courses_failed", m.CoursesFailed, "courses_forbidden", m.CoursesForbidden,
		"token_rejected", m.TokenRejected, "rate_limit_remaining", m.RateLimitRemain,
		"run_duration_ms", m.RunDuration.Milliseconds(), "success", m.Success)
	if gateway == "" {
		log.Info("metrics.recorded", attrs)
		return nil
	}
	log.Warn("metrics.push_skipped", "gateway", gateway,
		"reason", "pushgateway client not wired yet (build step 6)", attrs)
	return nil
}
