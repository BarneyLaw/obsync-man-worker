package canvas

import (
	"math/rand"
	"net/http"
	"strconv"
	"sync"
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
	// Floor is the remaining-quota level below which we stall.
	Floor float64
	// MaxRetries on throttle responses.
	MaxRetries int

	mu        sync.Mutex
	remaining float64
	blockedTo time.Time
}

func NewLimiter(base http.RoundTripper) *Limiter {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Limiter{Base: base, Floor: 100, MaxRetries: 5, remaining: 700}
}

func (l *Limiter) Remaining() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.remaining
}

func (l *Limiter) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		l.wait()

		resp, err := l.Base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		l.observe(resp)

		// Instructure's own docs disagree about whether throttling returns 403
		// or 429, so treat both as throttle. 401 is NOT retryable: a dead token
		// should page you, not spin for an hour.
		if !isThrottle(resp) || attempt >= l.MaxRetries {
			return resp, nil
		}
		resp.Body.Close()
		l.backoff(attempt, resp)
	}
}

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

func (l *Limiter) wait() {
	for {
		l.mu.Lock()
		blocked := l.blockedTo
		rem := l.remaining
		l.mu.Unlock()

		if d := time.Until(blocked); d > 0 {
			time.Sleep(d)
			continue
		}
		if rem >= l.Floor {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func (l *Limiter) backoff(attempt int, resp *http.Response) {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	// Jitter: without it, every stalled request wakes at the same instant and
	// re-fills the bucket immediately.
	d += time.Duration(rand.Int63n(int64(d / 2)))
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			d = time.Duration(secs) * time.Second
		}
	}
	l.mu.Lock()
	l.blockedTo = time.Now().Add(d)
	l.mu.Unlock()
	time.Sleep(d)
}
