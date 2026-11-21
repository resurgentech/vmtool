package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// fakeMgr is an app.Manager that records every call and touches no libvirt.
type fakeMgr struct {
	calls []string
	pools []vmtool.PoolInfo

	// returns
	imagePath string
	volPath   string
	ip        string
	err       error  // returned by every primitive when set
	poolsErr  error  // returned by ListPools
	cloneSize uint64 // the sizeBytes CloneImage was last given

	defined vmtool.VMConfig
	waited  time.Duration
}

func (f *fakeMgr) rec(format string, a ...any) { f.calls = append(f.calls, fmt.Sprintf(format, a...)) }

func (f *fakeMgr) Start(n string) error   { f.rec("Start %s", n); return f.err }
func (f *fakeMgr) Stop(n string) error    { f.rec("Stop %s", n); return f.err }
func (f *fakeMgr) Resume(n string) error  { f.rec("Resume %s", n); return f.err }
func (f *fakeMgr) Destroy(n string) error { f.rec("Destroy %s", n); return f.err }
func (f *fakeMgr) Delete(n string, noclone bool) error {
	f.rec("Delete %s %v", n, noclone)
	return f.err
}
func (f *fakeMgr) Info(n string) (*vmtool.VMInfo, error) {
	f.rec("Info %s", n)
	return &vmtool.VMInfo{Name: n}, f.err
}
func (f *fakeMgr) List() ([]vmtool.VMInfo, error) { f.rec("List"); return nil, f.err }
func (f *fakeMgr) WaitForIP(n string, d time.Duration) (string, error) {
	f.rec("WaitForIP %s", n)
	f.waited = d
	return f.ip, f.err
}
func (f *fakeMgr) SetAutostart(n string, e bool) error {
	f.rec("SetAutostart %s %v", n, e)
	return f.err
}
func (f *fakeMgr) Reboot(n string) error              { f.rec("Reboot %s", n); return f.err }
func (f *fakeMgr) ResizeDisk(n string, gb uint) error { f.rec("ResizeDisk %s %d", n, gb); return f.err }
func (f *fakeMgr) MigrateDisk(n, p string) error      { f.rec("MigrateDisk %s %s", n, p); return f.err }
func (f *fakeMgr) CreateVolume(p, n string, gb uint) (string, error) {
	f.rec("CreateVolume %s %s %d", p, n, gb)
	return f.volPath, f.err
}
func (f *fakeMgr) AddDisk(n string, gb uint, p string) (*vmtool.DiskDevice, error) {
	f.rec("AddDisk %s %d %s", n, gb, p)
	return &vmtool.DiskDevice{}, f.err
}
func (f *fakeMgr) ImagePath(n string) (string, error) {
	f.rec("ImagePath %s", n)
	return f.imagePath, f.err
}
func (f *fakeMgr) ListNetworks() ([]string, error) { f.rec("ListNetworks"); return nil, f.err }
func (f *fakeMgr) ListBridges() ([]string, error)  { f.rec("ListBridges"); return nil, f.err }
func (f *fakeMgr) CreateNetwork(n, s, d string) (*vmtool.NetworkInfo, error) {
	f.rec("CreateNetwork %s %s %s", n, s, d)
	return &vmtool.NetworkInfo{Name: n}, f.err
}
func (f *fakeMgr) Network(n string) (*vmtool.NetworkInfo, error) {
	f.rec("Network %s", n)
	return &vmtool.NetworkInfo{Name: n}, f.err
}
func (f *fakeMgr) DeleteNetwork(n string) error { f.rec("DeleteNetwork %s", n); return f.err }
func (f *fakeMgr) ListImagesByPool() (map[string][]string, error) {
	f.rec("ListImagesByPool")
	return nil, f.err
}
func (f *fakeMgr) DeleteImage(n, p string) error { f.rec("DeleteImage %s %s", n, p); return f.err }
func (f *fakeMgr) ListPools() ([]vmtool.PoolInfo, error) {
	// not recorded: every path check calls it
	return f.pools, f.poolsErr
}
func (f *fakeMgr) CreatePool(n, p string) error { f.rec("CreatePool %s %s", n, p); return f.err }
func (f *fakeMgr) CloneImage(base, n, pool string, size uint64) (string, error) {
	f.cloneSize = size
	f.rec("CloneImage %s %s %s", base, n, pool)
	return f.volPath, f.err
}
func (f *fakeMgr) ResizeVolume(p string, b uint64) error {
	f.rec("ResizeVolume %s %d", p, b)
	return f.err
}
func (f *fakeMgr) Define(c vmtool.VMConfig) error {
	f.rec("Define %s", c.Name)
	f.defined = c
	return f.err
}

