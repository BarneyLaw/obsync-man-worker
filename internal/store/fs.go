package store

import (
	"context"
	"errors"
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

// Path resolves a key to a filesystem path, refusing keys that escape Root.
func (f *FS) Path(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	return filepath.Join(f.Root, filepath.FromSlash(key)), nil
}

func (f *FS) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := f.Path(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return fh, err
}

func (f *FS) GetRange(_ context.Context, key string, off, n int64) (io.ReadCloser, error) {
	p, err := f.Path(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
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
func (f *FS) Put(_ context.Context, key string, r io.Reader, size int64) error {
	dst, tmp, err := f.stage(key, r, size)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, dst)
}

// PutIfAbsent stages to a temp file then hard-links it into place. Link fails
// if the destination exists, so creation is exclusive and the object appears
// complete or not at all. Filesystems without hard links fall back to an
// O_EXCL create, which is still exclusive but not atomic for readers.
func (f *FS) PutIfAbsent(_ context.Context, key string, r io.Reader, size int64) error {
	dst, tmp, err := f.stage(key, r, size)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	lerr := os.Link(tmp, dst)
	if lerr == nil {
		return nil
	}
	if errors.Is(lerr, fs.ErrExist) {
		return ErrExists
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	in, err := os.Open(tmp)
	if err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	defer in.Close()
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// stage writes r to a synced temp file beside the destination.
func (f *FS) stage(key string, r io.Reader, size int64) (dst, tmp string, err error) {
	dst, err = f.Path(key)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", "", err
	}
	fh, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return "", "", err
	}
	cr := &countingReader{r: r}
	_, err = io.Copy(fh, cr)
	if err == nil {
		err = checkSize(key, size, cr.n)
	}
	if err == nil {
		err = fh.Sync()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(fh.Name())
		return "", "", err
	}
	return dst, fh.Name(), nil
}

func (f *FS) Exists(_ context.Context, key string) (bool, error) {
	p, err := f.Path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// Stat reports only objects: a directory is not an object, so it is not found.
func (f *FS) Stat(_ context.Context, key string) (ObjectInfo, error) {
	p, err := f.Path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(p)
	if os.IsNotExist(err) || (err == nil && info.IsDir()) {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: info.Size(), Modified: info.ModTime()}, nil
}

func (f *FS) Delete(_ context.Context, key string) error {
	p, err := f.Path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (f *FS) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	root := filepath.Join(f.Root, filepath.FromSlash(prefix))
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		rel, err := filepath.Rel(f.Root, p)
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
