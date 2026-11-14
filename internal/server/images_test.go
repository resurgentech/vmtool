package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/remote"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// poolMgr is a fake hypervisor libvirt whose storage pools are temp
// directories. Like libvirt, it only knows the volumes it has seen: a file
// written into a pool directory is invisible to ImagePath and DeleteImage
// until the pool is refreshed (or the test marks it known). Everything that
// is not about images is rtMgr's.
type poolMgr struct {
	*rtMgr
	pmu       sync.Mutex
	dirs      map[string]string          // pool name -> directory
	known     map[string]map[string]bool // pool name -> volume names libvirt knows
	deletes   []string
	refreshes []string
}

func newPoolMgr(t *testing.T) *poolMgr {
	t.Helper()
	rt := newRTMgr(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	return &poolMgr{
		rtMgr: rt,
		dirs:  map[string]string{"default": rt.pool, "other": other},
		known: map[string]map[string]bool{"default": {}, "other": {}},
	}
}

func (p *poolMgr) ListPools() ([]vmtool.PoolInfo, error) {
	return []vmtool.PoolInfo{
		{Name: "default", Path: p.dirs["default"], Active: true},
		{Name: "other", Path: p.dirs["other"], Active: true},
	}, nil
}

func (p *poolMgr) ImagePath(n string) (string, error) {
	p.rec("ImagePath %s", n)
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for _, pool := range []string{"default", "other"} {
		if p.known[pool][n] {
			return filepath.Join(p.dirs[pool], n), nil
		}
	}
	return "", fmt.Errorf("image %q not found in any storage pool: %w", n, vmtool.ErrNotFound)
}

func (p *poolMgr) DeleteImage(n, pool string) error {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.deletes = append(p.deletes, n+" "+pool)
	if !p.known[pool][n] {
		return fmt.Errorf("image %q not found in pool %q: %w", n, pool, vmtool.ErrNotFound)
	}
	delete(p.known[pool], n)
	return os.Remove(filepath.Join(p.dirs[pool], n))
}

func (p *poolMgr) RefreshPool(pool string) error {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.refreshes = append(p.refreshes, pool)
	ents, err := os.ReadDir(p.dirs[pool])
	if err != nil {
		return err
	}
	p.known[pool] = map[string]bool{}
	for _, e := range ents {
		p.known[pool][e.Name()] = true
	}
	return nil
}

// put writes a file into pool and, when known, tells the fake libvirt.
func (p *poolMgr) put(t *testing.T, pool, name string, data []byte, known bool) string {
	t.Helper()
	path := filepath.Join(p.dirs[pool], name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if known {
		p.pmu.Lock()
		p.known[pool][name] = true
		p.pmu.Unlock()
	}
	return path
}

func (p *poolMgr) deleted() []string {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return append([]string(nil), p.deletes...)
}

var _ app.Manager = (*poolMgr)(nil)

func hexSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// imgFixture is a hypervisor "hv1" whose libvirt is pm and whose manifest is
// a file in a temp working directory. No auth (handler-level).
type imgFixture struct {
	pm       *poolMgr
	h        *handlers
	srv      http.Handler
	manifest string
}

func newImgFixture(t *testing.T, role string) *imgFixture {
	t.Helper()
	t.Chdir(t.TempDir())
	pm := newPoolMgr(t)
	res := hosts.NewWith("hv1", role, nil, func() (app.Manager, func(), error) { return pm, func() {}, nil })
	manifest := filepath.Join(t.TempDir(), vmtool.ImageManifestFile)
	h := &handlers{hosts: res, role: role, imageManifest: manifest}
	return &imgFixture{pm: pm, h: h, srv: handlerFor(h), manifest: manifest}
}

func (f *imgFixture) seedManifest(t *testing.T, entries map[string]vmtool.ImageEntry) {
	t.Helper()
	if err := vmtool.OpenImageManifest(f.manifest).Update(func(es map[string]vmtool.ImageEntry) error {
		for k, v := range entries {
			es[k] = v
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *imgFixture) manifestBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(f.manifest)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return b
}

// tripReader fails the test if the body is read at all.
type tripReader struct{ t *testing.T }

func (r tripReader) Read([]byte) (int, error) {
	r.t.Error("request body read")
	return 0, io.EOF
}

func (f *imgFixture) put(name string, body io.Reader, length int64, sha string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, "/v1/hosts/hv1/images/"+name, body)
	r.ContentLength = length
	r.Header.Set("Content-Type", "application/octet-stream")
	if sha != "" {
		r.Header.Set("X-Image-SHA256", sha)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

func (f *imgFixture) noPart(t *testing.T, name string) {
	t.Helper()
	for _, d := range f.pm.dirs {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".part") {
				t.Errorf("temporary file %s left in %s", e.Name(), d)
			}
		}
	}
}

func errText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q is not an api.Error: %v", rec.Body, err)
	}
	return e.Error
}

func getManifest(t *testing.T, srv http.Handler, host, name string) api.ImageManifestEntry {
	t.Helper()
	rec := do(srv, http.MethodGet, "/v1/hosts/"+host+"/images/"+name+"/manifest", "")
	if rec.Code != 200 {
		t.Fatalf("GET manifest %s: %d %s", name, rec.Code, rec.Body)
	}
	var e api.ImageManifestEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// A new image over 1 MiB (the body cap every other route has) is stored,
// recorded and visible to libvirt; nothing temporary is left.
func TestUploadImageStores(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	data := randomBytes(t, 3<<20+17)
	rec := f.put("ubuntu.qcow2", bytes.NewReader(data), int64(len(data)), strings.ToUpper(hexSum(data)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	final := filepath.Join(f.pm.dirs["default"], "ubuntu.qcow2")
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("stored file differs (%v)", err)
	}
	f.noPart(t, "ubuntu.qcow2")
	e, ok, err := vmtool.OpenImageManifest(f.manifest).Get("ubuntu.qcow2")
	if err != nil || !ok || e.SHA256 != hexSum(data) || e.Size != int64(len(data)) {
		t.Fatalf("manifest entry %+v %v %v", e, ok, err)
	}
	if p, err := f.pm.ImagePath("ubuntu.qcow2"); err != nil || p != final {
		t.Errorf("pool not refreshed: ImagePath = %q, %v", p, err)
	}
	m := getManifest(t, f.srv, "hv1", "ubuntu.qcow2")
	if m.Foreign || m.Sha256 == nil || *m.Sha256 != hexSum(data) || m.Size == nil || *m.Size != int64(len(data)) || m.Path == nil || *m.Path != final {
		t.Errorf("manifest = %+v", m)
	}
	if d := f.pm.deleted(); len(d) != 0 {
		t.Errorf("deleted %v for a new image", d)
	}

	// Every other route keeps the 1 MiB cap.
	big := `{"name":"x","size_gb":1,"pool":"` + strings.Repeat("a", 2<<20) + `"}`
	rec = do(f.srv, http.MethodPost, "/v1/hosts/hv1/volumes", big)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too large") {
		t.Errorf("2 MiB createVolume body: %d %.200s", rec.Code, rec.Body)
	}
}

func TestUploadImageHashMismatch(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	data := randomBytes(t, 4096)
	rec := f.put("u.qcow2", bytes.NewReader(data), int64(len(data)), hexSum([]byte("something else")))
	if rec.Code != http.StatusBadRequest || !strings.Contains(errText(t, rec), "SHA-256") {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Lstat(filepath.Join(f.pm.dirs["default"], "u.qcow2")); !os.IsNotExist(err) {
		t.Errorf("file left after a hash mismatch: %v", err)
	}
	f.noPart(t, "u.qcow2")
	if b := f.manifestBytes(t); b != nil {
		t.Errorf("manifest written: %s", b)
	}
}

func TestUploadImageBadRequests(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	data := []byte("0123456789")
	sha := hexSum(data)
	cases := []struct {
		name, img, sha string
		body           io.Reader
		length         int64
		code           int
	}{
		{"no sha header", "u.qcow2", "", bytes.NewReader(data), 10, 400},
		{"bad sha header", "u.qcow2", "xyz", tripReader{t}, 10, 400},
		{"empty body", "u.qcow2", hexSum(nil), bytes.NewReader(nil), 0, 400},
		{"unknown length", "u.qcow2", sha, bytes.NewReader(data), -1, 411},
		{"short body", "u.qcow2", sha, bytes.NewReader(data[:4]), 10, 400},
		{"leading dot", ".u.qcow2.part", sha, tripReader{t}, 10, 400},
		{"dot dot", "..x", sha, tripReader{t}, 10, 400},
		{"too long", strings.Repeat("a", 201), sha, tripReader{t}, 10, 400},
	}
	for _, c := range cases {
		rec := f.put(c.img, c.body, c.length, c.sha)
		if rec.Code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body, c.code)
		}
		ents, _ := os.ReadDir(f.pm.dirs["default"])
		if len(ents) != 0 {
			t.Errorf("%s: pool holds %v", c.name, ents)
		}
	}
	if b := f.manifestBytes(t); b != nil {
		t.Errorf("manifest written: %s", b)
	}
	if d := f.pm.deleted(); len(d) != 0 {
		t.Errorf("deleted %v", d)
	}
}

// The foreign-volume contract: a same-named file in the default pool's
// directory that the manifest does not list (and libvirt has not even seen)
// is refused before the body is read, and nothing changes.
func TestUploadImageForeignFile(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	f.seedManifest(t, map[string]vmtool.ImageEntry{"mine.qcow2": {SHA256: hexSum([]byte("m")), Size: 1}})
	f.pm.put(t, "default", "mine.qcow2", []byte("m"), true)
	foreign := f.pm.put(t, "default", "ubuntu.qcow2", []byte("someone else's image"), false)
	old := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}
	before := f.manifestBytes(t)

	rec := f.put("ubuntu.qcow2", tripReader{t}, 1<<30, hexSum([]byte("new")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	want := fmt.Sprintf("image %q already exists on hv1 at %s; vmtool did not create it and will not replace it", "ubuntu.qcow2", foreign)
	if got := errText(t, rec); got != want {
		t.Errorf("error %q\nwant %q", got, want)
	}
	b, err := os.ReadFile(foreign)
	st, _ := os.Stat(foreign)
	if err != nil || string(b) != "someone else's image" || !st.ModTime().Equal(old) {
		t.Errorf("foreign file changed: %q mtime %v (%v)", b, st.ModTime(), err)
	}
	f.noPart(t, "ubuntu.qcow2")
	if after := f.manifestBytes(t); !bytes.Equal(before, after) {
		t.Errorf("manifest changed:\n%s\n->\n%s", before, after)
	}
	if d := f.pm.deleted(); len(d) != 0 {
		t.Errorf("deleted %v", d)
	}
	m := getManifest(t, f.srv, "hv1", "ubuntu.qcow2")
	if !m.Foreign || m.Path == nil || *m.Path != foreign || m.Sha256 != nil {
		t.Errorf("manifest for a foreign file = %+v", m)
	}
}

// A same-named volume in a second pool is foreign too.
func TestUploadImageForeignOtherPool(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	foreign := f.pm.put(t, "other", "ubuntu.qcow2", []byte("theirs"), true)
	rec := f.put("ubuntu.qcow2", tripReader{t}, 10, hexSum([]byte("new")))
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), "at "+foreign+";") {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if ents, _ := os.ReadDir(f.pm.dirs["default"]); len(ents) != 0 {
		t.Errorf("default pool holds %v", ents)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "theirs" {
		t.Errorf("foreign file now %q", b)
	}
	if b := f.manifestBytes(t); b != nil {
		t.Errorf("manifest written: %s", b)
	}
	if m := getManifest(t, f.srv, "hv1", "ubuntu.qcow2"); !m.Foreign || m.Path == nil || *m.Path != foreign {
		t.Errorf("manifest = %+v", m)
	}
}

// A volume the manifest lists is the daemon's: a stale one is replaced.
func TestUploadImageReplacesListedStale(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	f.pm.put(t, "default", "u.qcow2", []byte("old bytes"), true)
	f.seedManifest(t, map[string]vmtool.ImageEntry{
		"u.qcow2":    {SHA256: hexSum([]byte("old bytes")), Size: 9},
		"keep.qcow2": {SHA256: hexSum([]byte("k")), Size: 1},
	})
	data := randomBytes(t, 70000)
	rec := f.put("u.qcow2", bytes.NewReader(data), int64(len(data)), hexSum(data))
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if d := f.pm.deleted(); len(d) != 1 || d[0] != "u.qcow2 default" {
		t.Errorf("deletes %v, want the old volume via the Manager", d)
	}
	if b, _ := os.ReadFile(filepath.Join(f.pm.dirs["default"], "u.qcow2")); !bytes.Equal(b, data) {
		t.Error("new bytes not in place")
	}
	f.noPart(t, "u.qcow2")
	all, err := vmtool.OpenImageManifest(f.manifest).Load()
	if err != nil || all["u.qcow2"].SHA256 != hexSum(data) || all["u.qcow2"].Size != int64(len(data)) || all["keep.qcow2"].Size != 1 {
		t.Errorf("manifest %+v %v", all, err)
	}
}

// Listed but the file is gone: not foreign, no digest (so a client
// uploads), and the upload goes ahead.
func TestUploadImageListedMissing(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	f.seedManifest(t, map[string]vmtool.ImageEntry{"u.qcow2": {SHA256: hexSum([]byte("x")), Size: 1}})
	if m := getManifest(t, f.srv, "hv1", "u.qcow2"); m.Foreign || m.Sha256 != nil {
		t.Errorf("manifest = %+v", m)
	}
	data := []byte("fresh")
	if rec := f.put("u.qcow2", bytes.NewReader(data), 5, hexSum(data)); rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if m := getManifest(t, f.srv, "hv1", "u.qcow2"); m.Sha256 == nil || *m.Sha256 != hexSum(data) {
		t.Errorf("manifest = %+v", m)
	}
}

// A foreign file that appears while the body is uploading is never
// overwritten: the hard link fails, 409, temporary file removed.
func TestUploadImageLinkRace(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	var foreign string
	f.h.beforeCommit = func(name string) {
		foreign = filepath.Join(f.pm.dirs["default"], name)
		if err := os.WriteFile(foreign, []byte("appeared"), 0o644); err != nil {
			t.Error(err)
		}
	}
	data := []byte("mine")
	rec := f.put("u.qcow2", bytes.NewReader(data), 4, hexSum(data))
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), "will not replace it") {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "appeared" {
		t.Errorf("foreign file now %q", b)
	}
	f.noPart(t, "u.qcow2")
	if b := f.manifestBytes(t); b != nil {
		t.Errorf("manifest written: %s", b)
	}
}

// A temporary file that already exists (another upload, or a crashed one) is
// left alone: 409, nothing read.
func TestUploadImagePartExists(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	part := f.pm.put(t, "default", ".u.qcow2.part", []byte("in progress"), false)
	rec := f.put("u.qcow2", tripReader{t}, 10, hexSum([]byte("x")))
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), part) {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(part); string(b) != "in progress" {
		t.Errorf("part now %q", b)
	}
}

