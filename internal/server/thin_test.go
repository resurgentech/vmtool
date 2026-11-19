package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	thinSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	thinTpl = "vmtpl-aaaaaaaaaaaa"
	thinGiB = int64(1 << 30)
)

// thinMgr is a fakeMgr that also knows thin volumes: vols maps a path to its
// kind, saved maps a saved template's name to its path, and CloneImageFrom
// records the image name it was given.
type thinMgr struct {
	*fakeMgr
	vols      map[string]string
	saved     map[string]string
	fromImage string
}

func (f *thinMgr) ThinVolume(p string) (string, bool, error) {
	k, ok := f.vols[p]
	return k, ok, nil
}

func (f *thinMgr) SavedTemplatePath(n string) (string, error) {
	if p, ok := f.saved[n]; ok {
		return p, nil
	}
	return "", fmt.Errorf("template %q: %w", n, vmtool.ErrNotFound)
}

func (f *thinMgr) CloneImageFrom(image, base, n, pool string, size uint64) (string, error) {
	f.fromImage = image
	f.cloneSize = size
	f.rec("CloneImageFrom %s %s %s %s", image, base, n, pool)
	return f.volPath, f.err
}

func newThinMgr(t *testing.T) (*thinMgr, string) {
	t.Helper()
	fm, pool := poolFixture(t)
	fm.pools = append(fm.pools, vmtool.PoolInfo{Name: "thin-test", Path: "/dev/vg-test", Active: true, Type: "thin"})
	return &thinMgr{fakeMgr: fm, vols: map[string]string{
		"/dev/vg-test/vm1":                 vmtool.ThinKindVol,
		"/dev/vg-test/vm1-2":               vmtool.ThinKindVol,
		"/dev/vg-test/" + thinTpl:          vmtool.ThinKindTemplate,
		"/dev/vg-test/vmtbld-aaaaaaaaaaaa": vmtool.ThinKindBuilding,
	}, saved: map[string]string{}}, pool
}

func thinServer(t *testing.T, m app.Manager) http.Handler {
	t.Helper()
	t.Chdir(t.TempDir())
	res := hosts.NewWith("box", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return m, func() {}, nil })
	return handlerFor(&handlers{hosts: res, role: config.RoleHypervisor})
}

func TestInPoolThinVolumes(t *testing.T) {
	m, _ := newThinMgr(t)
	for _, tc := range []struct {
		name, p     string
		okVol, okTp bool
	}{
		{"vol", "/dev/vg-test/vm1", true, true},
		{"extra vol", "/dev/vg-test/vm1-2", true, true},
		{"template: base only", "/dev/vg-test/" + thinTpl, false, true},
		{"building: never", "/dev/vg-test/vmtbld-aaaaaaaaaaaa", false, false},
		{"foreign thin LV", "/dev/vg-test/foreign", false, false},
		{"dotdot", "/dev/vg-test/../vm1", false, false},
		{"dotdot out", "/dev/vg-test/../x", false, false},
		{"mapper", "/dev/mapper/vg--test-vm1", false, false},
		{"dm", "/dev/dm-3", false, false},
		{"other vg", "/dev/other/x", false, false},
		{"dotdot that cleans to a vol", "/dev/vg-test/x/../vm1", false, false},
		{"double slash (cleaned first, as for every path)", "/dev/vg-test//vm1", true, true},
		{"trailing slash (cleaned first)", "/dev/vg-test/vm1/", true, true},
	} {
		if err := inPool(m, tc.p); (err == nil) != tc.okVol {
			t.Errorf("%s: inPool(%q) = %v, want ok=%v", tc.name, tc.p, err, tc.okVol)
		}
		if err := inPoolBase(m, tc.p); (err == nil) != tc.okTp {
			t.Errorf("%s: inPoolBase(%q) = %v, want ok=%v", tc.name, tc.p, err, tc.okTp)
		}
	}
}

