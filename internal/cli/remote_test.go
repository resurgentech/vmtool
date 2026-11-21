package cli

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// errFakeHost is what every fake Manager operation that is not a listing
// returns, so a command stops at its first Manager call.
var errFakeHost = errors.New("fake host refused the operation")

// cliFake is a Manager standing in for one host: listings answer, every other
// operation records itself and fails with errFakeHost.
type cliFake struct {
	host string
	f    *fakeHosts
}

func (c cliFake) rec(op string) error {
	c.f.mu.Lock()
	c.f.ops = append(c.f.ops, c.host+" "+op)
	c.f.mu.Unlock()
	return errFakeHost
}

func (c cliFake) Start(string) error                  { return c.rec("Start") }
func (c cliFake) Stop(string) error                   { return c.rec("Stop") }
func (c cliFake) Resume(string) error                 { return c.rec("Resume") }
func (c cliFake) Destroy(string) error                { return c.rec("Destroy") }
func (c cliFake) Delete(string, bool) error           { return c.rec("Delete") }
func (c cliFake) Info(string) (*vmtool.VMInfo, error) { return nil, c.rec("Info") }
func (c cliFake) List() ([]vmtool.VMInfo, error)      { return nil, c.rec("List") }
func (c cliFake) WaitForIP(string, time.Duration) (string, error) {
	return "", c.rec("WaitForIP")
}
func (c cliFake) SetAutostart(string, bool) error  { return c.rec("SetAutostart") }
func (c cliFake) SetComment(string, string) error  { return c.rec("SetComment") }
func (c cliFake) Reboot(string) error              { return c.rec("Reboot") }
func (c cliFake) ResizeDisk(string, uint) error    { return c.rec("ResizeDisk") }
func (c cliFake) MigrateDisk(string, string) error { return c.rec("MigrateDisk") }
func (c cliFake) CreateVolume(string, string, uint) (string, error) {
	return "", c.rec("CreateVolume")
}
func (c cliFake) AddDisk(string, uint, string) (*vmtool.DiskDevice, error) {
	return nil, c.rec("AddDisk")
}
func (c cliFake) ImagePath(string) (string, error) { return "", c.rec("ImagePath") }
func (c cliFake) ListNetworks() ([]string, error) {
	c.rec("ListNetworks")
	return []string{c.host + "-net"}, nil
}
func (c cliFake) ListBridges() ([]string, error) {
	c.rec("ListBridges")
	return []string{c.host + "-br"}, nil
}
func (c cliFake) ListImagesByPool() (map[string][]string, error) {
	c.rec("ListImagesByPool")
	return map[string][]string{"default": {c.host + ".qcow2"}}, nil
}
func (c cliFake) DeleteImage(name, pool string) error {
	c.rec("DeleteImage " + name + " " + pool)
	return nil
}
func (c cliFake) ListPools() ([]vmtool.PoolInfo, error) {
	c.rec("ListPools")
	return []vmtool.PoolInfo{{Name: c.host + "-pool", Path: "/p", Active: true}}, nil
}
func (c cliFake) CreatePool(name, path string) error {
	c.rec("CreatePool " + name + " " + path)
	return nil
}
func (c cliFake) CloneImage(string, string, string, uint64) (string, error) {
	return "", c.rec("CloneImage")
}
func (c cliFake) ResizeVolume(string, uint64) error { return c.rec("ResizeVolume") }
func (c cliFake) Define(vmtool.VMConfig) error      { return c.rec("Define") }

var _ app.Manager = cliFake{}

// fakeHosts replaces the CLI's resolver for one test: the local host
// "jetfire" and the configured "veritech01" are cliFakes, so no test opens
// libvirt or reaches a network address. It records the token each remote
// client was built with.
type fakeHosts struct {
	mu     sync.Mutex
	ops    []string
	tokens []string
}

func installFakeHosts(t *testing.T) *fakeHosts {
	t.Helper()
	f := &fakeHosts{}
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		var configured []string
		for n := range e.cfg.Hosts {
			configured = append(configured, n)
		}
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, configured, func() (app.Manager, func(), error) {
			return cliFake{host: e.cfg.Name, f: f}, func() {}, nil
		}).WithRemote(func(host, token string) (app.Manager, func(), error) {
			f.mu.Lock()
			f.tokens = append(f.tokens, host+" "+token)
			f.mu.Unlock()
			return cliFake{host: host, f: f}, func() {}, nil
		})
	}
	return f
}

