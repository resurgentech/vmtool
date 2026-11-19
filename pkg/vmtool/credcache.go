package vmtool

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The credentials cache: primitives only. A VM's folder .machines/<vm>.<host>.vmtool/
// (and a machine's .machines/<name>/) holds password.yml and the key files; in
// store mode it is a cache of the store's record, marked by a .vmtool-source
// file. When to fetch, push or reconcile is policy and lives in internal/app.

const (
	// SourceMarkerFile names the marker in a credentials folder.
	SourceMarkerFile = ".vmtool-source"
	// SourceStore marks a folder that is a cache of the store's record.
	SourceStore = "store"
	// SourcePending marks a folder written here and not (yet) in the store.
	SourcePending = "pending"
	// StaleDir is where a folder that belongs to a VM that was deleted and
	// recreated is moved: .machines/.stale/<folder>-<date>/. Nothing there is
	// ever deleted or pushed.
	StaleDir = ".stale"
)

// SourceMarker is the content of .vmtool-source: "store <uuid>" or
// "pending <uuid>". UUID is the libvirt domain UUID the folder belongs to;
// machines have none, and the line is the bare state.
type SourceMarker struct {
	State string
	UUID  string
}

// ReadSourceMarker reads dir's marker. ok is false when there is none, and
// also when the file says nothing this code knows: a folder with no usable
// marker counts as pending with the UUID unknown, and must not block a
// command.
func ReadSourceMarker(dir string) (m SourceMarker, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, SourceMarkerFile))
	if os.IsNotExist(err) {
		return SourceMarker{}, false, nil
	}
	if err != nil {
		return SourceMarker{}, false, err
	}
	f := strings.Fields(string(b))
	if len(f) < 1 || len(f) > 2 || (f[0] != SourceStore && f[0] != SourcePending) {
		return SourceMarker{}, false, nil
	}
	m.State = f[0]
	if len(f) == 2 {
		m.UUID = f[1]
	}
	return m, true, nil
}

// WriteSourceMarker writes dir's marker (0600, atomically).
func WriteSourceMarker(dir string, m SourceMarker) error {
	if m.State != SourceStore && m.State != SourcePending {
		return fmt.Errorf("invalid credentials marker state %q", m.State)
	}
	line := m.State
	if m.UUID != "" {
		line += " " + m.UUID
	}
	return WriteFileAtomic(filepath.Join(dir, SourceMarkerFile), []byte(line+"\n"), 0o600)
}

// WriteFileAtomic writes data to path through a temporary file in the same
// directory (created 0600, mode set to perm before the data is written) and a
// rename, so a reader sees the old file or the new one and a crash leaves no
// half-written key.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+hex.EncodeToString(rnd[:])+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// EnsureCredsDir makes dir (and its parents) and sets it 0700.
func EnsureCredsDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// LookupVMAuth is ResolveVMAuth without the fallback: the credentials in
// VM name's folder on host, and whether the folder holds any. It tells a
// missing folder (fetch it from the store) from a present one.
func LookupVMAuth(host, name string) (Auth, bool) {
	if validVMKey(host, name) != nil {
		return Auth{}, false
	}
	return loadAuthDir(CredsDir(host, name))
}

// HasVMCreds reports whether VM name's folder on host holds credentials.
func HasVMCreds(host, name string) bool {
	_, ok := LookupVMAuth(host, name)
	return ok
}

// MoveStale moves dir to .machines/.stale/<base>-<date>/ (next to dir's
// parent: <parent>/.stale/), adding -2, -3, ... when that name is taken, and
// returns the new path. Nothing is deleted.
func MoveStale(dir string, now time.Time) (string, error) {
	stale := filepath.Join(filepath.Dir(dir), StaleDir)
	if err := EnsureCredsDir(stale); err != nil {
		return "", err
	}
	base := fmt.Sprintf("%s-%s", filepath.Base(dir), now.Format("2006-01-02"))
	dest := filepath.Join(stale, base)
	for i := 2; ; i++ {
		if _, err := os.Lstat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(stale, fmt.Sprintf("%s-%d", base, i))
	}
	if err := os.Rename(dir, dest); err != nil {
		return "", err
	}
	return dest, nil
}
