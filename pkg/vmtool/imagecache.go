package vmtool

// Image cache manifests. Both ends of an image upload keep an
// image-manifest.yml in vmtool's working directory, mapping an image name to
// its SHA-256 and size:
//
//   - On a hypervisor daemon it is the ownership record: the daemon owns (may
//     replace or delete) a volume of its `default` pool only if this manifest
//     lists that name. Anything else of the same name is foreign and is never
//     touched (internal/server/images.go).
//   - On the workstation it is a hash cache that also records each image's
//     path and mtime, so an unchanged image is not re-hashed.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ImageManifestFile is the manifest's name in vmtool's working directory.
const ImageManifestFile = "image-manifest.yml"

// maxImageName keeps ".<name>.part" within a 255-byte file name.
const maxImageName = 200

// imageNameRE is a plain file name: no separator, no leading dot (so it is
// never ".", "..", or a ".<name>.part" temporary file), nothing a shell or
// libvirt XML would interpret.
var imageNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidImageName reports whether name may be used as an image file name in a
// pool directory.
func ValidImageName(name string) bool {
	return len(name) <= maxImageName && imageNameRE.MatchString(name)
}

// ImageEntry is one image's manifest record. MTime and Path are set only on
// the workstation (the hash cache).
type ImageEntry struct {
	SHA256 string    `yaml:"sha256"`
	Size   int64     `yaml:"size"`
	MTime  time.Time `yaml:"mtime,omitempty"`
	Path   string    `yaml:"path,omitempty"`
}

// ImageManifest is a manifest file. Every read goes to the file; writes are
// atomic (temporary file + rename) and serialized within this process by a
// lock shared by every handle on the same path (OpenImageManifest). Another
// process writing the same file concurrently can lose an update; vmtool has
// one daemon per hypervisor, and the workstation file is only a cache.
type ImageManifest struct {
	path string
	mu   sync.Mutex
}

var (
	manifestsMu sync.Mutex
	manifests   = map[string]*ImageManifest{}
)

// OpenImageManifest returns the handle for path (relative paths are taken
// against the working directory now). It does not touch the file.
func OpenImageManifest(path string) *ImageManifest {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	manifestsMu.Lock()
	defer manifestsMu.Unlock()
	if m, ok := manifests[path]; ok {
		return m
	}
	m := &ImageManifest{path: path}
	manifests[path] = m
	return m
}

// Path is the manifest file's absolute path.
func (m *ImageManifest) Path() string { return m.path }

// Lock and Unlock hold the manifest's lock, for a caller that must keep the
// manifest unchanged across several steps (Load and Update must not be called
// while holding it; use LoadLocked and SaveLocked).
func (m *ImageManifest) Lock()   { m.mu.Lock() }
func (m *ImageManifest) Unlock() { m.mu.Unlock() }

// Load reads every entry. A missing file is an empty manifest; an unreadable
// or corrupt one is an error, never an empty manifest.
func (m *ImageManifest) Load() (map[string]ImageEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.LoadLocked()
}

// LoadLocked is Load for a caller holding the lock.
func (m *ImageManifest) LoadLocked() (map[string]ImageEntry, error) {
	b, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return map[string]ImageEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", m.path, err)
	}
	out := map[string]ImageEntry{}
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.path, err)
	}
	if out == nil {
		out = map[string]ImageEntry{}
	}
	return out, nil
}

// Get returns name's entry and whether the manifest lists it.
func (m *ImageManifest) Get(name string) (ImageEntry, bool, error) {
	all, err := m.Load()
	if err != nil {
		return ImageEntry{}, false, err
	}
	e, ok := all[name]
	return e, ok, nil
}

// Update loads the manifest, lets fn change the entries and, if anything
// changed, writes it back atomically, all under the lock. If fn fails nothing
// is written.
func (m *ImageManifest) Update(fn func(map[string]ImageEntry) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.LoadLocked()
	if err != nil {
		return err
	}
	before := make(map[string]ImageEntry, len(all))
	for k, v := range all {
		before[k] = v
	}
	if err := fn(all); err != nil {
		return err
	}
	if reflect.DeepEqual(before, all) {
		return nil
	}
	return m.SaveLocked(all)
}

// SaveLocked writes entries atomically, for a caller holding the lock: a
// temporary file in the same directory, synced, then renamed over the
// manifest. Mode 0644: it holds no secrets.
func (m *ImageManifest) SaveLocked(entries map[string]ImageEntry) error {
	b, err := yaml.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode %s: %w", m.path, err)
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), "."+filepath.Base(m.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", m.path, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write %s: %w", m.path, err)
	}
	if err := f.Chmod(0o644); err != nil {
		return fmt.Errorf("write %s: %w", m.path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", m.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", m.path, err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("write %s: %w", m.path, err)
	}
	ok = true
	return nil
}

// HashFile returns the lowercase hex SHA-256 and the size of the file at path.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// LocalImageInfo is the SHA-256 and size of the workstation's image name at
// path. The cache's entry is used when its path, size and mtime still match
// the file; otherwise the file is hashed (onHash, if set, is called first)
// and the cache updated. A cache that cannot be read or written only costs a
// re-hash.
func LocalImageInfo(cache *ImageManifest, name, path string, onHash func()) (ImageEntry, error) {
	st, err := os.Stat(path)
	if err != nil {
		return ImageEntry{}, err
	}
	if !st.Mode().IsRegular() {
		return ImageEntry{}, fmt.Errorf("image %s is not a regular file", path)
	}
	if cache != nil {
		if e, ok, err := cache.Get(name); err == nil && ok && e.SHA256 != "" &&
			e.Path == path && e.Size == st.Size() && e.MTime.Equal(st.ModTime()) {
			return e, nil
		}
	}
	if onHash != nil {
		onHash()
	}
	sum, n, err := HashFile(path)
	if err != nil {
		return ImageEntry{}, err
	}
	e := ImageEntry{SHA256: sum, Size: n, MTime: st.ModTime(), Path: path}
	// Cache only if the file did not change while it was being hashed.
	if after, err := os.Stat(path); err == nil && cache != nil &&
		after.Size() == n && after.ModTime().Equal(st.ModTime()) {
		_ = cache.Update(func(es map[string]ImageEntry) error {
			es[name] = e
			return nil
		})
	}
	return e, nil
}

// RefreshPool rescans the named storage pool so libvirt sees files written
// into its directory behind its back (an uploaded image).
func (m *Manager) RefreshPool(name string) error {
	p, err := m.conn.LookupStoragePoolByName(name)
	if err != nil {
		return fmt.Errorf("looking up pool %q: %w", name, err)
	}
	defer p.Free()
	if err := p.Refresh(0); err != nil {
		return fmt.Errorf("refreshing pool %q: %w", name, err)
	}
	return nil
}
