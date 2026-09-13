package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Every backend must behave identically; that is the promise the interface
// makes to everything above it.
func backends(t *testing.T) map[string]Store {
	return map[string]Store{
		"fs":         NewFS(t.TempDir()),
		"memory":     NewMemory(),
		"logged(fs)": WithLogging(NewFS(t.TempDir()), slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
}

func read(t *testing.T, s Store, key string) string {
	t.Helper()
	rc, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestConformance(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Get(ctx, "missing/key"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get missing = %v, want ErrNotFound", err)
			}
			if err := s.Put(ctx, "a/b/c", strings.NewReader("hello"), 5); err != nil {
				t.Fatal(err)
			}
			if got := read(t, s, "a/b/c"); got != "hello" {
				t.Fatalf("got %q", got)
			}
			if ok, _ := s.Exists(ctx, "a/b/c"); !ok {
				t.Fatal("Exists after Put")
			}

			// Size contract: a truncated stream must never become an object.
			if err := s.Put(ctx, "a/short", strings.NewReader("abc"), 10); err == nil {
				t.Fatal("short stream against a known size must fail")
			}
			if ok, _ := s.Exists(ctx, "a/short"); ok {
				t.Fatal("failed Put left an object behind")
			}
			if err := s.Put(ctx, "a/unknown", strings.NewReader("abc"), -1); err != nil {
				t.Fatalf("size -1 means unknown: %v", err)
			}

			if _, err := PutOnce(ctx, s, "once", strings.NewReader("1"), 1); err != nil {
				t.Fatal(err)
			}
			if _, err := PutOnce(ctx, s, "once", strings.NewReader("2"), 1); !errors.Is(err, ErrExists) {
				t.Fatalf("second PutOnce = %v, want ErrExists", err)
			}
			if got := read(t, s, "once"); got != "1" {
				t.Fatalf("write-once key was overwritten: %q", got)
			}

			var keys []string
			if err := s.List(ctx, "a/", func(o ObjectInfo) error {
				keys = append(keys, o.Key)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(keys, ",") != "a/b/c,a/unknown" {
				t.Fatalf("List = %v", keys)
			}

			if err := s.Delete(ctx, "a/b/c"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, "a/b/c"); err != nil {
				t.Fatalf("deleting a missing key is not an error: %v", err)
			}

			if rr, ok := s.(RangeReader); ok {
				_ = s.Put(ctx, "r", strings.NewReader("0123456789"), 10)
				rc, err := rr.GetRange(ctx, "r", 3, 4)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(rc)
				rc.Close()
				if string(b) != "3456" {
					t.Fatalf("GetRange = %q", b)
				}
			}
		})
	}
}

func TestRejectsEscapingKeys(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		for _, key := range []string{"../evil", "a/../../evil", "/abs", `a\b`, "a//b", ""} {
			if err := s.Put(ctx, key, strings.NewReader("x"), 1); err == nil {
				t.Fatalf("%s: key %q accepted", name, key)
			}
		}
	}
}

// Only one of many concurrent creators may win.
func TestPutIfAbsentIsExclusive(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]ExclusivePutter{"fs": NewFS(t.TempDir()), "memory": NewMemory()} {
		t.Run(name, func(t *testing.T) {
			var wg sync.WaitGroup
			var mu sync.Mutex
			wins := 0
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := s.PutIfAbsent(ctx, "locks/x", bytes.NewReader([]byte("me")), 2)
					if err == nil {
						mu.Lock()
						wins++
						mu.Unlock()
					} else if !errors.Is(err, ErrExists) {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if wins != 1 {
				t.Fatalf("%d creators won, want exactly 1", wins)
			}
		})
	}
}

func TestLoggedRecordsMutations(t *testing.T) {
	var buf bytes.Buffer
	s := WithLogging(NewMemory(), slog.New(slog.NewJSONHandler(&buf, nil)))
	ctx := context.Background()
	_ = s.Put(ctx, "blobs/x", strings.NewReader("abc"), 3)
	_ = s.Delete(ctx, "blobs/x")
	out := buf.String()
	for _, want := range []string{`"msg":"store.put"`, `"key":"blobs/x"`, `"bytes":3`, `"msg":"store.delete"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s:\n%s", want, out)
		}
	}
}

// A wrapper must not claim a capability its backend lacks.
func TestLoggedForwardsUnsupported(t *testing.T) {
	s := WithLogging(plainStore{NewMemory()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := s.PutIfAbsent(context.Background(), "k", strings.NewReader("x"), 1)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
	atomic, err := PutOnce(context.Background(), s, "k", strings.NewReader("x"), 1)
	if err != nil || atomic {
		t.Fatalf("PutOnce fallback: atomic=%v err=%v", atomic, err)
	}
}

// plainStore hides Memory's optional capabilities.
type plainStore struct{ m *Memory }

func (p plainStore) Get(ctx context.Context, k string) (io.ReadCloser, error) { return p.m.Get(ctx, k) }
func (p plainStore) Put(ctx context.Context, k string, r io.Reader, n int64) error {
	return p.m.Put(ctx, k, r, n)
}
func (p plainStore) Exists(ctx context.Context, k string) (bool, error) { return p.m.Exists(ctx, k) }
func (p plainStore) Delete(ctx context.Context, k string) error         { return p.m.Delete(ctx, k) }
func (p plainStore) List(ctx context.Context, pre string, fn func(ObjectInfo) error) error {
	return p.m.List(ctx, pre, fn)
}