func TestThinPathGateRoutes(t *testing.T) {
	m, pool := newThinMgr(t)
	h := thinServer(t, m)
	define := func(disk string, extra string) string {
		return `{"vcpus":1,"memory_gib":1,"disk_path":"` + disk + `","network":{"type":"nat"}` + extra + `}`
	}
	// defineVM and resizeVolume take a thin vol
	if rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/definition",
		define("/dev/vg-test/vm1", `,"extra_disks":[{"path":"/dev/vg-test/vm1-2","target":"vdb"}]`)); rec.Code != 204 {
		t.Fatalf("defineVM thin: %d %s", rec.Code, rec.Body)
	}
	if m.defined.DiskPath != "/dev/vg-test/vm1" || len(m.defined.ExtraDisks) != 1 || m.defined.ExtraDisks[0].Path != "/dev/vg-test/vm1-2" {
		t.Errorf("defined = %+v", m.defined)
	}
	if rec := do(h, "POST", "/v1/hosts/box/volumes/resize", `{"path":"/dev/vg-test/vm1","size_bytes":9}`); rec.Code != 204 {
		t.Fatalf("resizeVolume thin: %d %s", rec.Code, rec.Body)
	}
	// cloneVolume: a vol is ok as a base, a template too
	m.volPath = "/dev/vg-test/new"
	for _, base := range []string{"/dev/vg-test/vm1", "/dev/vg-test/" + thinTpl} {
		if rec := do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+base+`","new_name":"new","target_pool":"thin-test"}`); rec.Code != 201 {
			t.Errorf("cloneVolume base %s: %d %s", base, rec.Code, rec.Body)
		}
	}
	nCalls := len(m.calls)

	// the template is a clone base only
	for name, tc := range map[string]struct{ method, path, body string }{
		"define boot":   {"PUT", "/v1/hosts/box/vms/web/definition", define("/dev/vg-test/"+thinTpl, "")},
		"define extra":  {"PUT", "/v1/hosts/box/vms/web/definition", define(filepath.Join(pool, "a.qcow2"), `,"extra_disks":[{"path":"/dev/vg-test/`+thinTpl+`","target":"vdb"}]`)},
		"define bld":    {"PUT", "/v1/hosts/box/vms/web/definition", define("/dev/vg-test/vmtbld-aaaaaaaaaaaa", "")},
		"resize":        {"POST", "/v1/hosts/box/volumes/resize", `{"path":"/dev/vg-test/` + thinTpl + `","size_bytes":9}`},
		"foreign":       {"PUT", "/v1/hosts/box/vms/web/definition", define("/dev/vg-test/foreign", "")},
		"dotdot":        {"PUT", "/v1/hosts/box/vms/web/definition", define("/dev/vg-test/../x", "")},
		"mapper":        {"PUT", "/v1/hosts/box/vms/web/definition", define("/dev/mapper/vg--test-vm1", "")},
		"other vg":      {"POST", "/v1/hosts/box/volumes/resize", `{"path":"/dev/other/x","size_bytes":9}`},
		"clone bld":     {"POST", "/v1/hosts/box/volumes/clone", `{"base_path":"/dev/vg-test/vmtbld-aaaaaaaaaaaa","new_name":"n"}`},
		"clone foreign": {"POST", "/v1/hosts/box/volumes/clone", `{"base_path":"/dev/vg-test/foreign","new_name":"n"}`},
	} {
		rec := do(h, tc.method, tc.path, tc.body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	if len(m.calls) != nCalls {
		t.Errorf("a Manager method ran after a refused path: %q", m.calls[nCalls:])
	}
}

// The clone request's template reaches a Manager that takes it; an older
// client's request without one is the plain CloneImage.
func TestCloneVolumeTemplate(t *testing.T) {
	m, pool := newThinMgr(t)
	h := thinServer(t, m)
	base := filepath.Join(pool, "img1")
	m.volPath = "/dev/vg-test/vm1"
	if rec := do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+base+`","new_name":"vm1","target_pool":"thin-test","size_bytes":5,"template":"img1"}`); rec.Code != 201 {
		t.Fatalf("clone: %d %s", rec.Code, rec.Body)
	}
	if m.fromImage != "img1" || m.cloneSize != 5 {
		t.Errorf("CloneImageFrom image=%q size=%d", m.fromImage, m.cloneSize)
	}
	if rec := do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+base+`","new_name":"vm1","target_pool":"thin-test"}`); rec.Code != 201 {
		t.Fatalf("clone without template: %d %s", rec.Code, rec.Body)
	}
	want := []string{
		"CloneImageFrom img1 " + base + " vm1 thin-test",
		"CloneImage " + base + " vm1 thin-test",
	}
	if got := m.calls; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls %q, want %q", got, want)
	}
	// a bad template name is the caller's
	if rec := do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+base+`","new_name":"vm1","template":"a/b"}`); rec.Code != 400 {
		t.Errorf("template a/b: %d %s", rec.Code, rec.Body)
	}
	// a Manager without TemplateCloner gets CloneImage whatever the request says
	plain, ppool := poolFixture(t)
	hp := primServer(t, plain, config.RoleHypervisor)
	plain.volPath = "/x"
	if rec := do(hp, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+filepath.Join(ppool, "b")+`","new_name":"vm1","template":"img1"}`); rec.Code != 201 {
		t.Fatalf("plain: %d %s", rec.Code, rec.Body)
	}
	if got := plain.calls; len(got) != 1 || !strings.HasPrefix(got[0], "CloneImage ") {
		t.Errorf("plain calls %q", got)
	}
}