// A workstation keeps no image cache: it refuses uploads and manifest
// reads for itself without touching its pools.
func TestUploadImageWorkstationRefuses(t *testing.T) {
	f := newImgFixture(t, config.RoleWorkstation)
	rec := f.put("u.qcow2", tripReader{t}, 10, hexSum([]byte("x")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if rec := do(f.srv, http.MethodGet, "/v1/hosts/hv1/images/u.qcow2/manifest", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("GET manifest: %d %s", rec.Code, rec.Body)
	}
	if ents, _ := os.ReadDir(f.pm.dirs["default"]); len(ents) != 0 {
		t.Errorf("pool holds %v", ents)
	}
}

// deleteImage is unchanged, and drops the manifest entry of a listed image it
// deleted from the default pool.
func TestDeleteImageDropsManifestEntry(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	f.pm.put(t, "default", "u.qcow2", []byte("u"), true)
	f.pm.put(t, "default", "theirs.qcow2", []byte("t"), true)
	f.seedManifest(t, map[string]vmtool.ImageEntry{
		"u.qcow2":    {SHA256: hexSum([]byte("u")), Size: 1},
		"keep.qcow2": {SHA256: hexSum([]byte("k")), Size: 1},
	})
	before := f.manifestBytes(t)
	if rec := do(f.srv, http.MethodDelete, "/v1/hosts/hv1/images/theirs.qcow2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE theirs: %d %s", rec.Code, rec.Body)
	}
	if after := f.manifestBytes(t); !bytes.Equal(before, after) {
		t.Errorf("unlisted delete changed the manifest")
	}
	if rec := do(f.srv, http.MethodDelete, "/v1/hosts/hv1/images/u.qcow2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE u: %d %s", rec.Code, rec.Body)
	}
	all, err := vmtool.OpenImageManifest(f.manifest).Load()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "keep.qcow2" {
		t.Errorf("manifest holds %v", names)
	}
}

