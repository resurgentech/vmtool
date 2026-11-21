package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/remote"
	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// rtMgr is a goroutine-safe fake libvirt app.Manager for the end-to-end
// tests: every call is recorded with its arguments, every result is a fixed,
// fully populated value so the round trip can be compared field by field.
type rtMgr struct {
	mu    sync.Mutex
	calls []string

	pool    string // the fake storage pool directory
	info    vmtool.VMInfo
	vms     []vmtool.VMInfo
	defined vmtool.VMConfig

	cloneSize uint64 // the sizeBytes CloneImage was last given
}

func (f *rtMgr) rec(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

func (f *rtMgr) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *rtMgr) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func (f *rtMgr) Start(n string) error   { f.rec("Start %s", n); return nil }
func (f *rtMgr) Stop(n string) error    { f.rec("Stop %s", n); return nil }
func (f *rtMgr) Resume(n string) error  { f.rec("Resume %s", n); return nil }
func (f *rtMgr) Destroy(n string) error { f.rec("Destroy %s", n); return nil }
func (f *rtMgr) Delete(n string, noclone bool) error {
	f.rec("Delete %s %v", n, noclone)
	return nil
}
func (f *rtMgr) Info(n string) (*vmtool.VMInfo, error) {
	f.rec("Info %s", n)
	if n == "ghost" {
		return nil, fmt.Errorf("domain %q: %w", n, vmtool.ErrNotFound)
	}
	i := f.info
	i.Name = n
	return &i, nil
}
func (f *rtMgr) List() ([]vmtool.VMInfo, error) { f.rec("List"); return f.vms, nil }
func (f *rtMgr) WaitForIP(n string, d time.Duration) (string, error) {
	f.rec("WaitForIP %s %s", n, d)
	return "192.0.2.44", nil
}
func (f *rtMgr) SetAutostart(n string, e bool) error { f.rec("SetAutostart %s %v", n, e); return nil }
func (f *rtMgr) SetComment(n, c string) error        { f.rec("SetComment %s %s", n, c); return nil }
func (f *rtMgr) Reboot(n string) error               { f.rec("Reboot %s", n); return nil }
func (f *rtMgr) ResizeDisk(n string, gb uint) error  { f.rec("ResizeDisk %s %d", n, gb); return nil }
func (f *rtMgr) MigrateDisk(n, p string) error       { f.rec("MigrateDisk %s %s", n, p); return nil }
func (f *rtMgr) CreateVolume(p, n string, gb uint) (string, error) {
	f.rec("CreateVolume %s %s %d", p, n, gb)
	return filepath.Join(f.pool, n), nil
}
func (f *rtMgr) AddDisk(n string, gb uint, p string) (*vmtool.DiskDevice, error) {
	f.rec("AddDisk %s %d %s", n, gb, p)
	return &vmtool.DiskDevice{Path: filepath.Join(f.pool, n+"-2.qcow2"), Target: "vdb"}, nil
}
func (f *rtMgr) ImagePath(n string) (string, error) {
	f.rec("ImagePath %s", n)
	if n == "missing.qcow2" {
		return "", fmt.Errorf("image %q not found in any storage pool: %w", n, vmtool.ErrNotFound)
	}
	return filepath.Join(f.pool, n), nil
}
func (f *rtMgr) ListNetworks() ([]string, error) {
	f.rec("ListNetworks")
	return []string{"default", "lab"}, nil
}
func (f *rtMgr) ListBridges() ([]string, error) { f.rec("ListBridges"); return []string{"br0"}, nil }
func (f *rtMgr) ListImagesByPool() (map[string][]string, error) {
	f.rec("ListImagesByPool")
	return map[string][]string{"default": {"a.qcow2", "b.qcow2"}, "fast": {"c.qcow2"}}, nil
}
func (f *rtMgr) DeleteImage(n, p string) error { f.rec("DeleteImage %s %s", n, p); return nil }
func (f *rtMgr) ListPools() ([]vmtool.PoolInfo, error) {
	// not recorded: every path check calls it
	return []vmtool.PoolInfo{{Name: vmtool.HypervisorPool, Path: f.pool, Active: true}, {Name: "cold", Path: "/nonexistent-cold", Active: false}}, nil
}
func (f *rtMgr) CreatePool(n, p string) error { f.rec("CreatePool %s %s", n, p); return nil }
func (f *rtMgr) CloneImage(base, n, pool string, size uint64) (string, error) {
	f.mu.Lock()
	f.cloneSize = size
	f.mu.Unlock()
	f.rec("CloneImage %s %s %s", base, n, pool)
	return filepath.Join(f.pool, n+".qcow2"), nil
}
func (f *rtMgr) ResizeVolume(p string, b uint64) error { f.rec("ResizeVolume %s %d", p, b); return nil }
func (f *rtMgr) Define(c vmtool.VMConfig) error {
	f.rec("Define %s", c.Name)
	f.mu.Lock()
	f.defined = c
	f.mu.Unlock()
	return nil
}

