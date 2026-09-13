// Package lease is the single-writer guard for the store.
//
// Phase 1 relied on the CronJob's concurrencyPolicy: Forbid alone, because
// manifests/<course>/latest is read-modify-write with no compare-and-swap.
// Manual pulls bypass Forbid: `obsync-worker pull` on a laptop, or
// `kubectl create job --from=cronjob/obsync-worker`, which the CronJob
// controller does not count. So every writer also takes a lease in the store
// itself before writing anything.
//
// Guarantees, stated honestly:
//   - On a backend with a working exclusive create (FS, Memory, and S3 servers
//     proven to honour If-None-Match), acquisition is exclusive. Garage v2.3.0
//     is not one of them.
//   - A lease past its expiry may be stolen. Stealing is delete-then-create,
//     which is not atomic, so two stealers can both believe they won. That is
//     why the runner calls Verify immediately before publishing latest: at
//     most one of them still sees its own holder id there.
//   - Otherwise the lease is best-effort and says so in the logs
//     (lease.best_effort): it verifies itself straight after writing and again
//     before every commit, which stops a second writer committing once it has
//     been overwritten, but cannot stop two writers that race inside that
//     window. On Garage, scheduled runs stay single through the CronJob's
//     concurrencyPolicy: Forbid; keep manual pulls clear of the schedule.
package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/leifsen/obsync/internal/store"
)

// Key is the lease object. It is the second, and last, mutable key.
const Key = "locks/worker.json"

var (
	ErrHeld = errors.New("lease: held by another writer")
	ErrLost = errors.New("lease: no longer held by this writer")
)

type Record struct {
	Holder     string    `json:"holder"`
	RunID      string    `json:"run_id"`
	Trigger    string    `json:"trigger"`
	Command    string    `json:"command"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// HeldError names who holds the lease, so the operator knows what to wait for.
type HeldError struct{ Current Record }

func (e *HeldError) Error() string {
	return fmt.Sprintf("lease: held by %s (run %s, trigger %s) until %s",
		e.Current.Holder, e.Current.RunID, e.Current.Trigger, e.Current.ExpiresAt.Format(time.RFC3339))
}

func (e *HeldError) Unwrap() error { return ErrHeld }

type Lease struct {
	st  store.Store
	log *slog.Logger
	now func() time.Time
	rec Record
}

type Options struct {
	TTL time.Duration
	// Wait keeps retrying a held lease for this long. Zero fails immediately.
	Wait time.Duration
	// Poll is the retry interval while waiting. Defaults to 10s.
	Poll time.Duration
	Now  func() time.Time
	Log  *slog.Logger
}

// Acquire takes the lease for rec.Holder.
func Acquire(ctx context.Context, st store.Store, rec Record, opt Options) (*Lease, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Poll <= 0 {
		opt.Poll = 10 * time.Second
	}
	if opt.TTL <= 0 {
		return nil, fmt.Errorf("lease: ttl must be positive")
	}
	log := opt.Log.With("lease_key", Key, "holder", rec.Holder)
	deadline := opt.Now().Add(opt.Wait)

	for {
		l, err := tryAcquire(ctx, st, rec, opt, log)
		var held *HeldError
		if !errors.As(err, &held) {
			return l, err
		}
		if !opt.Now().Before(deadline) {
			log.Warn("lease.busy", "current_holder", held.Current.Holder,
				"current_run_id", held.Current.RunID, "current_trigger", held.Current.Trigger,
				"expires_at", held.Current.ExpiresAt)
			return nil, err
		}
		log.Info("lease.waiting", "current_holder", held.Current.Holder,
			"current_run_id", held.Current.RunID, "retry_in", opt.Poll.String())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(opt.Poll):
		}
	}
}

func tryAcquire(ctx context.Context, st store.Store, rec Record, opt Options, log *slog.Logger) (*Lease, error) {
	for attempt := 0; attempt < 3; attempt++ {
		now := opt.Now().UTC()
		rec.AcquiredAt, rec.ExpiresAt = now, now.Add(opt.TTL)
		body, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		atomic, err := store.PutOnce(ctx, st, Key, bytes.NewReader(body), int64(len(body)))
		if err == nil {
			l := &Lease{st: st, log: log, now: opt.Now, rec: rec}
			if !atomic {
				log.Warn("lease.best_effort", "reason", "store cannot create objects exclusively")
				// Check-then-write can race; confirm we are the one recorded.
				if err := l.Verify(ctx); err != nil {
					return nil, err
				}
			}
			log.Info("lease.acquired", "run_id", rec.RunID, "trigger", rec.Trigger,
				"expires_at", rec.ExpiresAt, "atomic", atomic)
			return l, nil
		}
		if !errors.Is(err, store.ErrExists) {
			return nil, fmt.Errorf("lease: acquire: %w", err)
		}

		cur, err := read(ctx, st)
		if errors.Is(err, store.ErrNotFound) {
			continue // released between our create and our read
		}
		if err != nil {
			// Unreadable lease: never steal what we cannot inspect.
			return nil, &HeldError{Current: Record{Holder: "unknown (" + err.Error() + ")"}}
		}
		if now.Before(cur.ExpiresAt) {
			return nil, &HeldError{Current: cur}
		}
		log.Warn("lease.stale_stolen", "stale_holder", cur.Holder, "stale_run_id", cur.RunID,
			"expired_at", cur.ExpiresAt)
		if err := st.Delete(ctx, Key); err != nil {
			return nil, fmt.Errorf("lease: remove stale lease: %w", err)
		}
	}
	return nil, fmt.Errorf("lease: could not acquire after repeated contention")
}

// Verify confirms the lease still records this holder. Call it immediately
// before any write that must not race another writer.
func (l *Lease) Verify(ctx context.Context) error {
	cur, err := read(ctx, l.st)
	if err != nil {
		l.log.Error("lease.verify_failed", "err", err)
		return fmt.Errorf("%w: %v", ErrLost, err)
	}
	if cur.Holder != l.rec.Holder {
		l.log.Error("lease.lost", "current_holder", cur.Holder, "current_run_id", cur.RunID)
		return fmt.Errorf("%w: now held by %s", ErrLost, cur.Holder)
	}
	if !l.now().Before(cur.ExpiresAt) {
		l.log.Warn("lease.expired_but_held", "expired_at", cur.ExpiresAt,
			"hint", "run is outliving -lock-ttl; raise it")
	}
	l.log.Debug("lease.verified")
	return nil
}

// Release deletes the lease if this holder still has it.
func (l *Lease) Release(ctx context.Context) error {
	cur, err := read(ctx, l.st)
	if errors.Is(err, store.ErrNotFound) {
		l.log.Warn("lease.release_missing")
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Holder != l.rec.Holder {
		l.log.Warn("lease.release_skipped", "current_holder", cur.Holder)
		return nil
	}
	if err := l.st.Delete(ctx, Key); err != nil {
		return err
	}
	l.log.Info("lease.released", "held_for", l.now().Sub(l.rec.AcquiredAt).Round(time.Second).String())
	return nil
}

func (l *Lease) Record() Record { return l.rec }

func read(ctx context.Context, st store.Store) (Record, error) {
	rc, err := st.Get(ctx, Key)
	if err != nil {
		return Record{}, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("lease: corrupt lease object: %w", err)
	}
	return r, nil
}
