package hosts

import (
	"context"
	"io"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
)

// storeFake is a Manager that caches images and can fetch them (what
// remote.Manager is).
type storeFake struct {
	poolFake
	manifests, uploads, fetches int
}

func (f *storeFake) ImageManifest(name string) (app.RemoteImage, error) {
	f.manifests++
	return app.RemoteImage{Name: name}, nil
}
func (f *storeFake) UploadImage(context.Context, string, io.Reader, int64, string, func(int64)) error {
	f.uploads++
	return nil
}
func (f *storeFake) FetchFromStore(context.Context, string, string, string, int64, func(int64, int64)) error {
	f.fetches++
	return nil
}

// storeOnlyFake caches images but cannot fetch (a daemon client without S9).
type storeOnlyFake struct{ poolFake }

func (f *storeOnlyFake) ImageManifest(name string) (app.RemoteImage, error) {
	return app.RemoteImage{Name: name}, nil
}
func (f *storeOnlyFake) UploadImage(context.Context, string, io.Reader, int64, string, func(int64)) error {
	return nil
}

// A listed host with a default_pool is wrapped to default its pools; the
// wrapper must still be an app.ImageStore and an app.ImageFetcher, or create
// skips syncing the image to it.
func TestDefaultPoolsKeepsImageStoreAndFetcher(t *testing.T) {
	f := &storeFake{}
	m := withDefaultPools(f, "fast")
	st, ok := m.(app.ImageStore)
	if !ok {
		t.Fatal("wrapped host lost app.ImageStore")
	}
	fe, ok := m.(app.ImageFetcher)
	if !ok {
		t.Fatal("wrapped host lost app.ImageFetcher")
	}
	_, _ = st.ImageManifest("x")
	_ = st.UploadImage(context.Background(), "x", nil, 0, "", nil)
	_ = fe.FetchFromStore(context.Background(), "x", "t", "s", 1, nil)
	if f.manifests != 1 || f.uploads != 1 || f.fetches != 1 {
		t.Fatalf("calls reached the host: manifest %d upload %d fetch %d", f.manifests, f.uploads, f.fetches)
	}
	// Pool defaulting still works through the same value.
	exercise(t, m, "")
	if f.calls[0] != "CloneImage fast" {
		t.Errorf("calls %v", f.calls)
	}
}

// The wrapper claims only what the wrapped host has.
func TestDefaultPoolsClaimsNothingItLacks(t *testing.T) {
	m := withDefaultPools(&poolFake{}, "fast")
	if _, ok := m.(app.ImageStore); ok {
		t.Error("a host without an image cache became an ImageStore")
	}
	if _, ok := m.(app.ImageFetcher); ok {
		t.Error("a host that cannot fetch became an ImageFetcher")
	}
	m = withDefaultPools(&storeOnlyFake{}, "fast")
	if _, ok := m.(app.ImageStore); !ok {
		t.Error("lost ImageStore")
	}
	if _, ok := m.(app.ImageFetcher); ok {
		t.Error("an ImageStore-only host became an ImageFetcher")
	}
}
