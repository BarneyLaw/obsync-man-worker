package main

import (
	"context"
	"errors"
	"io"

	"github.com/leifsen/obsync/internal/store"
)

// objectReader is an io.ReadSeeker over a stored object, so http.ServeContent
// can answer Range, HEAD and conditional requests for any backend.
//
// Seeking is free; the object is opened lazily at the current offset and read
// sequentially to the end. ServeContent seeks once per requested range and then
// copies, so a Range request costs one ranged GET against S3, never one per
// buffer.
type objectReader struct {
	ctx  context.Context
	rr   store.RangeReader
	key  string
	size int64

	off  int64
	body io.ReadCloser
}

func (o *objectReader) Read(p []byte) (int, error) {
	if o.off >= o.size {
		return 0, io.EOF
	}
	if o.body == nil {
		b, err := o.rr.GetRange(o.ctx, o.key, o.off, o.size-o.off)
		if err != nil {
			return 0, err
		}
		o.body = b
	}
	n, err := o.body.Read(p)
	o.off += int64(n)
	if errors.Is(err, io.EOF) && o.off < o.size {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (o *objectReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = o.off + offset
	case io.SeekEnd:
		abs = o.size + offset
	default:
		return 0, errors.New("objectReader: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("objectReader: negative position")
	}
	if abs != o.off {
		o.Close()
	}
	o.off = abs
	return abs, nil
}

func (o *objectReader) Close() error {
	if o.body == nil {
		return nil
	}
	err := o.body.Close()
	o.body = nil
	return err
}