var _ app.Manager = (*fakeMgr)(nil)

// primServer wires a handler chain whose only host, "box", is backed by fm.
// The role is the server's role (what Config.Role gives the real server).
func primServer(t *testing.T, fm *fakeMgr, role string) http.Handler {
	t.Helper()
	t.Chdir(t.TempDir())
	res := hosts.NewWith("box", role, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
	return handlerFor(&handlers{hosts: res, role: role})
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	h.ServeHTTP(rec, r)
	return rec
}

// poolFixture is a temp "images" pool directory and a manager listing it.
func poolFixture(t *testing.T) (*fakeMgr, string) {
	t.Helper()
	dir := t.TempDir()
	pool := filepath.Join(dir, "images")
	if err := os.Mkdir(pool, 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeMgr{pools: []vmtool.PoolInfo{{Name: "default", Path: pool, Active: true}}}, pool
}

func TestInPool(t *testing.T) {
	root := t.TempDir()
	pool := filepath.Join(root, "pool")
	evil := filepath.Join(root, "pool-evil")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{pool, evil, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(pool, "escape")); err != nil {
		t.Fatal(err)
	}
	// A symlink to a file outside the pool.
	if err := os.WriteFile(filepath.Join(outside, "f.qcow2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "f.qcow2"), filepath.Join(pool, "link.qcow2")); err != nil {
		t.Fatal(err)
	}
	// A pool reached through a symlinked path is the same pool.
	viaLink := filepath.Join(root, "poollink")
	if err := os.Symlink(pool, viaLink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool, "exists.qcow2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &fakeMgr{pools: []vmtool.PoolInfo{
		{Name: "nopath"},
		{Name: "default", Path: pool, Active: true},
	}}

	for _, tc := range []struct {
		name, p string
		ok      bool
	}{
		{"file in pool", filepath.Join(pool, "a.qcow2"), true},
		{"existing file", filepath.Join(pool, "exists.qcow2"), true},
		{"new file in new subdir", filepath.Join(pool, "sub", "deeper", "a.qcow2"), true},
		{"unclean but inside", pool + "/./x/../a.qcow2", true},
		{"via symlinked pool path", filepath.Join(viaLink, "a.qcow2"), true},
		{"pool root itself", pool, false},
		{"dotdot escape", pool + "/../outside/x", false},
		{"dotdot escape to etc", pool + "/../../../../etc/passwd", false},
		{"sibling with shared prefix", filepath.Join(evil, "a.qcow2"), false},
		{"outside entirely", "/etc/passwd", false},
		{"relative", "images/a.qcow2", false},
		{"relative dotdot", "../x", false},
		{"empty", "", false},
		{"symlink dir escape", filepath.Join(pool, "escape", "x.qcow2"), false},
		{"symlink file escape", filepath.Join(pool, "link.qcow2"), false},
	} {
		err := inPool(m, tc.p)
		if tc.ok && err != nil {
			t.Errorf("%s: %q rejected: %v", tc.name, tc.p, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: %q accepted", tc.name, tc.p)
			} else if !errors.Is(err, errOutsidePool) {
				t.Errorf("%s: error %v is not errOutsidePool", tc.name, err)
			}
		}
	}

	// ListPools failing is not an input error.
	bad := &fakeMgr{poolsErr: errors.New("libvirt down")}
	if err := inPool(bad, filepath.Join(pool, "a")); err == nil || errors.Is(err, errOutsidePool) {
		t.Errorf("ListPools failure = %v, want a non-input error", err)
	}
	// No pools with a path: nothing is inside.
	if err := inPool(&fakeMgr{pools: []vmtool.PoolInfo{{Name: "x"}}}, filepath.Join(pool, "a")); !errors.Is(err, errOutsidePool) {
		t.Errorf("no pool paths = %v, want errOutsidePool", err)
	}
}

func TestPrimitiveRoutes(t *testing.T) {
	fm, pool := poolFixture(t)
	h := primServer(t, fm, config.RoleHypervisor)
	base := filepath.Join(pool, "base.qcow2")

	// getImagePath
	fm.imagePath = "/var/lib/libvirt/images/u.qcow2"
	rec := do(h, "GET", "/v1/hosts/box/images/u.qcow2/path", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"path":"/var/lib/libvirt/images/u.qcow2"`) {
		t.Fatalf("getImagePath: %d %s", rec.Code, rec.Body)
	}

	// cloneVolume
	fm.volPath = filepath.Join(pool, "web.qcow2")
	rec = do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+base+`","new_name":"web","target_pool":"fast"}`)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), fm.volPath) {
		t.Fatalf("cloneVolume: %d %s", rec.Code, rec.Body)
	}

	// resizeVolume
	rec = do(h, "POST", "/v1/hosts/box/volumes/resize", `{"path":"`+base+`","size_bytes":21474836480}`)
	if rec.Code != 204 {
		t.Fatalf("resizeVolume: %d %s", rec.Code, rec.Body)
	}

	// createVolume
	fm.volPath = filepath.Join(pool, "data.qcow2")
	rec = do(h, "POST", "/v1/hosts/box/volumes", `{"pool":"default","name":"data","size_gb":5}`)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), fm.volPath) {
		t.Fatalf("createVolume: %d %s", rec.Code, rec.Body)
	}

	// defineVM
	extra := filepath.Join(pool, "web-2.qcow2")
	body := `{"vcpus":2,"memory_gib":4,"disk_path":"` + base + `","disk_size_gb":20,"pool":"default",
	  "network":{"type":"direct","source":"eth0","macvtap_mode":"vepa"},"noclone":true,
	  "disk_cache":"none","extra_disk_size_gb":10,"extra_disk_pool":"fast","extra_disks":[{"path":"` + extra + `","target":"vdb"}]}`
	rec = do(h, "PUT", "/v1/hosts/box/vms/web/definition", body)
	if rec.Code != 204 {
		t.Fatalf("defineVM: %d %s", rec.Code, rec.Body)
	}
	want := vmtool.VMConfig{
		Name: "web", VCPUs: 2, Memory: 4, DiskPath: base, DiskSizeGB: 20, Pool: "default",
		Network: vmtool.NetworkConfig{Type: vmtool.NetworkDirect, Source: "eth0", MacvtapMode: vmtool.MacvtapVEPA},
		Noclone: true, ExtraDiskSizeGB: 10, ExtraDiskPool: "fast",
		ExtraDisks: []vmtool.DiskDevice{{Path: extra, Target: "vdb"}},
		Firmware:   vmtool.FirmwareEFI, // omitted in the body: the default
		DiskCache:  "none",
	}
	if !reflect.DeepEqual(fm.defined, want) {
		t.Errorf("Define got %+v\nwant %+v", fm.defined, want)
	}
	if fm.defined.SSHUser != "" || fm.defined.SSHPass != "" {
		t.Error("Define received guest credentials")
	}

	// deleteVMDefinition: raw, noclone passes through
	for _, c := range []struct {
		q  string
		nc bool
	}{{"", false}, {"?noclone=true", true}} {
		q, nc := c.q, c.nc
		rec = do(h, "DELETE", "/v1/hosts/box/vms/web/definition"+q, "")
		if rec.Code != 204 {
			t.Fatalf("deleteVMDefinition%s: %d %s", q, rec.Code, rec.Body)
		}
		if got := fm.calls[len(fm.calls)-1]; got != fmt.Sprintf("Delete web %v", nc) {
			t.Errorf("deleteVMDefinition%s called %q", q, got)
		}
	}

	// waitVMIP
	fm.ip = "192.168.122.9"
	rec = do(h, "POST", "/v1/hosts/box/vms/web/wait-ip", `{"timeout_seconds":30}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ip":"192.168.122.9"`) || fm.waited != 30*time.Second {
		t.Fatalf("waitVMIP: %d %s waited=%v", rec.Code, rec.Body, fm.waited)
	}
	rec = do(h, "POST", "/v1/hosts/box/vms/web/wait-ip", `{}`)
	if rec.Code != 200 || fm.waited != 120*time.Second {
		t.Fatalf("waitVMIP default: %d waited=%v", rec.Code, fm.waited)
	}

	// resizeVMVolume
	rec = do(h, "POST", "/v1/hosts/box/vms/web/resize-volume", `{"size_gb":40}`)
	if rec.Code != 204 {
		t.Fatalf("resizeVMVolume: %d %s", rec.Code, rec.Body)
	}

	wantCalls := []string{
		"ImagePath u.qcow2",
		"CloneImage " + base + " web fast",
		"ResizeVolume " + base + " 21474836480",
		"CreateVolume default data 5",
		"Define web",
		"Delete web false",
		"Delete web true",
		"WaitForIP web",
		"WaitForIP web",
		"ResizeDisk web 40",
	}
	if !reflect.DeepEqual(fm.calls, wantCalls) {
		t.Errorf("calls:\n got %q\nwant %q", fm.calls, wantCalls)
	}
}

