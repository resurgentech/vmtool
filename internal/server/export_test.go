package server

// exportTemplate and getImageBlob tests (API 2.4.0). The hypervisor's thin
// helper is a fake Manager (exportMgr): no real lvm, qemu-img or sudo runs; the
// pool directory is a temp directory.

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/remote"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	tplLVName = "vmtpl-000000000001"
	sha64a    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// exportMgr is a hypervisor Manager with thin templates: the daemon side of
// export (app.TemplateVolumeExporter). ExportTemplateVolume writes data to the
// destination the way the helper's qemu-img does (it fails if dest exists).
type exportMgr struct {
	*poolMgr
	emu       sync.Mutex
	tpls      []app.TemplateInfo
	data      []byte
	exportErr error // after writing a partial file
	ecalls    []string
	onExport  func(dest string)
	noThin    bool
}

func (m *exportMgr) ecall(format string, a ...any) {
	m.emu.Lock()
	defer m.emu.Unlock()
	m.ecalls = append(m.ecalls, fmt.Sprintf(format, a...))
}

func (m *exportMgr) ncalls(prefix string) int {
	m.emu.Lock()
	defer m.emu.Unlock()
	n := 0
	for _, c := range m.ecalls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (m *exportMgr) LookupTemplate(name, pool string) (app.TemplateInfo, error) {
	m.ecall("lookup %s %s", name, pool)
	if m.noThin {
		return app.TemplateInfo{}, app.ErrNoThinPools
	}
	var found []app.TemplateInfo
	for _, t := range m.tpls {
		if (t.Name == name || t.LV == name) && (pool == "" || t.Pool == pool) {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return app.TemplateInfo{}, fmt.Errorf("%q is not a template: %w", name, vmtool.ErrNotFound)
	case 1:
		return found[0], nil
	}
	return app.TemplateInfo{}, &app.Error{Kind: app.KindConflict, Err: fmt.Errorf("%q matches %d templates", name, len(found))}
}

func (m *exportMgr) ExportTemplateVolume(t app.TemplateInfo, dest string) error {
	m.ecall("export %s %s", t.LV, filepath.Base(dest))
	if m.onExport != nil {
		m.onExport(dest)
	}
	if _, err := os.Lstat(dest); err == nil {
		return errors.New("destination exists")
	}
	if err := os.WriteFile(dest, m.data, 0o644); err != nil {
		return err
	}
	return m.exportErr
}

func (m *exportMgr) RecordTemplateSHA(t app.TemplateInfo, sha string) error {
	m.ecall("tag %s %s", t.LV, sha)
	m.emu.Lock()
	defer m.emu.Unlock()
	for i := range m.tpls {
		if m.tpls[i].LV == t.LV {
			m.tpls[i].SHA256 = sha
		}
	}
	return nil
}

func savedTpl(lv, name, sha string) app.TemplateInfo {
	return app.TemplateInfo{Template: vmtool.Template{Name: name, LV: lv, Pool: "thin-test",
		Origin: vmtool.TemplateOriginSaved, SHA256: sha, SizeBytes: 1 << 20}}
}

type expFixture struct {
	*imgFixture
	em *exportMgr
}

func newExportFixture(t *testing.T, role string, tpls ...app.TemplateInfo) *expFixture {
	t.Helper()
	t.Chdir(t.TempDir())
	pm := newPoolMgr(t)
	em := &exportMgr{poolMgr: pm, tpls: tpls, data: randomBytes(t, 50000)}
	res := hosts.NewWith("hv1", role, nil, func() (app.Manager, func(), error) { return em, func() {}, nil })
	manifest := filepath.Join(t.TempDir(), vmtool.ImageManifestFile)
	h := &handlers{hosts: res, role: role, imageManifest: manifest}
	return &expFixture{imgFixture: &imgFixture{pm: pm, h: h, srv: handlerFor(h), manifest: manifest}, em: em}
}

func (f *expFixture) export(name, query string) *httptest.ResponseRecorder {
	return exportVia(f.srv, "hv1", name, query, "")
}

func exportVia(h http.Handler, host, name, query, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/hosts/"+host+"/templates/"+name+"/export"+query, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func exportEvents(t *testing.T, rec *httptest.ResponseRecorder) []api.ExportTemplateEvent {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type %q", ct)
	}
	var out []api.ExportTemplateEvent
	for _, l := range strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n") {
		var e api.ExportTemplateEvent
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		t.Fatal("no events")
	}
	for _, e := range out[:len(out)-1] {
		if e.Event != api.ExportTemplateEventEventProgress {
			t.Errorf("non-terminal event %+v", e)
		}
	}
	return out
}

func partsIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".part") {
			out = append(out, e.Name())
		}
	}
	return out
}

// An exported saved template becomes a daemon-created image of the template's
// name: hashed, in the pool directory, in the manifest, with the digest on the
// template's tags, and a progress line or more before the done line.
func TestExportTemplateWritesTheImage(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	// The first request names the LV; the image is still called by the template's name.
	rec := f.export(tplLVName, "")
	evs := exportEvents(t, rec)
	end := evs[len(evs)-1]
	if end.Event != api.ExportTemplateEventEventDone || end.Image == nil {
		t.Fatalf("terminal %+v: %s", end, rec.Body)
	}
	want := hexSum(f.em.data)
	img := end.Image
	if img.Name != "proj" || img.Foreign || img.Sha256 == nil || *img.Sha256 != want || img.Size == nil || *img.Size != int64(len(f.em.data)) {
		t.Errorf("image %+v", img)
	}
	if len(evs) < 2 || evs[0].Event != api.ExportTemplateEventEventProgress || evs[0].Stage == nil || *evs[0].Stage != api.Export {
		t.Errorf("no export progress line first: %+v", evs)
	}
	dir := f.pm.dirs[vmtool.HypervisorPool]
	if b, _ := os.ReadFile(filepath.Join(dir, "proj")); !bytes.Equal(b, f.em.data) {
		t.Error("the image is not in the pool directory")
	}
	if p := partsIn(t, dir); len(p) != 0 {
		t.Errorf("temporary files left: %v", p)
	}
	// daemon-created: the manifest lists it and it is not foreign
	if e := getManifest(t, f.srv, "hv1", "proj"); e.Foreign || e.Sha256 == nil || *e.Sha256 != want {
		t.Errorf("manifest %+v", e)
	}
	// the template carries the digest, so a create finds it with no rebuild
	if got := f.em.ncalls("tag " + tplLVName + " " + want); got != 1 {
		t.Errorf("template tagged %d times: %v", got, f.em.ecalls)
	}
	if len(f.pm.refreshes) == 0 {
		t.Error("the pool was not refreshed")
	}
}

// Exporting the same template again, with the manifest entry equal to the
// template's recorded digest and the file in place, is one done line and no
// second conversion.
func TestExportTemplateSameTemplateAgainIsDone(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	first := exportEvents(t, f.export("proj", ""))
	img := first[len(first)-1].Image
	rec := f.export("proj", "")
	evs := exportEvents(t, rec)
	if len(evs) != 1 || evs[0].Event != api.ExportTemplateEventEventDone || evs[0].Image == nil ||
		*evs[0].Image.Sha256 != *img.Sha256 {
		t.Fatalf("second export: %s", rec.Body)
	}
	if n := f.em.ncalls("export "); n != 1 {
		t.Errorf("%d conversions", n)
	}
}

// A name the manifest already lists is refused unless it is the same
// template; the listed file is not touched.
func TestExportTemplateRefusesAnExistingManifestName(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	dir := f.pm.dirs[vmtool.HypervisorPool]
	old := []byte("PLACEHOLDER uploaded image")
	path := f.pm.put(t, vmtool.HypervisorPool, "proj", old, true)
	f.seedManifest(t, map[string]vmtool.ImageEntry{"proj": {SHA256: hexSum(old), Size: int64(len(old))}})

	rec := f.export("proj", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if msg := errText(t, rec); !strings.Contains(msg, "proj") || !strings.Contains(msg, "images delete") {
		t.Errorf("message %q", msg)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, old) {
		t.Error("the listed image was changed")
	}
	if n := f.em.ncalls("export "); n != 0 {
		t.Errorf("%d conversions", n)
	}
	if p := partsIn(t, dir); len(p) != 0 {
		t.Errorf("temporary files %v", p)
	}

	// An image template (built from a cached image) exports only as that same
	// image: a different listed digest is refused too, and an image template
	// whose image the cache no longer lists is not exported as a new file.
	g := newExportFixture(t, config.RoleHypervisor, app.TemplateInfo{Template: vmtool.Template{
		Name: "base", Image: "base", LV: "vmtpl-aaaaaaaaaaaa", Pool: "thin-test", Origin: vmtool.TemplateOriginImage, SHA256: sha64a}})
	if rec := g.export("base", ""); rec.Code != http.StatusConflict {
		t.Errorf("image template whose image is not cached: %d %s", rec.Code, rec.Body)
	}
	if n := g.em.ncalls("export "); n != 0 {
		t.Errorf("%d conversions of an image template", n)
	}
	// ... but with its image cached under that digest it is the same: done.
	g.pm.put(t, vmtool.HypervisorPool, "base", []byte("x"), true)
	g.seedManifest(t, map[string]vmtool.ImageEntry{"base": {SHA256: sha64a, Size: 1}})
	evs := exportEvents(t, g.export("base", ""))
	if len(evs) != 1 || evs[0].Event != api.ExportTemplateEventEventDone {
		t.Errorf("same image: %+v", evs)
	}
	if n := g.em.ncalls("export "); n != 0 {
		t.Errorf("%d conversions of a cached image", n)
	}
}

// A foreign volume of the name is 409 before anything is written, and stays.
func TestExportTemplateForeign(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	theirs := []byte("PLACEHOLDER someone else's image")
	path := f.pm.put(t, vmtool.HypervisorPool, "proj", theirs, false)
	rec := f.export("proj", "")
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), "will not replace it") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, theirs) {
		t.Error("the foreign file was changed")
	}
	if n := f.em.ncalls("export "); n != 0 {
		t.Errorf("%d conversions", n)
	}
	// also a foreign volume in another pool
	f2 := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	f2.pm.put(t, "other", "proj", theirs, true)
	if rec := f2.export("proj", ""); rec.Code != http.StatusConflict {
		t.Errorf("foreign in another pool: %d", rec.Code)
	}
}