// --- end to end: workstation app.CreateVM -> remote.Manager -> daemon ---

type progressLog struct {
	mu     sync.Mutex
	events []app.ProgressEvent
}

func (p *progressLog) add(ev app.ProgressEvent) {
	p.mu.Lock()
	p.events = append(p.events, ev)
	p.mu.Unlock()
}

func (p *progressLog) count(stage, status string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events {
		if e.Stage == stage && e.Status == status {
			n++
		}
	}
	return n
}

func (p *progressLog) last(stage, status string) (app.ProgressEvent, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.events) - 1; i >= 0; i-- {
		if e := p.events[i]; e.Stage == stage && e.Status == status {
			return e, true
		}
	}
	return app.ProgressEvent{}, false
}

func puts(log *authLog) int {
	n := 0
	for _, s := range log.seen() {
		if strings.HasPrefix(s, "PUT /v1/hosts/veritech01/images/") {
			n++
		}
	}
	return n
}

// e2e is a TLS hypervisor daemon (real chain, bearer, temp-dir pools) and a
// workstation Service with a local image and its own hash cache.
type e2e struct {
	pm       *poolMgr
	log      *authLog
	m        *remote.Manager
	svc      *app.Service
	local    string
	manifest string // the daemon's
}

func newE2E(t *testing.T, image []byte) *e2e {
	t.Helper()
	t.Chdir(t.TempDir())
	pm := newPoolMgr(t)
	res := hosts.NewWith("veritech01", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return pm, func() {}, nil })
	manifest := filepath.Join(t.TempDir(), vmtool.ImageManifestFile)
	h := chain(&handlers{hosts: res, role: config.RoleHypervisor, imageManifest: manifest}, func(next http.Handler) http.Handler {
		return requireBearer(config.Hash(testToken), publicRoutes(), next)
	})
	log := &authLog{next: h}
	srv := httptest.NewUnstartedServer(log)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cas := x509.NewCertPool()
	cas.AddCert(srv.Certificate())
	m := remoteFor(t, srv, cas, testToken)

	wsDir := t.TempDir()
	local := filepath.Join(wsDir, "pool", "ubuntu.qcow2")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, image, 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{
		IPTimeout:      time.Second,
		ImageCachePath: filepath.Join(wsDir, vmtool.ImageManifestFile),
		LocalImages: func(name string) (string, error) {
			p := filepath.Join(wsDir, "pool", name)
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("image %q: %w", name, vmtool.ErrNotFound)
			}
			return p, nil
		},
	}
	return &e2e{pm: pm, log: log, m: m, svc: svc, local: local, manifest: manifest}
}

