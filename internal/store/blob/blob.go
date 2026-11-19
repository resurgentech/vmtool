// Package blob is the store's object storage: base images and, later,
// encrypted database backups. BlobStore is the small interface the store's
// handlers use; S3 is the MinIO-backed implementation and Memory is the
// in-process double that handler tests use.
//
// Both implementations return the same sentinel errors, checkable with
// errors.Is:
//
//   - ErrNotFound: Get or Stat of a key that does not exist (or whose
//     bucket does not).
//   - ErrShortBody: Put's reader ended before size bytes were read. No
//     object is stored and any previous object under the key is kept.
//
// Delete of a missing key is not an error (idempotent): S3's DeleteObject
// already behaves so, and the double matches it.
//
// Put reads exactly size bytes from r; bytes beyond size are not read. A
// cancelled context stops a Put and returns the context's error.
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound means the key (or bucket) does not exist.
	ErrNotFound = errors.New("blob: not found")
	// ErrShortBody means the reader given to Put ended before size bytes.
	ErrShortBody = errors.New("blob: body shorter than declared size")
)

// Info describes a stored object.
type Info struct {
	Size    int64
	ModTime time.Time
}

// BlobStore is the object storage the store's handlers use.
type BlobStore interface {
	// Put streams size bytes from r to key, replacing any existing object.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens key. The caller closes the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, Info, error)
	// Stat returns key's metadata.
	Stat(ctx context.Context, key string) (Info, error)
	// Delete removes key; a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List returns the keys that start with prefix, sorted by name. An
	// empty result is not an error.
	List(ctx context.Context, prefix string) ([]string, error)
	// EnsureBucket makes the bucket if it does not exist. Idempotent.
	EnsureBucket(ctx context.Context) error
}

// readChunk caps one Read so a context cancel is noticed within 32 KiB.
const readChunk = 32 << 10

// fixedReader yields exactly size bytes of r. It checks ctx before each
// read, and when r ends early it sets short and returns ErrShortBody rather
// than io.EOF, so a short body can never look like a complete one.
type fixedReader struct {
	ctx    context.Context
	r      io.Reader
	remain int64
	short  bool
}

func newFixedReader(ctx context.Context, r io.Reader, size int64) *fixedReader {
	return &fixedReader{ctx: ctx, r: r, remain: size}
}

func (f *fixedReader) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	if f.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > f.remain {
		p = p[:f.remain]
	}
	if len(p) > readChunk {
		p = p[:readChunk]
	}
	n, err := f.r.Read(p)
	f.remain -= int64(n)
	if err == io.EOF {
		if f.remain > 0 {
			f.short = true
			return n, ErrShortBody
		}
		return n, io.EOF
	}
	return n, err
}
