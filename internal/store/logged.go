package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"
)

// Logged records every store operation, so the store's contents can be
// reconstructed from logs alone. Mutations log at Info because they are the
// audit trail of what entered or left the store; reads log at Debug.
//
// Optional capabilities are forwarded. When the wrapped backend lacks one, the
// method returns errors.ErrUnsupported, which PutOnce understands.
type Logged struct {
	Inner Store
	Log   *slog.Logger
}

func WithLogging(s Store, log *slog.Logger) *Logged {
	return &Logged{Inner: s, Log: log}
}

func (l *Logged) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := l.Inner.Get(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound):
		l.Log.Debug("store.get", "key", key, "found", false)
	case err != nil:
		l.Log.Warn("store.get_failed", "key", key, "err", err)
	default:
		l.Log.Debug("store.get", "key", key, "found", true)
	}
	return rc, err
}

// counted wraps r to measure an unknown-size stream. A known size is passed
// through untouched: every backend enforces it, and wrapping would hide the
// io.Seeker the S3 backend needs to sign and retry a multi-gigabyte upload
// without spooling it to disk first.
func counted(r io.Reader, size int64) (io.Reader, func() int64) {
	if size >= 0 {
		return r, func() int64 { return size }
	}
	cr := &countingReader{r: r}
	return cr, func() int64 { return cr.n }
}

func (l *Logged) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	start := time.Now()
	body, n := counted(r, size)
	err := l.Inner.Put(ctx, key, body, size)
	if err != nil {
		l.Log.Warn("store.put_failed", "key", key, "err", err)
		return err
	}
	l.Log.Info("store.put", "key", key, "bytes", n(), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (l *Logged) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error {
	ep, ok := l.Inner.(ExclusivePutter)
	if !ok {
		return errors.ErrUnsupported
	}
	start := time.Now()
	body, n := counted(r, size)
	err := ep.PutIfAbsent(ctx, key, body, size)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		l.Log.Info("store.put_if_absent_unsupported", "key", key)
	case errors.Is(err, ErrExists):
		l.Log.Info("store.put_if_absent", "key", key, "created", false)
	case err != nil:
		l.Log.Warn("store.put_if_absent_failed", "key", key, "err", err)
	default:
		l.Log.Info("store.put_if_absent", "key", key, "created", true, "bytes", n(),
			"duration_ms", time.Since(start).Milliseconds())
	}
	return err
}

func (l *Logged) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	st, ok := l.Inner.(Stater)
	if !ok {
		return ObjectInfo{}, errors.ErrUnsupported
	}
	info, err := st.Stat(ctx, key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		l.Log.Warn("store.stat_failed", "key", key, "err", err)
	} else {
		l.Log.Debug("store.stat", "key", key, "found", err == nil)
	}
	return info, err
}

func (l *Logged) Exists(ctx context.Context, key string) (bool, error) {
	ok, err := l.Inner.Exists(ctx, key)
	if err != nil {
		l.Log.Warn("store.exists_failed", "key", key, "err", err)
	} else {
		l.Log.Debug("store.exists", "key", key, "exists", ok)
	}
	return ok, err
}

func (l *Logged) Delete(ctx context.Context, key string) error {
	err := l.Inner.Delete(ctx, key)
	if err != nil {
		l.Log.Warn("store.delete_failed", "key", key, "err", err)
		return err
	}
	l.Log.Info("store.delete", "key", key)
	return nil
}

func (l *Logged) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	n := 0
	err := l.Inner.List(ctx, prefix, func(o ObjectInfo) error {
		n++
		return fn(o)
	})
	if err != nil {
		l.Log.Warn("store.list_failed", "prefix", prefix, "visited", n, "err", err)
		return err
	}
	l.Log.Debug("store.list", "prefix", prefix, "objects", n)
	return nil
}

func (l *Logged) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	rr, ok := l.Inner.(RangeReader)
	if !ok {
		return nil, errors.ErrUnsupported
	}
	rc, err := rr.GetRange(ctx, key, off, n)
	if err != nil {
		l.Log.Warn("store.get_range_failed", "key", key, "offset", off, "length", n, "err", err)
	} else {
		l.Log.Debug("store.get_range", "key", key, "offset", off, "length", n)
	}
	return rc, err
}

var (
	_ Store           = (*Logged)(nil)
	_ ExclusivePutter = (*Logged)(nil)
	_ RangeReader     = (*Logged)(nil)
	_ Stater          = (*Logged)(nil)
)