func (f *fakeHosts) seen() (ops, tokens []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...), append([]string(nil), f.tokens...)
}

// Every VM command acts on a configured host through its remote client,
// built with the CLI's verified token; an unconfigured host is still unknown.
func TestVMCommandsReachRemoteHost(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	for name, rest := range vmCommands {
		f := installFakeHosts(t)
		keep := map[string]bool{"vmtool " + name: true}
		r := runGate(t, dir, keep, append([]string{name, "veritech01/web"}, rest...)...)
		if r.err == nil || errors.Is(r.err, hosts.ErrUnknownHost) {
			t.Errorf("%s veritech01/web: %v, want the fake host's error", name, r.err)
		}
		ops, tokens := f.seen()
		if len(tokens) != 1 || tokens[0] != "veritech01 "+gateToken {
			t.Errorf("%s: remote clients %q, want one for veritech01 with the CLI token", name, tokens)
		}
		for _, op := range ops {
			if !strings.HasPrefix(op, "veritech01 ") {
				t.Errorf("%s: op %q on the wrong host", name, op)
			}
		}

		r = runGate(t, dir, keep, append([]string{name, "nosuch/web"}, rest...)...)
		if !errors.Is(r.err, hosts.ErrUnknownHost) {
			t.Errorf("%s nosuch/web: %v, want unknown host", name, r.err)
		}
	}
}

// networks, bridges, images and pool take --host (default: the local host).
func TestHostFlag(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	for _, tc := range []struct {
		args []string
		op   string
	}{
		{[]string{"networks"}, "ListNetworks"},
		{[]string{"bridges"}, "ListBridges"},
		{[]string{"images", "list"}, "ListImagesByPool"},
		{[]string{"images", "delete", "x.qcow2", "--pool", "fast"}, "DeleteImage x.qcow2 fast"},
		{[]string{"pool", "list"}, "ListPools"},
		{[]string{"pool", "create", "fast", "/srv/fast"}, "CreatePool fast /srv/fast"},
	} {
		path := "vmtool " + strings.Join(tc.args[:min(2, len(tc.args))], " ")
		if tc.args[0] == "networks" || tc.args[0] == "bridges" {
			path = "vmtool " + tc.args[0]
		}
		keep := map[string]bool{path: true}
		for _, host := range []string{"", "jetfire", "veritech01"} {
			f := installFakeHosts(t)
			args := tc.args
			if host != "" {
				args = append(append([]string(nil), args...), "--host", host)
			}
			r := runGate(t, dir, keep, args...)
			if r.err != nil {
				t.Errorf("%v: %v", args, r.err)
				continue
			}
			want := host
			if want == "" {
				want = "jetfire"
			}
			ops, tokens := f.seen()
			if len(ops) != 1 || ops[0] != want+" "+tc.op {
				t.Errorf("%v: ops %q, want [%s %s]", args, ops, want, tc.op)
			}
			if want == "veritech01" && (len(tokens) != 1 || tokens[0] != "veritech01 "+gateToken) {
				t.Errorf("%v: tokens %q", args, tokens)
			}
			if want == "jetfire" && len(tokens) != 0 {
				t.Errorf("%v: remote client built for the local host: %q", args, tokens)
			}
		}
		f := installFakeHosts(t)
		r := runGate(t, dir, keep, append(append([]string(nil), tc.args...), "--host", "nosuch")...)
		if !errors.Is(r.err, hosts.ErrUnknownHost) {
			t.Errorf("%v --host nosuch: %v", tc.args, r.err)
		}
		if ops, _ := f.seen(); len(ops) != 0 {
			t.Errorf("%v --host nosuch ran %v", tc.args, ops)
		}
	}
}

func TestHostFlagUsage(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	for _, p := range [][]string{{"networks"}, {"bridges"}, {"images", "list"}, {"images", "delete"}, {"pool", "list"}, {"pool", "create"}} {
		c, _, err := root.Find(p)
		if err != nil {
			t.Fatal(err)
		}
		fl := c.Flags().Lookup("host")
		if fl == nil || fl.DefValue != "" {
			t.Errorf("%v: --host flag %v", p, fl)
		}
	}
}
