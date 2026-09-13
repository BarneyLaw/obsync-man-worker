package canvas

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Limiter is a RoundTripper that respects Canvas's leaky bucket.
//
// The thing that kills you is CONCURRENCY, not volume: Canvas charges 50 units
// up front for every in-flight request before subtracting the real cost, so
// simultaneous calls fill the bucket regardless of how cheap each one is. Cap
// metadata concurrency at 2-4.
//
// Downloads go to presigned storage URLs on a different host and do NOT consume
// API quota, so they use a separate client with no limiter and much higher
// concurrency.
type Limiter struct {
	Base http.RoundTripper
	// Floor is the remaining-quota level below which a request first stalls.
	Floor float64
	// Stall is how long to pause when the last observed quota is below Floor.
	Stall time.Duration
	// MaxRetries on throttle responses.
	MaxRetries int
	// MaxInFlight caps concurrent API requests.
	MaxInFlight int
	// MaxRetryAfter caps a server-supplied Retry-After.
	MaxRetryAfter time.Duration
	Log           *slog.Logger

	semOnce sync.Once
	sem     chan struct{}

	mu        sync.Mutex
	remaining float64
	blockedTo time.Time

	throttles atomic.Int64
	stalls    atomic.Int64
}

func NewLimiter(base http.RoundTripper) *Limiter {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Limiter{
		Base: base, Floor: 100, Stall: 5 * time.Second, MaxRetries: 5,
		MaxInFlight: 3, MaxRetryAfter: 5 * time.Minute,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		remaining: 700,
	}
}

func (l *Limiter) Remaining() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.remaining
}

func (l *Limiter) Throttles() int64 { return l.throttles.Load() }
func (l *Limiter) Stalls() int64    { return l.stalls.Load() }

func (l *Limiter) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := l.acquire(ctx); err != nil {
		return nil, err
	}
	defer l.release()

	retryable := req.Body == nil || req.Body == http.NoBody
	for attempt := 0; ; attempt++ {
		if err := l.wait(ctx, req); err != nil {
			return nil, err
		}

		resp, err := l.Base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		l.observe(resp)

		// Instructure's own docs disagree about whether throttling returns 403
		// or 429, so treat both as throttle. 401 is NOT retryable: a dead token
		// should page you, not spin for an hour.
		if !isThrottle(resp) {
			return resp, nil
		}
		l.throttles.Add(1)
		if !retryable || attempt >= l.MaxRetries {
			l.Log.Warn("canvas.throttle_exhausted", "path", req.URL.Path,
				"status", resp.StatusCode, "attempts", attempt+1)
			return resp, nil
		}
		resp.Body.Close()
		d := l.backoff(attempt, resp)
		l.Log.Warn("canvas.throttled", "path", req.URL.Path, "status", resp.StatusCode,
			"attempt", attempt+1, "backoff_ms", d.Milliseconds(), "rate_limit_remaining", l.Remaining())
		if err := sleep(ctx, d); err != nil {
			return nil, err
		}
	}
}

func (l *Limiter) acquire(ctx context.Context) error {
	l.semOnce.Do(func() {
		n := l.MaxInFlight
		if n <= 0 {
			n = 1
		}
		l.sem = make(chan struct{}, n)
	})
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Limiter) release() { <-l.sem }

func isThrottle(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	// A 403 is only a throttle if the rate-limit header says so. A plain 403 is
	// a permissions problem and retrying it is pointless.
	if resp.StatusCode == http.StatusForbidden {
		if v := resp.Header.Get("X-Rate-Limit-Remaining"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f <= 0 {
				return true
			}
		}
	}
	return false
}

func (l *Limiter) observe(resp *http.Response) {
	v := resp.Header.Get("X-Rate-Limit-Remaining")
	if v == "" {
		return
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return
	}
	l.mu.Lock()
	l.remaining = f
	l.mu.Unlock()
}

// wait pauses at most once per call for low quota.
//
// The client only learns the bucket level from response headers, so it cannot
// watch the bucket refill without making a request. The previous version
// looped until remaining rose above Floor, which only a request could make
// happen, and hung forever. Now it stalls once, then assumes recovery and lets
// the next response correct the estimate.
func (l *Limiter) wait(ctx context.Context, req *http.Request) error {
	l.mu.Lock()
	blocked := l.blockedTo
	rem := l.remaining
	l.mu.Unlock()

	if d := time.Until(blocked); d > 0 {
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
	if rem < l.Floor {
		l.stalls.Add(1)
		l.Log.Info("canvas.quota_low_stall", "path", req.URL.Path, "rate_limit_remaining", rem,
			"floor", l.Floor, "stall_ms", l.Stall.Milliseconds())
		if err := sleep(ctx, l.Stall); err != nil {
			return err
		}
		l.mu.Lock()
		if l.remaining < l.Floor {
			l.remaining = l.Floor
		}
		l.mu.Unlock()
	}
	return nil
}

func (l *Limiter) backoff(attempt int, resp *http.Response) time.Duration {
	d := time.Duration(1<<uint(min(attempt, 6))) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	// Jitter: without it, every stalled request wakes at the same instant and
	// re-fills the bucket immediately.
	d += time.Duration(rand.Int63n(int64(d / 2)))
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			d = min(time.Duration(secs)*time.Second, l.MaxRetryAfter)
		}
	}
	l.mu.Lock()
	l.blockedTo = time.Now().Add(d)
	l.mu.Unlock()
	return d
}

// sleep is a cancellable time.Sleep. SIGTERM from Kubernetes must not wait out
// a minute of backoff.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