func TestPrimitivePathRejected(t *testing.T) {
	fm, pool := poolFixture(t)
	h := primServer(t, fm, config.RoleWorkstation)
	inside := filepath.Join(pool, "ok.qcow2")
	for _, tc := range []struct{ name, method, path, body string }{
		{"clone base outside", "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"/etc/passwd","new_name":"x"}`},
		{"clone base dotdot", "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"` + pool + `/../../etc/passwd","new_name":"x"}`},
		{"clone base relative", "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"base.qcow2","new_name":"x"}`},
		{"resize outside", "POST", "/v1/hosts/box/volumes/resize", `{"path":"/etc/passwd","size_bytes":1}`},
		{"resize sibling prefix", "POST", "/v1/hosts/box/volumes/resize", `{"path":"` + pool + `-evil/x","size_bytes":1}`},
		{"define disk_path outside", "PUT", "/v1/hosts/box/vms/web/definition",
			`{"vcpus":1,"memory_gib":1,"disk_path":"/etc/shadow","network":{"type":"nat"}}`},
		{"define extra disk outside", "PUT", "/v1/hosts/box/vms/web/definition",
			`{"vcpus":1,"memory_gib":1,"disk_path":"` + inside + `","network":{"type":"nat"},"extra_disks":[{"path":"/dev/sda","target":"vdb"}]}`},
	} {
		rec := do(h, tc.method, tc.path, tc.body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", tc.name, rec.Code, rec.Body)
			continue
		}
		if !strings.Contains(rec.Body.String(), "storage pool") {
			t.Errorf("%s: unclear message %s", tc.name, rec.Body)
		}
	}
	if len(fm.calls) != 0 {
		t.Errorf("a mutating Manager method ran after a rejected path: %q", fm.calls)
	}
}

func TestPrimitiveBadInput(t *testing.T) {
	fm, pool := poolFixture(t)
	h := primServer(t, fm, config.RoleWorkstation)
	inside := filepath.Join(pool, "ok.qcow2")
	def := func(extra string) string {
		return `{"vcpus":1,"memory_gib":1,"disk_path":"` + inside + `","network":{"type":"nat"` + extra + `}}`
	}
	for _, tc := range []struct{ name, method, path, body string }{
		{"clone no body", "POST", "/v1/hosts/box/volumes/clone", ``},
		{"clone empty names", "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"` + inside + `","new_name":""}`},
		{"clone name slash", "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"` + inside + `","new_name":"../x"}`},
		{"clone name xml", "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"` + inside + `","new_name":"a<b"}`},
		{"resize zero", "POST", "/v1/hosts/box/volumes/resize", `{"path":"` + inside + `","size_bytes":0}`},
		{"create zero size", "POST", "/v1/hosts/box/volumes", `{"name":"a","size_gb":0}`},
		{"create name slash", "POST", "/v1/hosts/box/volumes", `{"name":"a/b","size_gb":1}`},
		{"create no name", "POST", "/v1/hosts/box/volumes", `{"size_gb":1}`},
		{"define no vcpus", "PUT", "/v1/hosts/box/vms/web/definition", `{"memory_gib":1,"disk_path":"` + inside + `","network":{"type":"nat"}}`},
		{"define bad vm name", "PUT", "/v1/hosts/box/vms/a%3Cb/definition", def("")},
		{"define xml in source", "PUT", "/v1/hosts/box/vms/web/definition", def(`,"source":"x'/><evil/>"`)},
		{"define bad net type", "PUT", "/v1/hosts/box/vms/web/definition", `{"vcpus":1,"memory_gib":1,"disk_path":"` + inside + `","network":{"type":"weird"}}`},
		{"wait zero", "POST", "/v1/hosts/box/vms/web/wait-ip", `{"timeout_seconds":0}`},
		{"wait too long", "POST", "/v1/hosts/box/vms/web/wait-ip", `{"timeout_seconds":3601}`},
		{"wait negative", "POST", "/v1/hosts/box/vms/web/wait-ip", `{"timeout_seconds":-1}`},
		{"resize-volume zero", "POST", "/v1/hosts/box/vms/web/resize-volume", `{"size_gb":0}`},
		{"resize-volume no body", "POST", "/v1/hosts/box/vms/web/resize-volume", ``},
	} {
		rec := do(h, tc.method, tc.path, tc.body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", tc.name, rec.Code, rec.Body)
		}
	}
	if len(fm.calls) != 0 {
		t.Errorf("Manager called for bad input: %q", fm.calls)
	}
}

func TestPrimitiveErrors(t *testing.T) {
	fm, pool := poolFixture(t)
	h := primServer(t, fm, config.RoleWorkstation)
	inside := filepath.Join(pool, "ok.qcow2")
	reqs := []struct{ name, method, path, body string }{
		{"getImagePath", "GET", "/v1/hosts/box/images/u/path", ""},
		{"createVolume", "POST", "/v1/hosts/box/volumes", `{"name":"a","size_gb":1}`},
		{"cloneVolume", "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"` + inside + `","new_name":"a"}`},
		{"resizeVolume", "POST", "/v1/hosts/box/volumes/resize", `{"path":"` + inside + `","size_bytes":9}`},
		{"defineVM", "PUT", "/v1/hosts/box/vms/web/definition", `{"vcpus":1,"memory_gib":1,"disk_path":"` + inside + `","network":{"type":"nat"}}`},
		{"deleteVMDefinition", "DELETE", "/v1/hosts/box/vms/web/definition", ""},
		{"waitVMIP", "POST", "/v1/hosts/box/vms/web/wait-ip", `{}`},
		{"resizeVMVolume", "POST", "/v1/hosts/box/vms/web/resize-volume", `{"size_gb":3}`},
	}
	// A missing image or domain is 404; anything else is 500.
	for _, tc := range reqs {
		fm.err = fmt.Errorf("boom: %w", vmtool.ErrNotFound)
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != 404 {
			t.Errorf("%s not found: %d %s, want 404", tc.name, rec.Code, rec.Body)
		}
		fm.err = errors.New("libvirt exploded")
		rec := do(h, tc.method, tc.path, tc.body)
		if rec.Code != 500 || !strings.Contains(rec.Body.String(), "libvirt exploded") {
			t.Errorf("%s failure: %d %s, want 500", tc.name, rec.Code, rec.Body)
		}
	}
}

func TestPrimitiveUnknownHost404(t *testing.T) {
	h := handlerFor(&handlers{hosts: noLibvirt(t)})
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/hosts/nope/images/u/path", ""},
		{"POST", "/v1/hosts/nope/volumes", `{"name":"a","size_gb":1}`},
		{"POST", "/v1/hosts/nope/volumes/clone", `{"base_path":"/p/a","new_name":"a"}`},
		{"POST", "/v1/hosts/nope/volumes/resize", `{"path":"/p/a","size_bytes":9}`},
		{"PUT", "/v1/hosts/nope/vms/web/definition", `{"vcpus":1,"memory_gib":1,"disk_path":"/p/a","network":{"type":"nat"}}`},
		{"DELETE", "/v1/hosts/nope/vms/web/definition", ""},
		{"POST", "/v1/hosts/nope/vms/web/wait-ip", `{}`},
		{"POST", "/v1/hosts/nope/vms/web/resize-volume", `{"size_gb":3}`},
	} {
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != 404 {
			t.Errorf("%s %s: %d %s, want 404", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}

// workstationOnly lists the operations the spec marks
// x-vmtool-workstation-only.
func workstationOnly(t *testing.T) []specOp {
	var out []specOp
	for _, o := range specOps(t) {
		if _, ok := o.op.Extensions["x-vmtool-workstation-only"]; ok {
			out = append(out, o)
		}
	}
	return out
}

func TestWorkstationOnlySet(t *testing.T) {
	var got []string
	for _, o := range workstationOnly(t) {
		got = append(got, o.id)
	}
	sort.Strings(got)
	want := []string{
		"addMachine", "createVM", "execMachine", "execVM", "getInventory", "getMachine",
		"listMachines", "listPlaybooks", "openMachineConsole", "openVMConsole",
		"rebootMachine", "removeMachine", "resizeDisk", "runMachinePlaybook",
		"runPlaybook", "setVMComment",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("workstation-only operations\n got %v\nwant %v", got, want)
	}
	// Every /v1/metal operation is in the set, and each documents the 403.
	marked := map[string]bool{}
	for _, o := range workstationOnly(t) {
		marked[o.id] = true
		if o.op.Responses.Value("403") == nil {
			t.Errorf("%s has no 403 response", o.id)
		}
		if !strings.Contains(o.op.Description, "served by the workstation") {
			t.Errorf("%s description does not mention the workstation", o.id)
		}
	}
	for _, o := range specOps(t) {
		if strings.HasPrefix(o.path, "/v1/metal") && !marked[o.id] {
			t.Errorf("metal operation %s is not workstation-only", o.id)
		}
		// The image cache is the inverse: served by hypervisors, refused
		// (403) by a workstation in its handler (images.go).
		hypervisorOnly := o.id == "uploadImage" || o.id == "getImageManifest" || o.id == "fetchImage" ||
			o.id == "exportTemplate" || o.id == "getImageBlob"
		if !marked[o.id] && !hypervisorOnly && o.op.Responses.Value("403") != nil {
			t.Errorf("%s documents 403 but is not marked", o.id)
		}
	}
}

func TestHypervisorRole403(t *testing.T) {
	ops := workstationOnly(t)
	if len(ops) == 0 {
		t.Fatal("no workstation-only operations in the spec")
	}
	connected := 0
	res := func(role string) http.Handler {
		r := hosts.NewWith("box", role, nil, func() (app.Manager, func(), error) {
			connected++
			return &fakeMgr{}, func() {}, nil
		})
		return handlerFor(&handlers{hosts: r, role: role})
	}
	t.Chdir(t.TempDir())
	hyp, ws := res(config.RoleHypervisor), res(config.RoleWorkstation)
	for _, o := range ops {
		p := concrete(o.path)
		label := o.method + " " + p + " (" + o.id + ")"
		// Malformed body: the 403 comes before any body parsing.
		rec := do(hyp, strings.ToUpper(o.method), p, "{not json")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s on hypervisor: %d %s, want 403", label, rec.Code, rec.Body)
			continue
		}
		var e api.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil ||
			!strings.Contains(e.Error, "served by the workstation") ||
			!strings.Contains(e.Error, o.id) || !strings.Contains(e.Error, "box") {
			t.Errorf("%s: 403 body %q", label, rec.Body)
		}
		rec = do(ws, strings.ToUpper(o.method), p, "")
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s on workstation: 403", label)
		}
	}
	if connected != 0 {
		t.Errorf("libvirt connector ran %d times", connected)
	}
	// Non-gated operations are still served by a hypervisor.
	for _, p := range []string{"/v1/hosts/box/vms", "/v1/hosts/box/pools", "/v1/hosts"} {
		if rec := do(hyp, "GET", p, ""); rec.Code == http.StatusForbidden {
			t.Errorf("GET %s on hypervisor: 403", p)
		}
	}
}

func TestHypervisorRoleOverRealRouter(t *testing.T) {
	// Handler() reads the role from config.yml: a hypervisor refuses the
	// workflow routes with 403 after the bearer check, and 401 before it.
	t.Chdir(t.TempDir())
	s := authedServer()
	s.Config.Role = config.RoleHypervisor
	h := s.Handler()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/hosts/x/vms"},
		{"POST", "/v1/hosts/x/vms/y/exec"},
		{"GET", "/v1/metal"},
		{"GET", "/v1/playbooks"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		check401(t, rec, tc.method+" "+tc.path+" no token")

		rec = httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{x"))
		r.Header.Set("Authorization", "Bearer "+testToken)
		h.ServeHTTP(rec, r)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "served by the workstation") {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}

// cmd became exec in API 2.0.0, with no alias: the old paths are not routes,
// for either role, even with a valid token.
func TestOldCmdRoutesAreGone(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, role := range []string{"", config.RoleHypervisor} {
		s := authedServer()
		s.Config.Role = role
		h := s.Handler()
		for _, path := range []string{"/v1/hosts/x/vms/y/cmd", "/v1/metal/y/cmd"} {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"command":["true"]}`))
			r.Header.Set("Authorization", "Bearer "+testToken)
			h.ServeHTTP(rec, r)
			if rec.Code != 404 {
				t.Errorf("role %q POST %s: %d %s, want 404", role, path, rec.Code, rec.Body)
			}
		}
	}
}