var _ app.Manager = (*rtMgr)(nil)

func newRTMgr(t *testing.T) *rtMgr {
	t.Helper()
	pool := filepath.Join(t.TempDir(), "images")
	if err := os.Mkdir(pool, 0o755); err != nil {
		t.Fatal(err)
	}
	return &rtMgr{
		pool: pool,
		info: vmtool.VMInfo{State: vmtool.StateRunning, VCPUs: 3, Memory: 5, IP: "192.0.2.44", Autostart: true,
			Comment: "placeholder comment",
			Origin: &vmtool.VMOrigin{Image: "img1", ImageSHA256: strings.Repeat("a", 64), CloneKind: vmtool.CloneKindFile,
				Pool: "vmtool", Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}},
		vms: []vmtool.VMInfo{
			{Name: "a", State: vmtool.StateRunning, VCPUs: 2, Memory: 4, IP: "192.0.2.1", Autostart: true, Comment: "placeholder comment",
				Origin: &vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindNone, Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}},
			{Name: "b", State: vmtool.StateShutoff, VCPUs: 1, Memory: 1},
		},
	}
}

// seedCached puts name in the hypervisor's vmtool pool directory and lists
// it in the daemon's manifest (image-manifest.yml in the working directory),
// as a previous upload would have.
func seedCached(t *testing.T, poolDir, name string) {
	t.Helper()
	data := []byte("cached image " + name)
	if err := os.WriteFile(filepath.Join(poolDir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := vmtool.OpenImageManifest(vmtool.ImageManifestFile).Update(func(es map[string]vmtool.ImageEntry) error {
		es[name] = vmtool.ImageEntry{SHA256: hexSum(data), Size: int64(len(data))}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// authLog wraps a handler and records every request's Authorization header.
type authLog struct {
	mu    sync.Mutex
	auths []string
	next  http.Handler
}

func (a *authLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.auths = append(a.auths, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	a.mu.Unlock()
	a.next.ServeHTTP(w, r)
}

func (a *authLog) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

// hypervisor starts the real handler chain (bearer check, hypervisor gate)
// over TLS for a hypervisor named veritech01 whose libvirt is fm.
func hypervisor(t *testing.T, fm app.Manager, token string) (*httptest.Server, *x509.CertPool, *authLog) {
	t.Helper()
	res := hosts.NewWith("veritech01", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
	h := chain(&handlers{hosts: res, role: config.RoleHypervisor}, func(next http.Handler) http.Handler {
		return requireBearer(config.Hash(token), publicRoutes(), next)
	})
	log := &authLog{next: h}
	srv := httptest.NewUnstartedServer(log)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool, log
}

func remoteFor(t *testing.T, srv *httptest.Server, pool *x509.CertPool, token string) *remote.Manager {
	t.Helper()
	m, err := remote.New("veritech01", srv.URL, token, remote.WithRootCAs(pool))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// Every app.Manager method, through remote.Manager and the real handler,
// reaches the hypervisor's Manager with the same arguments, and every result
// comes back field for field.
func TestRemoteManagerEveryMethod(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)
	vol := filepath.Join(fm.pool, "web.qcow2")

	wantInfo := func(name string) *vmtool.VMInfo {
		i := fm.info
		i.Name = name
		return &i
	}
	check := func(what string, err error, calls ...string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
		if got := fm.got(); !reflect.DeepEqual(got, calls) {
			t.Errorf("%s: calls %q, want %q", what, got, calls)
		}
		fm.reset()
	}

	// Lifecycle routes run Manager.X then Info; the client keeps only the error.
	check("Start", m.Start("web"), "Start web", "Info web")
	check("Stop", m.Stop("web"), "Stop web", "Info web")
	check("Resume", m.Resume("web"), "Resume web", "Info web")
	check("Destroy", m.Destroy("web"), "Destroy web", "Info web")
	check("Reboot", m.Reboot("web"), "Reboot web", "Info web")
	check("SetAutostart", m.SetAutostart("web", true), "SetAutostart web true", "Info web")
	check("SetAutostart off", m.SetAutostart("web", false), "SetAutostart web false", "Info web")
	check("SetComment", m.SetComment("web", "placeholder comment"), "SetComment web placeholder comment", "Info web")
	check("SetComment clear", m.SetComment("web", ""), "SetComment web ", "Info web")
	check("Delete", m.Delete("web", false), "Delete web false")
	check("Delete noclone", m.Delete("web", true), "Delete web true")
	check("ResizeDisk", m.ResizeDisk("web", 40), "ResizeDisk web 40")
	check("MigrateDisk", m.MigrateDisk("web", "fast"), "MigrateDisk web fast", "Info web")
	check("DeleteImage", m.DeleteImage("a.qcow2", "fast"), "DeleteImage a.qcow2 fast")
	check("CreatePool", m.CreatePool("fast", "/srv/fast"), "CreatePool fast /srv/fast")
	check("ResizeVolume", m.ResizeVolume(vol, 21474836480), "ResizeVolume "+vol+" 21474836480")

	info, err := m.Info("web")
	check("Info", err, "Info web")
	if !reflect.DeepEqual(info, wantInfo("web")) {
		t.Errorf("Info = %+v, want %+v", info, wantInfo("web"))
	}
	vms, err := m.List()
	check("List", err, "List")
	if !reflect.DeepEqual(vms, fm.vms) {
		t.Errorf("List = %+v, want %+v", vms, fm.vms)
	}
	ip, err := m.WaitForIP("web", 75*time.Second)
	check("WaitForIP", err, "WaitForIP web 1m15s")
	if ip != "192.0.2.44" {
		t.Errorf("WaitForIP = %q", ip)
	}
	p, err := m.CreateVolume("default", "web-2.qcow2", 7)
	check("CreateVolume", err, "CreateVolume default web-2.qcow2 7")
	if p != filepath.Join(fm.pool, "web-2.qcow2") {
		t.Errorf("CreateVolume = %q", p)
	}
	d, err := m.AddDisk("web", 9, "fast")
	check("AddDisk", err, "AddDisk web 9 fast", "Info web")
	if !reflect.DeepEqual(d, &vmtool.DiskDevice{Path: filepath.Join(fm.pool, "web-2.qcow2"), Target: "vdb"}) {
		t.Errorf("AddDisk = %+v", d)
	}
	p, err = m.ImagePath("base.qcow2")
	check("ImagePath", err, "ImagePath base.qcow2")
	if p != filepath.Join(fm.pool, "base.qcow2") {
		t.Errorf("ImagePath = %q", p)
	}
	nets, err := m.ListNetworks()
	check("ListNetworks", err, "ListNetworks")
	if !reflect.DeepEqual(nets, []string{"default", "lab"}) {
		t.Errorf("ListNetworks = %v", nets)
	}
	brs, err := m.ListBridges()
	check("ListBridges", err, "ListBridges")
	if !reflect.DeepEqual(brs, []string{"br0"}) {
		t.Errorf("ListBridges = %v", brs)
	}
	imgs, err := m.ListImagesByPool()
	check("ListImagesByPool", err, "ListImagesByPool")
	if !reflect.DeepEqual(imgs, map[string][]string{"default": {"a.qcow2", "b.qcow2"}, "fast": {"c.qcow2"}}) {
		t.Errorf("ListImagesByPool = %v", imgs)
	}
	pools, err := m.ListPools()
	check("ListPools", err)
	if want, _ := fm.ListPools(); !reflect.DeepEqual(pools, want) {
		t.Errorf("ListPools = %+v, want %+v", pools, want)
	}
	p, err = m.CloneImage(filepath.Join(fm.pool, "base.qcow2"), "web", "default", 0)
	check("CloneImage", err, "CloneImage "+filepath.Join(fm.pool, "base.qcow2")+" web default")
	if p != vol {
		t.Errorf("CloneImage = %q", p)
	}
	// size_bytes reaches the hypervisor's Manager; 0 is not sent and arrives as 0.
	if fm.cloneSize != 0 {
		t.Errorf("CloneImage size 0 arrived as %d", fm.cloneSize)
	}
	_, err = m.CloneImage(filepath.Join(fm.pool, "base.qcow2"), "web", "default", 5<<30)
	check("CloneImage sized", err, "CloneImage "+filepath.Join(fm.pool, "base.qcow2")+" web default")
	if fm.cloneSize != 5<<30 {
		t.Errorf("CloneImage size 5GiB arrived as %d", fm.cloneSize)
	}

	cfg := vmtool.VMConfig{
		Name: "web", VCPUs: 4, Memory: 8, DiskPath: vol, DiskSizeGB: 30, Pool: "default",
		Network: vmtool.NetworkConfig{Type: vmtool.NetworkDirect, Source: "eno1", MacvtapMode: vmtool.MacvtapMode("vepa")},
		SSHUser: "secret-user", SSHPass: "secret-pass",
		Noclone:         true,
		Firmware:        vmtool.FirmwareBIOS, // travels explicitly to the daemon
		DiskCache:       "writeback",         // so does the disk cache
		ExtraDiskSizeGB: 6, ExtraDiskPool: "fast",
		ExtraDisks: []vmtool.DiskDevice{{Path: filepath.Join(fm.pool, "web-2.qcow2"), Target: "vdb"}},
		Comment:    "placeholder comment",
		Origin: &vmtool.VMOrigin{Image: "img1", ImageSHA256: strings.Repeat("a", 64), Template: "img1", TemplateLV: "vmtpl-aaaaaaaaaaaa",
			CloneKind: vmtool.CloneKindThin, Pool: "default", Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)},
	}
	check("Define", m.Define(cfg), "Define web")
	want := cfg
	want.SSHUser, want.SSHPass = "", "" // never sent
	fm.mu.Lock()
	defined := fm.defined
	fm.mu.Unlock()
	if !reflect.DeepEqual(defined, want) {
		t.Errorf("Define reached libvirt with\n%+v\nwant\n%+v", defined, want)
	}

	// A daemon 404 is the app layer's not-found.
	_, err = m.Info("ghost")
	if !vmtool.IsNotFound(err) || !strings.Contains(err.Error(), "veritech01") {
		t.Errorf("Info(ghost) = %v, want a not-found naming the host", err)
	}
	if _, err := (&app.Service{}).GetVM(context.Background(), m, "ghost"); kindOf(err) != app.KindNotFound {
		t.Errorf("GetVM(ghost) kind = %v (%v)", kindOf(err), err)
	}
	if _, err := m.ImagePath("missing.qcow2"); !vmtool.IsNotFound(err) {
		t.Errorf("ImagePath(missing) = %v", err)
	}
}

// app.Service.CreateVM on the workstation drives a remote hypervisor end to
// end through remote.Manager, with the base image already in the
// hypervisor's pool. The guest SSH wait runs against the stub ansible.
func TestCreateVMOverRemote(t *testing.T) {
	t.Chdir(t.TempDir())
	testexec.Ansible(t)
	fm := newRTMgr(t)
	seedCached(t, fm.pool, "ubuntu.qcow2")
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)

	// No LocalImages: the workstation has no copy, so the hypervisor's
	// cached one is used as it is.
	svc := &app.Service{IPTimeout: 7 * time.Second, Tunnel: vmtool.Tunnel{Exe: stubTunnelExe}}
	var stages []string
	info, err := svc.CreateVM(context.Background(), m, app.CreateVMInput{
		Host: "veritech01", Name: "web", Image: "ubuntu.qcow2",
		VCPUs: 4, Memory: 8, ExtraDiskSizeGB: 5,
		SSHUser: "guest-user", SSHPass: "guest-pass",
		OnProgress: func(ev app.ProgressEvent) {
			if ev.Status == "done" {
				stages = append(stages, ev.Stage)
			}
		},
	})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	base := filepath.Join(fm.pool, "ubuntu.qcow2")
	vol := filepath.Join(fm.pool, "web.qcow2")
	want := []string{
		"ImagePath ubuntu.qcow2",
		"CloneImage " + base + " web vmtool",
		"CreateVolume vmtool web-2.qcow2 5",
		"Define web",
		"Start web", "Info web",
		"WaitForIP web 7s",
		"Info web",
		"Info web",
	}
	if got := fm.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("hypervisor calls\n%q\nwant\n%q", got, want)
	}
	if strings.Join(stages, ",") != "upload,clone,extra_disk,start,wait_ip,wait_ssh" {
		t.Errorf("stages = %v", stages)
	}
	if info == nil || info.Name != "web" || info.IP != "192.0.2.44" || info.VCPUs != 3 {
		t.Errorf("info = %+v", info)
	}
	fm.mu.Lock()
	d := fm.defined
	fm.mu.Unlock()
	if d.DiskCache != "unsafe" {
		t.Errorf("the daemon defined disk cache %q, want the clone default unsafe sent explicitly", d.DiskCache)
	}
	if d.Firmware != vmtool.FirmwareEFI {
		t.Errorf("the daemon defined firmware %q, want the default efi sent explicitly", d.Firmware)
	}
	if d.DiskPath != vol || d.VCPUs != 4 || d.Memory != 8 || d.SSHUser != "" || d.SSHPass != "" ||
		len(d.ExtraDisks) != 1 || d.ExtraDisks[0] != (vmtool.DiskDevice{Path: filepath.Join(fm.pool, "web-2.qcow2"), Target: "vdb"}) {
		t.Errorf("defined %+v", d)
	}
}

// workstation is the real handler chain for a workstation named ws whose one
// configured host, veritech01, is served by a remote.Manager pointed at
// hvURL. The local libvirt is a tripwire.
func workstation(t *testing.T, hvURL string, pool *x509.CertPool, token string, h *handlers) http.Handler {
	t.Helper()
	res := hosts.NewWith("ws", config.RoleWorkstation, []string{"veritech01"}, func() (app.Manager, func(), error) {
		t.Error("workstation libvirt opened")
		return nil, nil, errors.New("no libvirt")
	}).WithRemote(func(host, tok string) (app.Manager, func(), error) {
		m, err := remote.New(host, hvURL, tok, remote.WithRootCAs(pool))
		if err != nil {
			return nil, nil, err
		}
		return m, m.Close, nil
	})
	h.hosts = res
	h.role = config.RoleWorkstation
	return chain(h, func(next http.Handler) http.Handler {
		return requireBearer(config.Hash(token), publicRoutes(), next)
	})
}

// The workstation server acting on a remote host forwards the incoming
// request's bearer and holds no token of its own: the hypervisor sees exactly
// the caller's token on every request, workflow (createVM) included.
func TestWorkstationForwardsBearer(t *testing.T) {
	t.Chdir(t.TempDir())
	const callerToken = "test-caller-token-91b7"
	fm := newRTMgr(t)
	seedCached(t, fm.pool, "ubuntu.qcow2")
	hv, pool, hvLog := hypervisor(t, fm, callerToken)
	testexec.Ansible(t)
	ws := workstation(t, hv.URL, pool, callerToken, &handlers{tunnelExe: stubTunnelExe})

	req := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+callerToken)
		rec := httptest.NewRecorder()
		ws.ServeHTTP(rec, r)
		return rec
	}

	rec := req("GET", "/v1/hosts/veritech01/vms/web", "")
	if rec.Code != 200 {
		t.Fatalf("GET vm: %d %s", rec.Code, rec.Body)
	}
	var vm api.VMInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &vm); err != nil || vm.Name != "web" || vm.Ip != "192.0.2.44" || vm.Vcpus != 3 {
		t.Fatalf("vm = %+v (%v)", vm, err)
	}
	rec = req("POST", "/v1/hosts/veritech01/vms", `{"name":"web","image":"ubuntu.qcow2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("createVM: %d %s", rec.Code, rec.Body)
	}
	rec = req("GET", "/v1/hosts/veritech01/pools", "")
	if rec.Code != 200 {
		t.Fatalf("pools: %d %s", rec.Code, rec.Body)
	}
	// A daemon 404 is a 404 at the workstation.
	rec = req("GET", "/v1/hosts/veritech01/vms/ghost", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "veritech01") {
		t.Fatalf("ghost: %d %s", rec.Code, rec.Body)
	}

	seen := hvLog.seen()
	if len(seen) < 5 {
		t.Fatalf("hypervisor saw only %v", seen)
	}
	for _, s := range seen {
		if !strings.HasSuffix(s, " Bearer "+callerToken) {
			t.Errorf("hypervisor saw %q, want the caller's bearer", s)
		}
	}
	if !strings.Contains(strings.Join(fm.got(), ","), "CloneImage") {
		t.Errorf("createVM did not reach the hypervisor: %v", fm.got())
	}
}

// A workstation request without a bearer never reaches the remote: it is
// refused by the workstation's own check, and handlers that bypass it (the
// handler-level chain) have no token to forward.
func TestWorkstationNoTokenNoForward(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	hv, pool, hvLog := hypervisor(t, fm, testToken)
	h := &handlers{}
	workstation(t, hv.URL, pool, testToken, h)
	unauth := handlerFor(h)
	rec := httptest.NewRecorder()
	unauth.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/hosts/veritech01/vms", nil))
	if rec.Code == 200 {
		t.Fatalf("no-token request served: %s", rec.Body)
	}
	if s := hvLog.seen(); len(s) != 0 {
		t.Fatalf("hypervisor contacted without a token: %v", s)
	}
}

// On a workstation, the tunnel to a remote host's VM is that host's daemon's
// job: the workstation refuses it without looking up the VM or dialling
// anything (guests on different hosts share NAT subnets).
func TestTunnelRefusesRemoteHost(t *testing.T) {
	h := &handlers{
		vmIP: func(context.Context, string, string) (string, error) {
			t.Error("VM lookup for a remote host's tunnel")
			return "192.0.2.10", nil
		},
		dialGuest: func(context.Context, string) (net.Conn, error) {
			t.Error("dial for a remote host's tunnel")
			return nil, errors.New("no")
		},
	}
	res := hosts.NewWith("ws", config.RoleWorkstation, []string{"veritech01"}, func() (app.Manager, func(), error) {
		t.Error("libvirt opened")
		return nil, nil, errors.New("no")
	}).WithRemote(func(host, tok string) (app.Manager, func(), error) {
		t.Error("remote client built for a tunnel")
		return nil, nil, errors.New("no")
	})
	h.hosts = res
	srv := handlerFor(h)
	r := httptest.NewRequest(http.MethodGet, "/v1/hosts/veritech01/vms/web/tunnel", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "vmtool-tunnel")
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := "tunnels to veritech01 are served by that host's daemon, not by ws; use `vmtool tunnel veritech01/web`"
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error != want {
		t.Fatalf("body %s (%v)\nwant error %q", rec.Body, err, want)
	}
}
