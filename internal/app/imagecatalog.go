package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// ErrNoStore is what the local ImageCatalog answers: this command is in
// local mode, so there is no store to list, fetch from or push to. Create
// then uses ImageSource (this machine's pools and image_dir) as before.
var ErrNoStore = errors.New("no store in use: this command is in local mode")

// StoreImage is a base image the store holds.
type StoreImage struct {
	Name    string
	SHA256  string
	Size    int64
	Updated time.Time
}

// ImageTicket is a one-use, 15-minute, read-only download ticket for one
// image at one digest. A daemon presents it to the store to fetch the image
// itself. It is a credential: never print or log Ticket.
type ImageTicket struct {
	Ticket    string
	ExpiresAt time.Time
	Image     string
	SHA256    string
	Size      int64
}

// ImageCatalog is the store's side of base images: metadata, tickets, the
// bytes, and push. ImageSource (FindImage) stays the other side: this
// machine's own files. In local mode the catalog is LocalCatalog and every
// call answers ErrNoStore.
//
// Not-found errors match vmtool.IsNotFound.
type ImageCatalog interface {
	List(ctx context.Context) ([]StoreImage, error)
	Get(ctx context.Context, name string) (*StoreImage, error)
	Delete(ctx context.Context, name string) error
	// Push streams size bytes with SHA-256 sha256 to the store; the store
	// indexes the image only when both match.
	Push(ctx context.Context, name, sha256 string, size int64, r io.Reader) (*StoreImage, error)
	// Open starts a download of the image's bytes (streamed; the caller
	// closes it). The returned StoreImage has the digest and size the store
	// sent, not the Name or Updated.
	Open(ctx context.Context, name string) (io.ReadCloser, StoreImage, error)
	// Ticket mints a download ticket for the image.
	Ticket(ctx context.Context, name string) (*ImageTicket, error)
}

// LocalCatalog is the catalog of local mode: there is none.
type LocalCatalog struct{}

func (LocalCatalog) List(context.Context) ([]StoreImage, error)       { return nil, ErrNoStore }
func (LocalCatalog) Get(context.Context, string) (*StoreImage, error) { return nil, ErrNoStore }
func (LocalCatalog) Delete(context.Context, string) error             { return ErrNoStore }
func (LocalCatalog) Push(context.Context, string, string, int64, io.Reader) (*StoreImage, error) {
	return nil, ErrNoStore
}
func (LocalCatalog) Open(context.Context, string) (io.ReadCloser, StoreImage, error) {
	return nil, StoreImage{}, ErrNoStore
}
func (LocalCatalog) Ticket(context.Context, string) (*ImageTicket, error) { return nil, ErrNoStore }

// StoreImages is the catalog backed by the store: thin calls of
// internal/storeclient.
type StoreImages struct{ Client *storeclient.Client }

func imgOf(i storeclient.Image) StoreImage {
	return StoreImage{Name: i.Name, SHA256: i.Sha256, Size: i.Size, Updated: i.Updated}
}

func imgErr(name string, err error) error {
	if errors.Is(err, storeclient.ErrNotFound) {
		return fmt.Errorf("image %q: %v: %w", name, err, vmtool.ErrNotFound)
	}
	return err
}

func (s StoreImages) List(ctx context.Context) ([]StoreImage, error) {
	l, err := s.Client.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]StoreImage, 0, len(l))
	for _, i := range l {
		out = append(out, imgOf(i))
	}
	return out, nil
}

func (s StoreImages) Get(ctx context.Context, name string) (*StoreImage, error) {
	i, err := s.Client.GetImage(ctx, name)
	if err != nil {
		return nil, imgErr(name, err)
	}
	o := imgOf(*i)
	return &o, nil
}

func (s StoreImages) Delete(ctx context.Context, name string) error {
	return imgErr(name, s.Client.DeleteImage(ctx, name))
}

func (s StoreImages) Push(ctx context.Context, name, sha256 string, size int64, r io.Reader) (*StoreImage, error) {
	i, err := s.Client.PushImage(ctx, name, sha256, size, r)
	if err != nil {
		return nil, err
	}
	o := imgOf(*i)
	return &o, nil
}

func (s StoreImages) Open(ctx context.Context, name string) (io.ReadCloser, StoreImage, error) {
	rc, sha, size, err := s.Client.OpenImage(ctx, name)
	if err != nil {
		return nil, StoreImage{}, imgErr(name, err)
	}
	return rc, StoreImage{Name: name, SHA256: sha, Size: size}, nil
}

func (s StoreImages) Ticket(ctx context.Context, name string) (*ImageTicket, error) {
	t, err := s.Client.CreateImageTicket(ctx, name)
	if err != nil {
		return nil, imgErr(name, err)
	}
	return &ImageTicket{Ticket: t.Ticket, ExpiresAt: t.ExpiresAt, Image: t.Image, SHA256: t.Sha256, Size: t.Size}, nil
}