// A failure removes the export's own temporary file and nothing else: not the
// other files in the pool, not another image's temporary file, not a manifest
// entry.
func TestExportTemplateFailureRemovesOnlyItsPart(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	f.em.exportErr = errors.New("qemu-img: no space left")
	dir := f.pm.dirs[vmtool.HypervisorPool]
	keep := f.pm.put(t, vmtool.HypervisorPool, "keep.qcow2", []byte("keep"), true)
	other := f.pm.put(t, vmtool.HypervisorPool, ".other.part", []byte("another upload"), false)

	rec := f.export("proj", "")
	evs := exportEvents(t, rec)
	end := evs[len(evs)-1]
	if end.Event != api.ExportTemplateEventEventError || end.Error == nil || !strings.Contains(*end.Error, "no space left") {
		t.Fatalf("terminal %+v", end)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".proj.part")); !os.IsNotExist(err) {
		t.Error("the export's temporary file was left")
	}
	for _, p := range []string{keep, other} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "proj")); !os.IsNotExist(err) {
		t.Error("an image was put in place")
	}
	if b := f.manifestBytes(t); strings.Contains(string(b), "proj") {
		t.Errorf("manifest %q", b)
	}
	if n := f.em.ncalls("tag "); n != 0 {
		t.Error("a failed export tagged the template")
	}

	// A temporary file that is not ours (the helper refuses: it exists) is not
	// removed by the failure path.
	g := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	g.em.onExport = func(dest string) { _ = os.WriteFile(dest, []byte("created by another process"), 0o644) }
	rec = g.export("proj", "")
	evs = exportEvents(t, rec)
	if evs[len(evs)-1].Event != api.ExportTemplateEventEventError {
		t.Fatalf("%+v", evs)
	}
	if b, err := os.ReadFile(filepath.Join(g.pm.dirs[vmtool.HypervisorPool], ".proj.part")); err != nil || string(b) != "created by another process" {
		t.Errorf("a file another process created was removed: %q %v", b, err)
	}
}

// A truncated or empty conversion is an error line and no image.
func TestExportTemplateEmptyOutputIsAnError(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	f.em.data = nil
	evs := exportEvents(t, f.export("proj", ""))
	if end := evs[len(evs)-1]; end.Event != api.ExportTemplateEventEventError {
		t.Fatalf("%+v", end)
	}
	dir := f.pm.dirs[vmtool.HypervisorPool]
	if p := partsIn(t, dir); len(p) != 0 {
		t.Errorf("temporary files %v", p)
	}
	if _, err := os.Lstat(filepath.Join(dir, "proj")); !os.IsNotExist(err) {
		t.Error("an empty image was put in place")
	}
}

