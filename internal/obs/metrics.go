// Package obs holds metrics and logging setup.
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
	"log/slog"
	"os"
	"time"
)

type Metrics struct {
	FilesSeen       int
	FilesFetched    int
	FilesSkipped    int
	FilesFailed     int
	BytesDownloaded int64
	CanvasRequests  int
	ThrottleEvents  int
	RateLimitRemain float64
	RunDuration     time.Duration
	Success         bool
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
// Everything else is a counter you look at once that fires.
func Push(ctx context.Context, gateway string, m Metrics) error {
	slog.Info("metrics (pushgateway not wired yet)",
		"seen", m.FilesSeen, "fetched", m.FilesFetched, "skipped", m.FilesSkipped,
		"failed", m.FilesFailed, "bytes", m.BytesDownloaded,
		"rate_limit_remaining", m.RateLimitRemain, "success", m.Success)
	return nil
}

func Logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