func (e *e2e) create(t *testing.T, name string, p *progressLog) (*vmtool.VMInfo, error) {
	t.Helper()
	return e.svc.CreateVM(context.Background(), e.m, app.CreateVMInput{
		Host: "veritech01", Name: name, Image: "ubuntu.qcow2", OnProgress: p.add,
	})
}

func TestCreateVMUploadsImageOnce(t *testing.T) {
	data := randomBytes(t, 2<<20+5)
	e := newE2E(t, data)

	p := &progressLog{}
	if _, err := e.create(t, "web1", p); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if n := puts(e.log); n != 1 {
		t.Fatalf("first create made %d uploads", n)
	}
	if b, _ := os.ReadFile(filepath.Join(e.pm.dirs["default"], "ubuntu.qcow2")); !bytes.Equal(b, data) {
		t.Fatal("image not on the hypervisor")
	}
	ev, ok := p.last("upload", "progress")
	if !ok || ev.Sent != int64(len(data)) || ev.Total != int64(len(data)) {
		t.Errorf("last upload progress %+v (%v)", ev, ok)
	}
	if p.count("upload", "hash") != 1 {
		t.Errorf("first create hashed %d times", p.count("upload", "hash"))
	}
	if !strings.Contains(strings.Join(e.pm.got(), ","), "CloneImage "+filepath.Join(e.pm.dirs["default"], "ubuntu.qcow2")+" web1") {
		t.Errorf("clone did not use the uploaded image: %v", e.pm.got())
	}

	// Second create: cached hash, same digest on the daemon, no upload.
	p = &progressLog{}
	if _, err := e.create(t, "web2", p); err != nil {
		t.Fatalf("second create: %v", err)
	}
	if n := puts(e.log); n != 1 {
		t.Errorf("second create uploaded (%d PUTs)", n)
	}
	if p.count("upload", "hash") != 0 {
		t.Errorf("second create re-hashed")
	}

	// New mtime, same bytes: re-hash, no upload.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(e.local, later, later); err != nil {
		t.Fatal(err)
	}
	p = &progressLog{}
	if _, err := e.create(t, "web3", p); err != nil {
		t.Fatalf("third create: %v", err)
	}
	if p.count("upload", "hash") != 1 {
		t.Errorf("mtime change did not re-hash")
	}
	if n := puts(e.log); n != 1 {
		t.Errorf("mtime change uploaded (%d PUTs)", n)
	}

	// Changed bytes: upload again, the daemon replaces its own volume.
	data2 := randomBytes(t, 1234)
	if err := os.WriteFile(e.local, data2, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create(t, "web4", &progressLog{}); err != nil {
		t.Fatalf("fourth create: %v", err)
	}
	if n := puts(e.log); n != 2 {
		t.Errorf("changed image: %d PUTs, want 2", n)
	}
	if b, _ := os.ReadFile(filepath.Join(e.pm.dirs["default"], "ubuntu.qcow2")); !bytes.Equal(b, data2) {
		t.Error("changed image not replaced on the hypervisor")
	}
}

// A foreign volume stops create before any upload or libvirt work.
func TestCreateVMForeignImageStops(t *testing.T) {
	e := newE2E(t, []byte("my image"))
	foreign := e.pm.put(t, "default", "ubuntu.qcow2", []byte("someone else's"), false)

	_, err := e.create(t, "web", &progressLog{})
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindConflict {
		t.Fatalf("err = %v, want a conflict", err)
	}
	want := fmt.Sprintf("image %q already exists on veritech01 at %s; vmtool did not create it and will not replace it", "ubuntu.qcow2", foreign)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err %q\nwant it to contain %q", err, want)
	}
	if n := puts(e.log); n != 0 {
		t.Errorf("%d uploads", n)
	}
	if calls := strings.Join(e.pm.got(), ","); strings.Contains(calls, "Clone") || strings.Contains(calls, "Define") {
		t.Errorf("libvirt work after a foreign image: %v", calls)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "someone else's" {
		t.Errorf("foreign file now %q", b)
	}
	if _, err := os.Stat(e.manifest); !os.IsNotExist(err) {
		t.Errorf("daemon manifest written: %v", err)
	}
}
