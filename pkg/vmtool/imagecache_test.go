package vmtool

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestValidImageName(t *testing.T) {
	for _, ok := range []string{"ubuntu.qcow2", "a", "Ubuntu-24.04_x.img", "a..b"} {
		if !ValidImageName(ok) {
			t.Errorf("ValidImageName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", ".x.part", "a/b", "../x", "a\x00b", "a b", strings.Repeat("a", 201)} {
		if ValidImageName(bad) {
			t.Errorf("ValidImageName(%q) = true", bad)
		}
	}
}

func TestImageManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ImageManifestFile)
	m := OpenImageManifest(path)
	if _, ok, err := m.Get("a.qcow2"); err != nil || ok {
		t.Fatalf("Get on a missing file = %v, %v", ok, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Get created the file: %v", err)
	}
	if err := m.Update(func(es map[string]ImageEntry) error {
		es["a.qcow2"] = ImageEntry{SHA256: sum([]byte("a")), Size: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A second handle on the same path shares the lock and sees the entry.
	e, ok, err := OpenImageManifest(path).Get("a.qcow2")
	if err != nil || !ok || e.SHA256 != sum([]byte("a")) || e.Size != 1 {
		t.Fatalf("Get = %+v %v %v", e, ok, err)
	}
	if OpenImageManifest(path) != m {
		t.Error("OpenImageManifest does not share one handle per path")
	}
	// Atomic write leaves no temporary file behind.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("dir holds %v", ents)
	}
	// Concurrent updates do not lose entries.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.Update(func(es map[string]ImageEntry) error {
				es[string(rune('b'+i))+".img"] = ImageEntry{Size: int64(i)}
				return nil
			})
		}(i)
	}
	wg.Wait()
	all, err := m.Load()
	if err != nil || len(all) != 21 {
		t.Fatalf("Load = %d entries, %v", len(all), err)
	}
	// A corrupt manifest is an error, never an empty manifest.
	if err := os.WriteFile(path, []byte("{not yaml: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Get("a.qcow2"); err == nil {
		t.Error("corrupt manifest read as valid")
	}
}

func TestLocalImageInfoRehashesOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "u.qcow2")
	if err := os.WriteFile(img, []byte("hello image"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := OpenImageManifest(filepath.Join(dir, ImageManifestFile))
	hashes := 0
	onHash := func() { hashes++ }

	e, err := LocalImageInfo(cache, "u.qcow2", img, onHash)
	if err != nil || e.SHA256 != sum([]byte("hello image")) || e.Size != 11 || hashes != 1 {
		t.Fatalf("first = %+v, %v, hashes %d", e, err, hashes)
	}
	if _, err := LocalImageInfo(cache, "u.qcow2", img, onHash); err != nil || hashes != 1 {
		t.Fatalf("unchanged file re-hashed (%d) %v", hashes, err)
	}
	// Same bytes, new mtime: re-hash, same digest.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(img, later, later); err != nil {
		t.Fatal(err)
	}
	e, err = LocalImageInfo(cache, "u.qcow2", img, onHash)
	if err != nil || hashes != 2 || e.SHA256 != sum([]byte("hello image")) {
		t.Fatalf("after mtime change: %+v %v hashes %d", e, err, hashes)
	}
	if _, err := LocalImageInfo(cache, "u.qcow2", img, onHash); err != nil || hashes != 2 {
		t.Fatalf("cache not updated after re-hash (%d) %v", hashes, err)
	}
	// New size: re-hash.
	if err := os.WriteFile(img, []byte("hello image, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err = LocalImageInfo(cache, "u.qcow2", img, onHash)
	if err != nil || hashes != 3 || e.SHA256 != sum([]byte("hello image, longer")) || e.Size != 19 {
		t.Fatalf("after size change: %+v %v hashes %d", e, err, hashes)
	}
	// The cache records the mtime on the workstation.
	got, ok, _ := cache.Get("u.qcow2")
	if !ok || got.MTime.IsZero() || got.Path != img {
		t.Errorf("cache entry %+v", got)
	}
}
