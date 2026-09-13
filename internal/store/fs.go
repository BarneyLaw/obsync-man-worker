package store

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FS is a filesystem-backed Store. Build against this first: the entire
// pipeline runs into a local directory with no Garage in sight, and swapping to
// S3 later changes nothing above this line.
type FS struct{ Root string }

func NewFS(root string) *FS { return &FS{Root: root} }

func (f *FS) path(key string) string {
	return filepath.Join(f.Root, filepath.FromSlash(key))
}

func (f *FS) Get(_ context.Context, key string) (io.ReadCloser, error) {
	fh, err := os.Open(f.path(key))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return fh, err
}

func (f *FS) GetRange(_ context.Context, key string, off, n int64) (io.ReadCloser, error) {
	fh, err := os.Open(f.path(key))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		fh.Close()
		return nil, err
	}
	return readCloser{Reader: io.LimitReader(fh, n), Closer: fh}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Put writes to a temp file and renames. Same directory, so the rename is
// atomic and a crash mid-write never leaves a half-object at the real key.
func (f *FS) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	dst := f.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (f *FS) Exists(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(f.path(key))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (f *FS) Delete(_ context.Context, key string) error {
	err := os.Remove(f.path(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (f *FS) List(_ context.Context, prefix string, fn func(ObjectInfo) error) error {
	root := filepath.Join(f.Root, filepath.FromSlash(prefix))
	base := f.Root
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(ObjectInfo{
			Key:      filepath.ToSlash(rel),
			Size:     info.Size(),
			Modified: info.ModTime(),
		})
	})
}