func TestExportTemplateStatuses(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor,
		savedTpl(tplLVName, "proj", ""), savedTpl("vmtpl-000000000002", "dup", ""), savedTpl("vmtpl-000000000003", "dup", ""))
	for _, tc := range []struct {
		name, query string
		code        int
	}{
		{"nosuch", "", 404},
		{".hidden", "", 400},
		{"dup", "", 409},
		{"proj", "?pool=elsewhere", 404},
	} {
		if rec := f.export(tc.name, tc.query); rec.Code != tc.code {
			t.Errorf("%s%s: %d %s, want %d", tc.name, tc.query, rec.Code, rec.Body, tc.code)
		}
	}
	if n := f.em.ncalls("export "); n != 0 {
		t.Errorf("%d conversions", n)
	}
	// a host with no thin pools: 404 saying so
	g := newExportFixture(t, config.RoleHypervisor)
	g.em.noThin = true
	if rec := g.export("proj", ""); rec.Code != 404 || !strings.Contains(errText(t, rec), "no thin pools") {
		t.Errorf("no thin pools: %d %s", rec.Code, rec.Body)
	}
	// an unknown host
	if rec := exportVia(f.srv, "nohost", "proj", "", ""); rec.Code != 404 {
		t.Errorf("unknown host: %d", rec.Code)
	}
}

// One upload, fetch or export of a name at a time.
func TestExportTemplateInFlightIs409(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	part := partPath(f.pm.dirs[vmtool.HypervisorPool], "proj")
	if !uploads.enter(part) {
		t.Fatal("registry busy")
	}
	defer uploads.leave(part)
	rec := f.export("proj", "")
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), "already running") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if n := f.em.ncalls("export "); n != 0 {
		t.Errorf("%d conversions", n)
	}
}

