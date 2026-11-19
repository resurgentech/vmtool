package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process BlobStore for tests. It is safe for concurrent
// use and honours the same errors as the S3 implementation. A Put stores
// the object only once the whole body arrived.
type Memory struct {
	mu   sync.RWMutex
	objs map[string]memObj
}

type memObj struct {
	data []byte
	mod  time.Time
}

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{objs: map[string]memObj{}} }

func (m *Memory) EnsureBucket(ctx context.Context) error { return ctx.Err() }

func (m *Memory) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("blob: negative size %d", size)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var buf bytes.Buffer
	fr := newFixedReader(ctx, r, size)
	if _, err := io.Copy(&buf, fr); err != nil {
		if errors.Is(err, ErrShortBody) {
			return ErrShortBody
		}
		return err
	}
	m.mu.Lock()
	m.objs[key] = memObj{data: buf.Bytes(), mod: time.Now()}
	m.mu.Unlock()
	return nil
}

func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, Info{}, err
	}
	o, ok := m.lookup(key)
	if !ok {
		return nil, Info{}, ErrNotFound
	}
	// Stored slices are never modified, so sharing one is safe.
	return io.NopCloser(bytes.NewReader(o.data)), Info{Size: int64(len(o.data)), ModTime: o.mod}, nil
}

func (m *Memory) Stat(ctx context.Context, key string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	o, ok := m.lookup(key)
	if !ok {
		return Info{}, ErrNotFound
	}
	return Info{Size: int64(len(o.data)), ModTime: o.mod}, nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.objs, key)
	m.mu.Unlock()
	return nil
}

func (m *Memory) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *Memory) lookup(key string) (memObj, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objs[key]
	return o, ok
}

var _ BlobStore = (*Memory)(nil)
