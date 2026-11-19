package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func notFound(n string) (string, error) { return "", fmt.Errorf("%s: %w", n, vmtool.ErrNotFound) }

func writeImage(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("placeholder image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocalImagesOrder(t *testing.T) {
	dir := t.TempDir()
	inDir := writeImage(t, dir, "u.qcow2")

	// Pools win over the folder.
	src := LocalImages{Dir: dir, Pools: func(string) (string, error) { return "/pool/u.qcow2", nil }}
	if p, err := src.FindImage("u.qcow2"); err != nil || p != "/pool/u.qcow2" {
		t.Fatalf("pool hit: %q %v", p, err)
	}
	// A pool miss falls to the folder.
	src.Pools = notFound
	if p, err := src.FindImage("u.qcow2"); err != nil || p != inDir {
		t.Fatalf("folder: %q %v", p, err)
	}
	// No pools at all (nil) falls to the folder too.
	if p, err := (LocalImages{Dir: dir}).FindImage("u.qcow2"); err != nil || p != inDir {
		t.Fatalf("nil pools: %q %v", p, err)
	}
	// A pool failure that is not "not found" is not hidden by the folder.
	src.Pools = func(string) (string, error) { return "", errors.New("pool boom") }
	if _, err := src.FindImage("u.qcow2"); err == nil || !strings.Contains(err.Error(), "pool boom") {
		t.Fatalf("pool error: %v", err)
	}
	// Nowhere: not found.
	if _, err := (LocalImages{Dir: dir, Pools: notFound}).FindImage("zz.qcow2"); !vmtool.IsNotFound(err) {
		t.Fatalf("miss: %v", err)
	}
	if _, err := (LocalImages{}).FindImage("u.qcow2"); !vmtool.IsNotFound(err) {
		t.Fatalf("no dir: %v", err)
	}
}

func TestFindImageInDirRules(t *testing.T) {
	dir := t.TempDir()
	plain := writeImage(t, dir, "plain")
	q := writeImage(t, dir, "base.qcow2")
	if err := os.Mkdir(filepath.Join(dir, "adir.qcow2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(q, filepath.Join(dir, "link.qcow2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "broken.qcow2")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeImage(t, outside, "esc.qcow2")

	for name, want := range map[string]string{
		"plain":      plain,
		"base":       q, // .qcow2 appended
		"base.qcow2": q,
		"link":       filepath.Join(dir, "link.qcow2"), // symlink to a regular file
		"link.qcow2": filepath.Join(dir, "link.qcow2"),
	} {
		if p, err := vmtool.FindImageInDir(dir, name); err != nil || p != want {
			t.Errorf("%q: %q %v, want %q", name, p, err, want)
		}
	}
	for _, name := range []string{"adir", "adir.qcow2", "broken", "broken.qcow2", "nope",
		"../" + filepath.Base(outside) + "/esc", "../" + filepath.Base(outside) + "/esc.qcow2",
		"/etc/passwd", "a/b", ".hidden", ""} {
		if p, err := vmtool.FindImageInDir(dir, name); !vmtool.IsNotFound(err) {
			t.Errorf("%q: %q %v, want not found", name, p, err)
		}
	}
}

// create from the folder: hashed and uploaded as before; pools are not
// searched when the lookup says so (Pools nil, as wired for a listed own name).
func TestCreateVMImageFromImageDir(t *testing.T) {
	t.Chdir(t.TempDir())
	cache := filepath.Join(t.TempDir(), vmtool.ImageManifestFile)
	dir := t.TempDir()
	writeImage(t, dir, "u.qcow2")

	poolCalls := 0
	pools := func(n string) (string, error) { poolCalls++; return notFound(n) }

	m := &storeVMs{}
	err := createOn(t, &Service{Images: LocalImages{Dir: dir, Pools: pools}, ImageCachePath: cache}, m)
	if err != nil || m.uploads != 1 || poolCalls != 1 {
		t.Fatalf("folder upload: %v, %d uploads, %d pool calls", err, m.uploads, poolCalls)
	}

	poolCalls = 0
	m = &storeVMs{}
	if err := createOn(t, &Service{Images: LocalImages{Dir: dir}, ImageCachePath: cache}, m); err != nil || m.uploads != 1 || poolCalls != 0 {
		t.Fatalf("no pools: %v, %d uploads, %d pool calls", err, m.uploads, poolCalls)
	}

	// No local copy, nothing cached: the message names image_dir and the host.
	m = &storeVMs{}
	empty := t.TempDir()
	err = createOn(t, &Service{Images: LocalImages{Dir: empty, Pools: notFound}, ImageCachePath: cache}, m)
	if !vmtool.IsNotFound(err) || m.uploads != 0 {
		t.Fatalf("nowhere: %v", err)
	}
	for _, want := range []string{"u.qcow2", "image_dir (" + empty + ")", "veritech01"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	err = createOn(t, &Service{Images: LocalImages{}, ImageCachePath: cache}, &storeVMs{})
	if !vmtool.IsNotFound(err) || !strings.Contains(err.Error(), "image_dir (not set)") {
		t.Fatalf("unset: %v", err)
	}
	// A source with no Searched still gets a message naming the host.
	err = createOn(t, &Service{Images: ImageSourceFunc(notFound), ImageCachePath: cache}, &storeVMs{})
	if !vmtool.IsNotFound(err) || !strings.Contains(err.Error(), "veritech01") {
		t.Fatalf("plain source: %v", err)
	}
}
