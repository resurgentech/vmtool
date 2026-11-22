package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/config"
)

var (
	pushKeep = map[string]bool{"vmtool images push": true}
	pullKeep = map[string]bool{"vmtool images pull": true}
	listKeep = map[string]bool{"vmtool images list": true}
)

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// images push streams the file to the store under --name (default: the base
// name); the store verified digest and size (the double refuses a mismatch).
func TestImagesPushToStore(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	data := []byte("PLACEHOLDER image bytes")
	file := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	r := runGate(t, dir, pushKeep, "images", "push", file)
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	if string(s.images["base.qcow2"]) != string(data) || s.pushes.Load() != 1 {
		t.Fatalf("store has %q after %d pushes", s.images["base.qcow2"], s.pushes.Load())
	}
	if !strings.Contains(r.out, sumHex(data)) {
		t.Errorf("output %q lacks the digest", r.out)
	}
	r = runGate(t, dir, pushKeep, "images", "push", file, "--name", "other.qcow2")
	if r.err != nil || string(s.images["other.qcow2"]) != string(data) {
		t.Fatalf("--name: %v", r.err)
	}
	r = runGate(t, dir, pushKeep, "images", "push", file, "--name", "../evil")
	if r.err == nil || !strings.Contains(r.err.Error(), "invalid image name") {
		t.Fatalf("bad name accepted: %v", r.err)
	}
	r = runGate(t, dir, pushKeep, "images", "push", filepath.Join(t.TempDir(), "missing.qcow2"))
	if r.err == nil || s.pushes.Load() != 2 {
		t.Fatalf("missing file: %v", r.err)
	}
}

// images pull writes the file only after the size and SHA-256 check, to
// --to or the default, and refuses to replace a file without --force.
func TestImagesPullFromStore(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	data := []byte("PLACEHOLDER pulled image bytes")
	s.images["base.qcow2"], s.updated["base.qcow2"] = data, time.Now()

	to := filepath.Join(t.TempDir(), "out.qcow2")
	r := runGate(t, dir, pullKeep, "images", "pull", "base.qcow2", "--to", to)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := os.ReadFile(to); string(got) != string(data) {
		t.Fatalf("file %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(to), ".*.part")); len(left) != 0 {
		t.Fatalf("leftover %v", left)
	}

	// Refuses to overwrite; --force replaces.
	r = runGate(t, dir, pullKeep, "images", "pull", "base.qcow2", "--to", to)
	if r.err == nil || !strings.Contains(r.err.Error(), "already exists") {
		t.Fatalf("overwrite: %v", r.err)
	}
	if err := os.WriteFile(to, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r = runGate(t, dir, pullKeep, "images", "pull", "base.qcow2", "--to", to, "--force"); r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := os.ReadFile(to); string(got) != string(data) {
		t.Fatalf("force: %q", got)
	}

	// Default destination: ./<name> (no image_dir).
	cwd, _ := os.Getwd()
	if r = runGate(t, dir, pullKeep, "images", "pull", "base.qcow2"); r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := os.ReadFile(filepath.Join(cwd, "base.qcow2")); string(got) != string(data) {
		t.Fatalf("default dest: %q", got)
	}

	// Not in the store.
	r = runGate(t, dir, pullKeep, "images", "pull", "nope.qcow2", "--to", filepath.Join(t.TempDir(), "n"))
	if r.err == nil || !strings.Contains(r.err.Error(), "not found") {
		t.Fatalf("missing: %v", r.err)
	}
}

// A blob that does not match the digest the store sent is never renamed into
// place and leaves no .part behind.
func TestImagesPullRejectsCorruptDownload(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	s.images["base.qcow2"], s.updated["base.qcow2"] = []byte("PLACEHOLDER pulled image bytes"), time.Now()
	s.corrupt.Store(true)
	outDir := t.TempDir()
	to := filepath.Join(outDir, "out.qcow2")
	r := runGate(t, dir, pullKeep, "images", "pull", "base.qcow2", "--to", to)
	if r.err == nil || !strings.Contains(r.err.Error(), "SHA-256") {
		t.Fatalf("corrupt download accepted: %v", r.err)
	}
	if ents, _ := os.ReadDir(outDir); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

// With image_dir set, the default destination is image_dir/<name>.
func TestImagesPullDefaultsToImageDir(t *testing.T) {
	s := newCLIStore(t)
	t.Setenv(config.TokenEnv, gateToken)
	t.Setenv(config.ServerTokenEnv, storeTok)
	t.Chdir(t.TempDir())
	imgDir := t.TempDir()
	dir := writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\nimage_dir: "+imgDir+"\n"+s.cfg(), "")
	s.images["base.qcow2"], s.updated["base.qcow2"] = []byte("PLACEHOLDER"), time.Now()
	r := runGate(t, dir, pullKeep, "images", "pull", "base.qcow2")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := os.ReadFile(filepath.Join(imgDir, "base.qcow2")); string(got) != "PLACEHOLDER" {
		t.Fatalf("image_dir copy %q", got)
	}
}

// images list --store lists the store; without --store the command does not
// touch the store.
func TestImagesListStore(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	s.images["b.qcow2"], s.updated["b.qcow2"] = []byte("bb"), time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	s.images["a.qcow2"], s.updated["a.qcow2"] = []byte("aaaa"), time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	r := runGate(t, dir, listKeep, "images", "list", "--store")
	if r.err != nil {
		t.Fatal(r.err)
	}
	got := norm(r.out)
	want := "NAME SIZE SHA256 UPDATED\n" +
		"a.qcow2 4 " + sumHex([]byte("aaaa"))[:12] + " 2026-11-17 09:30\n" +
		"b.qcow2 2 " + sumHex([]byte("bb"))[:12] + " 2026-11-18 10:00\n"
	if got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

// The three store commands in local mode (no server, or the store down) fail
// with a clear line and send nothing.
func TestImagesStoreCommandsNeedAStore(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\n", "")
	file := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"images", "push", file},
		{"images", "pull", "base.qcow2", "--to", filepath.Join(t.TempDir(), "o")},
		{"images", "list", "--store"},
	} {
		r := runGate(t, dir, map[string]bool{"vmtool images push": true, "vmtool images pull": true, "vmtool images list": true}, args...)
		if r.err == nil || !strings.Contains(r.err.Error(), "no store in use") {
			t.Errorf("%v: err %v", args, r.err)
		}
	}
	// The store is configured but down: the one fallback line, then the refusal.
	s := newCLIStore(t)
	s.down.Store(true)
	dir = storeWorkdir(t, s, storeTok)
	r := runGate(t, dir, pushKeep, "images", "push", file)
	if r.err == nil || !strings.Contains(r.err.Error(), "no store in use") || !strings.Contains(r.errOut, "unreachable") {
		t.Errorf("store down: err %v stderr %q", r.err, r.errOut)
	}
	if s.pushes.Load() != 0 {
		t.Error("pushed to a store that was down")
	}
}
