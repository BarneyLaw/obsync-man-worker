package lease

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/leifsen/obsync/internal/store"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func opts(c *clock) Options {
	return Options{TTL: time.Hour, Now: c.now, Log: quiet, Poll: time.Millisecond}
}

func TestSecondWriterIsRefused(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	c := &clock{time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)}

	a, err := Acquire(ctx, st, Record{Holder: "cron", RunID: "r1", Trigger: "cron"}, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(ctx, st, Record{Holder: "manual", RunID: "r2", Trigger: "manual"}, opts(c))
	var held *HeldError
	if !errors.As(err, &held) || !errors.Is(err, ErrHeld) {
		t.Fatalf("second writer got %v, want HeldError", err)
	}
	if held.Current.Holder != "cron" || !strings.Contains(err.Error(), "r1") {
		t.Fatalf("error must name the current holder: %v", err)
	}

	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(ctx, st, Record{Holder: "manual"}, opts(c)); err != nil {
		t.Fatalf("lease not reusable after release: %v", err)
	}
}

func TestStaleLeaseIsStolenAndOldHolderCannotCommit(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	c := &clock{time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)}

	old, err := Acquire(ctx, st, Record{Holder: "crashed"}, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Hour)
	fresh, err := Acquire(ctx, st, Record{Holder: "fresh"}, opts(c))
	if err != nil {
		t.Fatalf("expired lease should be stealable: %v", err)
	}
	if err := old.Verify(ctx); !errors.Is(err, ErrLost) {
		t.Fatalf("old holder Verify = %v, want ErrLost", err)
	}
	if err := fresh.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	// The old holder must not delete the new holder's lease on its way out.
	if err := old.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Verify(ctx); err != nil {
		t.Fatalf("stale holder's release removed the live lease: %v", err)
	}
}

func TestWaitGivesUpAtDeadline(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	c := &clock{time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)}
	if _, err := Acquire(ctx, st, Record{Holder: "cron"}, opts(c)); err != nil {
		t.Fatal(err)
	}
	o := opts(&clock{c.t})
	o.Now = func() time.Time { c.t = c.t.Add(time.Second); return c.t }
	o.Wait = 5 * time.Second
	if _, err := Acquire(ctx, st, Record{Holder: "manual"}, o); !errors.Is(err, ErrHeld) {
		t.Fatalf("got %v", err)
	}
}

func TestCorruptLeaseIsNeverStolen(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	_ = st.Put(ctx, Key, strings.NewReader("{not json"), -1)
	c := &clock{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := Acquire(ctx, st, Record{Holder: "x"}, opts(c)); !errors.Is(err, ErrHeld) {
		t.Fatalf("got %v, want ErrHeld", err)
	}
}