// The request body is never read into a log; neither is the stream.
func TestExportTemplateLogsNoBodies(t *testing.T) {
	f := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	var logs lockedBuf
	restore := captureLogs(&logs)
	defer restore()
	r := httptest.NewRequest(http.MethodPost, "/v1/hosts/hv1/templates/proj/export", strings.NewReader(`{"note":"PLACEHOLDER-not-a-secret-2"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(logs.String(), "PLACEHOLDER-not-a-secret-2") || strings.Contains(rec.Body.String(), "PLACEHOLDER-not-a-secret-2") {
		t.Error("a request body reached a log or the stream")
	}
}

func TestExportTemplateWorkstationRefusesItself(t *testing.T) {
	f := newExportFixture(t, config.RoleWorkstation, savedTpl(tplLVName, "proj", ""))
	if rec := f.export("proj", ""); rec.Code != http.StatusForbidden {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if n := f.em.ncalls("export "); n != 0 {
		t.Errorf("%d conversions", n)
	}
}

// --- getImageBlob ---

func (f *imgFixture) blob(name string) *httptest.ResponseRecorder {
	return blobVia(f.srv, "hv1", name, "")
}

func blobVia(h http.Handler, host, name, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/hosts/"+host+"/images/"+name+"/blob", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// listed puts data in the pool directory and the manifest, as an upload does.
func (f *imgFixture) listed(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := f.pm.put(t, vmtool.HypervisorPool, name, data, true)
	f.seedManifest(t, map[string]vmtool.ImageEntry{name: {SHA256: hexSum(data), Size: int64(len(data))}})
	return p
}

func TestImageBlobServesAListedImage(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	data := randomBytes(t, 70000)
	f.listed(t, "proj", data)
	rec := f.blob("proj")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Error("body differs")
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(data)) {
		t.Errorf("Content-Length %q", got)
	}
	if got := rec.Header().Get("X-Image-SHA256"); got != hexSum(data) {
		t.Errorf("X-Image-SHA256 %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	// the hot path needs no libvirt
	for _, c := range f.pm.got() {
		t.Errorf("libvirt call %q", c)
	}
}

// Only what the manifest lists is served, and only the manifest entry's own
// regular file of the listed size.
func TestImageBlobRefusals(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	secret := []byte("PLACEHOLDER secret bytes that must not leave")
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, secret, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := f.pm.dirs[vmtool.HypervisorPool]

	// not in the manifest, nothing of that name: 404
	if rec := f.blob("nosuch"); rec.Code != 404 {
		t.Errorf("not listed: %d", rec.Code)
	}
	// not in the manifest, a foreign file of that name: refused, not served
	f.pm.put(t, vmtool.HypervisorPool, "theirs", secret, false)
	if rec := f.blob("theirs"); rec.Code != 409 || bytes.Contains(rec.Body.Bytes(), secret) {
		t.Errorf("foreign file: %d %s", rec.Code, rec.Body)
	}
	// foreign in another pool, same
	f.pm.put(t, "other", "theirs2", secret, true)
	if rec := f.blob("theirs2"); rec.Code != 409 || bytes.Contains(rec.Body.Bytes(), secret) {
		t.Errorf("foreign file in another pool: %d", rec.Code)
	}
	// listed, with a symlink in place of the file
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	f.seedManifest(t, map[string]vmtool.ImageEntry{"linked": {SHA256: hexSum(secret), Size: int64(len(secret))}})
	if rec := f.blob("linked"); rec.Code != 409 || bytes.Contains(rec.Body.Bytes(), secret) {
		t.Errorf("symlink: %d %s", rec.Code, rec.Body)
	}
	// listed, a directory in place of the file
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.seedManifest(t, map[string]vmtool.ImageEntry{"adir": {SHA256: sha64a, Size: 4096}})
	if rec := f.blob("adir"); rec.Code != 409 {
		t.Errorf("directory: %d", rec.Code)
	}
	// listed, the file is not the listed size
	f.pm.put(t, vmtool.HypervisorPool, "grown", append(secret, '!'), true)
	f.seedManifest(t, map[string]vmtool.ImageEntry{"grown": {SHA256: hexSum(secret), Size: int64(len(secret))}})
	if rec := f.blob("grown"); rec.Code != 409 || bytes.Contains(rec.Body.Bytes(), secret) {
		t.Errorf("size mismatch: %d", rec.Code)
	}
	// listed, the file is gone
	f.seedManifest(t, map[string]vmtool.ImageEntry{"gone": {SHA256: sha64a, Size: 4}})
	if rec := f.blob("gone"); rec.Code != 404 {
		t.Errorf("missing file: %d", rec.Code)
	}
	// names that are not plain image names never reach the filesystem
	for _, name := range []string{".hidden", "%2E%2E", "..%2Fsecret", "a%2Fb", "%2Fetc%2Fpasswd", "x%00y"} {
		rec := f.blob(name)
		if rec.Code == 200 || bytes.Contains(rec.Body.Bytes(), secret) {
			t.Errorf("name %q: %d", name, rec.Code)
		}
		if rec.Code != 400 && rec.Code != 404 {
			t.Errorf("name %q: status %d, want 400 or 404", name, rec.Code)
		}
	}
	// a name listed in the manifest whose entry would point outside: the key
	// is validated as a name, so a hand-edited manifest cannot make it serve
	f.seedManifest(t, map[string]vmtool.ImageEntry{"../secret": {SHA256: hexSum(secret), Size: int64(len(secret))}})
	if rec := f.blob("..%2Fsecret"); rec.Code == 200 {
		t.Error("a path-like manifest key was served")
	}
}

func TestImageBlobWorkstationRefusesItself(t *testing.T) {
	f := newImgFixture(t, config.RoleWorkstation)
	if rec := f.blob("proj"); rec.Code != http.StatusForbidden {
		t.Errorf("%d", rec.Code)
	}
}

// --- a workstation forwards both routes to a listed host ---

func TestWorkstationForwardsExportAndBlob(t *testing.T) {
	hv := newExportFixture(t, config.RoleHypervisor, savedTpl(tplLVName, "proj", ""))
	hvSrv := httptest.NewTLSServer(hv.srv)
	t.Cleanup(hvSrv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(hvSrv.Certificate())

	wsh := &handlers{role: config.RoleWorkstation}
	wsh.hosts = hosts.NewWith("ws", config.RoleWorkstation, []string{"hv1"}, func() (app.Manager, func(), error) {
		t.Error("workstation libvirt opened")
		return nil, nil, errors.New("no libvirt")
	}).WithRemote(func(host, tok string) (app.Manager, func(), error) {
		m, err := remote.New(host, hvSrv.URL, tok, remote.WithRootCAs(pool))
		if err != nil {
			return nil, nil, err
		}
		return m, m.Close, nil
	})
	ws := handlerFor(wsh)

	evs := exportEvents(t, exportVia(ws, "hv1", "proj", "", "caller-token"))
	end := evs[len(evs)-1]
	if end.Event != api.ExportTemplateEventEventDone || end.Image == nil || end.Image.Name != "proj" {
		t.Fatalf("forwarded export %+v", end)
	}
	rec := blobVia(ws, "hv1", "proj", "caller-token")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), hv.em.data) ||
		rec.Header().Get("X-Image-SHA256") != hexSum(hv.em.data) || rec.Header().Get("Content-Length") != strconv.Itoa(len(hv.em.data)) {
		t.Fatalf("forwarded blob: %d, headers %v", rec.Code, rec.Header())
	}
	// a refusal is relayed as its status
	if rec := blobVia(ws, "hv1", "nosuch", "caller-token"); rec.Code != 404 {
		t.Errorf("forwarded 404: %d", rec.Code)
	}
	if rec := exportVia(ws, "hv1", "nosuch", "", "caller-token"); rec.Code != 404 {
		t.Errorf("forwarded export 404: %d", rec.Code)
	}
}

var _ = io.Discard
