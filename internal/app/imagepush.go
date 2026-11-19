package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// errNoStore is ErrNoStore with what to do about it, for a command that
// needs the store (images push, pull, list --store).
func errNoStore(op string) error {
	return &Error{Kind: KindInvalid, Op: op,
		Err: fmt.Errorf("%w; set server: in config.yml and VMTOOL_SERVER_TOKEN, and the store must answer", ErrNoStore)}
}

// storeCatalog is this command's catalog when it is in store mode.
func (s *Service) storeCatalog(ctx context.Context, op string) (ImageCatalog, error) {
	if s.Store.Mode(ctx) != ModeStore {
		return nil, errNoStore(op)
	}
	return s.Store.CatalogFor(ctx), nil
}

// ListStoreImages lists the images the store holds. Store mode only.
func (s *Service) ListStoreImages(ctx context.Context) ([]StoreImage, error) {
	cat, err := s.storeCatalog(ctx, "images list")
	if err != nil {
		return nil, err
	}
	l, err := cat.List(ctx)
	if err != nil {
		return nil, wrap("images list", err)
	}
	return l, nil
}

// progressReader reports the bytes read so far.
type progressReader struct {
	r        io.Reader
	n        int64
	total    int64
	progress func(done, total int64)
	step     int64
	next     int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.progress != nil && (p.n >= p.next || p.n == p.total) {
		p.next = p.n + p.step
		p.progress(p.n, p.total)
	}
	return n, err
}

// PushImage puts the file at path into the store as name (default: the file's
// base name). The store's API takes the digest before the bytes, so the file
// is hashed first (through the workstation's size-and-mtime hash cache, so a
// file already hashed by a create is not hashed again) and then streamed once;
// the store verifies the stream against the digest and size before indexing.
// onHash, if set, is called before a hash that is not cached. progress is
// called with the bytes streamed so far.
func (s *Service) PushImage(ctx context.Context, path, name string, onHash func(), progress func(sent, total int64)) (*StoreImage, error) {
	if name == "" {
		name = filepath.Base(path)
	}
	if !vmtool.ValidImageName(name) {
		return nil, invalid("images push", fmt.Errorf("invalid image name %q: use a plain file name (letters, digits, '.', '_', '-')", name))
	}
	cat, err := s.storeCatalog(ctx, "images push")
	if err != nil {
		return nil, err
	}
	cachePath := s.ImageCachePath
	if cachePath == "" {
		cachePath = vmtool.ImageManifestFile
	}
	info, err := vmtool.LocalImageInfo(vmtool.OpenImageManifest(cachePath), name, path, onHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &Error{Kind: KindNotFound, Op: "images push", Err: fmt.Errorf("%s: %w", path, vmtool.ErrNotFound)}
		}
		return nil, wrap("images push", err)
	}
	if info.Size <= 0 {
		return nil, invalid("images push", fmt.Errorf("%s is empty", path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, wrap("images push", err)
	}
	defer f.Close()
	body := &progressReader{r: io.LimitReader(f, info.Size), total: info.Size, progress: progress, step: uploadStep(info.Size)}
	img, err := cat.Push(ctx, name, info.SHA256, info.Size, body)
	if err != nil {
		return nil, badGateway("images push", err)
	}
	return img, nil
}

// PullImage writes the store's image name to dest. The bytes go to a
// ".<file>.part" beside dest and are checked against the digest and size the
// store sent before the rename, so dest is never a partial or wrong file. An
// existing dest is refused unless overwrite. progress gets the bytes written.
func (s *Service) PullImage(ctx context.Context, name, dest string, overwrite bool, progress func(done, total int64)) (*StoreImage, error) {
	if dest == "" {
		return nil, invalid("images pull", errors.New("no destination"))
	}
	cat, err := s.storeCatalog(ctx, "images pull")
	if err != nil {
		return nil, err
	}
	if !overwrite {
		if _, err := os.Lstat(dest); err == nil {
			return nil, conflict("images pull", fmt.Errorf("%s already exists; remove it, pick another --to, or pass --force", dest))
		}
	}
	rc, meta, err := cat.Open(ctx, name)
	if err != nil {
		if vmtool.IsNotFound(err) {
			return nil, &Error{Kind: KindNotFound, Op: "images pull", Err: err}
		}
		return nil, badGateway("images pull", err)
	}
	defer rc.Close()
	want := strings.ToLower(meta.SHA256)
	if want == "" {
		return nil, badGateway("images pull", errors.New("the store sent no SHA-256 for the image"))
	}
	part := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".part")
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, wrap("images pull", err)
	}
	done := false
	defer func() {
		f.Close()
		if !done {
			os.Remove(part)
		}
	}()
	h := sha256.New()
	src := &progressReader{r: io.TeeReader(rc, h), total: meta.Size, progress: progress, step: uploadStep(max(meta.Size, 1))}
	n, err := io.Copy(f, src)
	if err != nil {
		return nil, badGateway("images pull", fmt.Errorf("download %s: %w", name, err))
	}
	if meta.Size >= 0 && n != meta.Size {
		return nil, badGateway("images pull", fmt.Errorf("download %s: got %d bytes, the store said %d", name, n, meta.Size))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return nil, badGateway("images pull", fmt.Errorf("download %s: SHA-256 is %s, the store said %s", name, got, want))
	}
	if err := f.Sync(); err != nil {
		return nil, wrap("images pull", err)
	}
	if err := f.Close(); err != nil {
		return nil, wrap("images pull", err)
	}
	if err := os.Rename(part, dest); err != nil {
		return nil, wrap("images pull", err)
	}
	done = true
	return &StoreImage{Name: name, SHA256: want, Size: n}, nil
}
