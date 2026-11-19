package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// storeVMs is createVMs whose host caches images.
type storeVMs struct {
	createVMs
	remote    RemoteImage
	uploads   int
	imagePath int
}

func (s *storeVMs) ImageManifest(name string) (RemoteImage, error) { return s.remote, nil }
func (s *storeVMs) UploadImage(_ context.Context, _ string, r io.Reader, size int64, _ string, progress func(int64)) error {
	s.uploads++
	n, err := io.Copy(io.Discard, r)
	if progress != nil {
		progress(n)
	}
	if err != nil || n != size {
		return fmt.Errorf("read %d of %d: %v", n, size, err)
	}
	return nil
}
func (s *storeVMs) ImagePath(name string) (string, error) {
	s.imagePath++
	return s.createVMs.ImagePath(name)
}

func createOn(t *testing.T, svc *Service, m Manager) error {
	t.Helper()
	_, err := stubSvc(t, svc).CreateVM(context.Background(), m, CreateVMInput{Host: "veritech01", Name: "web", Image: "u.qcow2"})
	return err
}

func TestCreateVMImageStore(t *testing.T) {
	t.Chdir(t.TempDir())
	cache := filepath.Join(t.TempDir(), vmtool.ImageManifestFile)
	local := filepath.Join(t.TempDir(), "u.qcow2")
	if err := os.WriteFile(local, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	found := func(string) (string, error) { return local, nil }
	missing := func(n string) (string, error) { return "", fmt.Errorf("%s: %w", n, vmtool.ErrNotFound) }

	// Foreign: conflict, no upload, no libvirt work.
	m := &storeVMs{remote: RemoteImage{Foreign: true, Path: "/var/lib/libvirt/images/u.qcow2"}}
	err := createOn(t, &Service{Images: ImageSourceFunc(found), ImageCachePath: cache}, m)
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindConflict || !strings.Contains(err.Error(), "/var/lib/libvirt/images/u.qcow2") {
		t.Fatalf("foreign: %v", err)
	}
	if m.uploads != 0 || m.imagePath != 0 {
		t.Fatalf("foreign: %d uploads, %d ImagePath", m.uploads, m.imagePath)
	}

	// Absent on the host: upload, then create.
	m = &storeVMs{}
	if err := createOn(t, &Service{Images: ImageSourceFunc(found), ImageCachePath: cache}, m); err != nil || m.uploads != 1 {
		t.Fatalf("absent: %v, %d uploads", err, m.uploads)
	}

	// No local copy, host has it cached: use it.
	m = &storeVMs{remote: RemoteImage{SHA256: "ab", Size: 9}}
	if err := createOn(t, &Service{Images: ImageSourceFunc(missing), ImageCachePath: cache}, m); err != nil || m.uploads != 0 {
		t.Fatalf("cached only: %v, %d uploads", err, m.uploads)
	}
	if err := createOn(t, &Service{ImageCachePath: cache}, m); err != nil || m.uploads != 0 {
		t.Fatalf("no Images: %v, %d uploads", err, m.uploads)
	}

	// Neither: not found, before libvirt.
	m = &storeVMs{}
	err = createOn(t, &Service{Images: ImageSourceFunc(missing), ImageCachePath: cache}, m)
	if !vmtool.IsNotFound(err) || m.imagePath != 0 {
		t.Fatalf("nowhere: %v, ImagePath %d", err, m.imagePath)
	}

	// A local lookup failure is not "absent".
	m = &storeVMs{remote: RemoteImage{SHA256: "ab", Size: 9}}
	err = createOn(t, &Service{Images: ImageSourceFunc(func(string) (string, error) { return "", errors.New("libvirt down") }), ImageCachePath: cache}, m)
	if err == nil || !strings.Contains(err.Error(), "libvirt down") || m.imagePath != 0 {
		t.Fatalf("lookup failure: %v", err)
	}

	// A local libvirt Manager (not an ImageStore) is unaffected.
	if err := createOn(t, &Service{Images: ImageSourceFunc(found)}, &createVMs{}); err != nil {
		t.Fatalf("local host: %v", err)
	}
}