// A refusal for room is a 409 whatever carries it.
func TestCloneVolumeConflictIs409(t *testing.T) {
	m, pool := newThinMgr(t)
	h := thinServer(t, m)
	m.err = &vmtool.ThinRoomError{Msg: "thin pool thin-test (vg-test) has no room: data use is 80.00%, at or above the 80% limit"}
	rec := do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+filepath.Join(pool, "img1")+`","new_name":"vm1","target_pool":"thin-test","template":"img1"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "80.00%") {
		t.Errorf("room refusal: %d %s, want 409 with the numbers", rec.Code, rec.Body)
	}
	m.err = &app.Error{Kind: app.KindConflict, Op: "clone", Err: fmt.Errorf("busy")}
	if rec := do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+filepath.Join(pool, "img1")+`","new_name":"vm1"}`); rec.Code != 409 {
		t.Errorf("conflict: %d %s", rec.Code, rec.Body)
	}
}

// End to end through the resolver: a real ThinPools over a stub helper whose
// pool is at 80 % answers 409 with the numbers and no helper write ran.
func TestCloneVolumeNoRoomEndToEnd(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "sudo.log")
	report := `{"report":[{"lv":[` +
		`{"lv_name":"thin-test","vg_name":"vg-test","lv_attr":"twi-aotz--","lv_size":"64424509440","pool_lv":"","origin":"","data_percent":"80.00","metadata_percent":"1.00","lv_tags":"vmtool_thinpool","lv_time":""},` +
		fmt.Sprintf(`{"lv_name":%q,"vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"%d","pool_lv":"thin-test","origin":"","data_percent":"","metadata_percent":"","lv_tags":"vmtool_template,vmtool_name=img1,vmtool_sha256=%s,vmtool_origin=image","lv_time":""}`, thinTpl, 10*thinGiB, thinSHA) +
		`]}]}`
	testexec.Stub(t, map[string]string{"sudo": "#!/bin/sh\necho \"$*\" >> '" + log + "'\nprintf '%s' \"$STUB_OUT\"\n"})
	t.Setenv("STUB_OUT", report)

	cacheDir := t.TempDir()
	img := filepath.Join(cacheDir, "img1")
	hdr := make([]byte, 72)
	copy(hdr, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint64(hdr[24:], uint64(10*thinGiB))
	if err := os.WriteFile(img, hdr, 0o644); err != nil {
		t.Fatal(err)
	}
	man := filepath.Join(dir, "image-manifest.yml")
	if err := os.WriteFile(man, []byte(fmt.Sprintf("img1:\n  sha256: %s\n  size: %d\n", thinSHA, len(hdr))), 0o644); err != nil {
		t.Fatal(err)
	}

	inner := &fakeMgr{pools: []vmtool.PoolInfo{{Name: "vmtool", Path: cacheDir, Active: true}}}
	t.Chdir(t.TempDir())
	res := hosts.NewWith("box", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return inner, func() {}, nil }).
		WithThin(&vmtool.Thin{Helper: "/usr/local/libexec/vmtool-thin"}).WithImageManifest(man)
	h := handlerFor(&handlers{hosts: res, role: config.RoleHypervisor})

	rec := do(h, "POST", "/v1/hosts/box/volumes/clone",
		`{"base_path":"`+img+`","new_name":"vm1","target_pool":"thin-test","size_bytes":21474836480,"template":"img1"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "80.00%") || !strings.Contains(rec.Body.String(), "thin-test") {
		t.Fatalf("clone into a full pool: %d %s, want 409 with the numbers", rec.Code, rec.Body)
	}
	b, _ := os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasSuffix(line, " report") {
			t.Errorf("a helper write ran: %q", line)
		}
	}
	for _, c := range inner.calls {
		if strings.HasPrefix(c, "CloneImage") {
			t.Errorf("the libvirt Manager cloned: %q", c)
		}
	}
}

func TestGetImagePathSavedTemplate(t *testing.T) {
	m, _ := newThinMgr(t)
	m.saved["proj"] = "/dev/vg-test/vmtpl-000000000001"
	m.err = nil
	h := thinServer(t, m)

	// the inner lookup finds it: that answer stands
	m.imagePath = "/var/lib/vmtool/images/img1"
	if rec := do(h, "GET", "/v1/hosts/box/images/img1/path", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"/var/lib/vmtool/images/img1"`) {
		t.Fatalf("cached image: %d %s", rec.Code, rec.Body)
	}
	// not found there, a saved template answers
	nf := &notFoundImages{thinMgr: m}
	h = thinServer(t, nf)
	if rec := do(h, "GET", "/v1/hosts/box/images/proj/path", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"/dev/vg-test/vmtpl-000000000001"`) {
		t.Fatalf("saved template: %d %s", rec.Code, rec.Body)
	}
	// neither: the same 404 as today
	rec := do(h, "GET", "/v1/hosts/box/images/nothing/path", "")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "not found in any storage pool") {
		t.Errorf("neither: %d %s", rec.Code, rec.Body)
	}
	// a Manager without thin pools: 404 as before
	plain, _ := poolFixture(t)
	plain.err = fmt.Errorf("image %q not found in any storage pool: %w", "proj", vmtool.ErrNotFound)
	hp := primServer(t, plain, config.RoleHypervisor)
	if rec := do(hp, "GET", "/v1/hosts/box/images/proj/path", ""); rec.Code != 404 {
		t.Errorf("plain: %d %s", rec.Code, rec.Body)
	}
}

// notFoundImages is a thinMgr whose ImagePath finds nothing (the cache has no
// such image).
type notFoundImages struct{ *thinMgr }

func (n *notFoundImages) ImagePath(name string) (string, error) {
	return "", fmt.Errorf("image %q not found in any storage pool: %w", name, vmtool.ErrNotFound)
}

func TestStartThinCleanup(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "sudo.log")
	report := `{"report":[{"lv":[` +
		`{"lv_name":"thin-test","vg_name":"vg-test","lv_attr":"twi-aotz--","lv_size":"64424509440","pool_lv":"","origin":"","data_percent":"1.00","metadata_percent":"1.00","lv_tags":"vmtool_thinpool","lv_time":""},` +
		`{"lv_name":"vmtbld-111111111111","vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"1073741824","pool_lv":"thin-test","origin":"","data_percent":"","metadata_percent":"","lv_tags":"vmtool_building","lv_time":""},` +
		`{"lv_name":"vm1","vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"1073741824","pool_lv":"thin-test","origin":"","data_percent":"","metadata_percent":"","lv_tags":"vmtool_vol","lv_time":""}` +
		`]}]}`
	testexec.Stub(t, map[string]string{"sudo": "#!/bin/sh\necho \"$*\" >> '" + log + "'\nprintf '%s' \"$STUB_OUT\"\n"})
	t.Setenv("STUB_OUT", report)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	mk := func(role string) *hosts.Resolver {
		return hosts.NewWith("box", role, nil, func() (app.Manager, func(), error) { return &fakeMgr{}, func() {}, nil }).
			WithThin(&vmtool.Thin{Helper: "/usr/local/libexec/vmtool-thin"})
	}
	startThinCleanup(config.RoleWorkstation, mk(config.RoleWorkstation))
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a workstation ran the helper")
	}
	startThinCleanup(config.RoleHypervisor, mk(config.RoleHypervisor))
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{
		"-n /usr/local/libexec/vmtool-thin report",
		"-n /usr/local/libexec/vmtool-thin remove vg-test vmtbld-111111111111",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("helper calls:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(buf.String(), "vmtbld-111111111111") {
		t.Errorf("not logged: %s", buf.String())
	}
}
