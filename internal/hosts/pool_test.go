package hosts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// poolFake records the pool every pool-taking app.Manager method is given.
// Anything else panics (the embedded interface is nil).
type poolFake struct {
	app.Manager
	calls []string
}

func (f *poolFake) rec(format string, a ...any) { f.calls = append(f.calls, fmt.Sprintf(format, a...)) }

func (f *poolFake) CloneImage(base, name, pool string, _ uint64) (string, error) {
	f.rec("CloneImage %s", pool)
	return "", nil
}
func (f *poolFake) CreateVolume(pool, name string, gb uint) (string, error) {
	f.rec("CreateVolume %s", pool)
	return "", nil
}
func (f *poolFake) AddDisk(name string, gb uint, pool string) (*vmtool.DiskDevice, error) {
	f.rec("AddDisk %s", pool)
	return nil, nil
}
func (f *poolFake) DeleteImage(name, pool string) error { f.rec("DeleteImage %s", pool); return nil }
func (f *poolFake) MigrateDisk(name, pool string) error { f.rec("MigrateDisk %s", pool); return nil }
func (f *poolFake) Define(c vmtool.VMConfig) error {
	f.rec("Define %s %s", c.Pool, c.ExtraDiskPool)
	return nil
}
func (f *poolFake) RefreshPool(pool string) error { f.rec("RefreshPool %s", pool); return nil }

func exercise(t *testing.T, m app.Manager, pool string) {
	t.Helper()
	if _, err := m.CloneImage("/b", "web", pool, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateVolume(pool, "v", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDisk("web", 1, pool); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteImage("i", pool); err != nil {
		t.Fatal(err)
	}
	if err := m.MigrateDisk("web", pool); err != nil {
		t.Fatal(err)
	}
	if err := m.Define(vmtool.VMConfig{Name: "web", Pool: pool}); err != nil {
		t.Fatal(err)
	}
	// ExtraDiskPool "" means "same as Pool" and is left alone (Define above).
	// The image cache refreshes its pool through an optional interface; the
	// resolved Manager must still offer it.
	r, ok := m.(interface{ RefreshPool(string) error })
	if !ok {
		t.Fatal("resolved Manager lost RefreshPool")
	}
	if err := r.RefreshPool(pool); err != nil {
		t.Fatal(err)
	}
}

// An empty pool means "this host's default": on a hypervisor the resolver's
// local Manager substitutes the vmtool pool; a named pool passes through; on
// a workstation the empty pool reaches the libvirt Manager unchanged (which
// maps it to "default" itself).
func TestLocalManagerHostDefaultPool(t *testing.T) {
	for _, tc := range []struct {
		role, pool, want string
	}{
		{config.RoleHypervisor, "", vmtool.HypervisorPool},
		{config.RoleHypervisor, "tp", "tp"},
		{config.RoleWorkstation, "", ""},
		{config.RoleWorkstation, "tp", "tp"},
	} {
		t.Run(tc.role+"/"+tc.pool, func(t *testing.T) {
			f := &poolFake{}
			r := NewWith("box", tc.role, nil, func() (app.Manager, func(), error) { return f, func() {}, nil })
			m, done, err := r.Resolve("box")
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			exercise(t, m, tc.pool)
			w := tc.want
			want := []string{
				"CloneImage " + w, "CreateVolume " + w, "AddDisk " + w, "DeleteImage " + w,
				"MigrateDisk " + w, "Define " + w + " ", "RefreshPool " + w,
			}
			if got := strings.Join(f.calls, "|"); got != strings.Join(want, "|") {
				t.Errorf("fake saw\n%q\nwant\n%q", f.calls, want)
			}
		})
	}
}

// The hypervisor pool name is "vmtool"; the scripts that create it agree.
func TestHypervisorPoolName(t *testing.T) {
	if vmtool.HypervisorPool != "vmtool" {
		t.Fatalf("HypervisorPool = %q", vmtool.HypervisorPool)
	}
}

func fakeResolver(f *poolFake, role string) *Resolver {
	return NewWith("box", role, []string{"far", "plain"}, func() (app.Manager, func(), error) { return f, func() {}, nil })
}

func resolveOne(t *testing.T, r *Resolver, host string) app.Manager {
	t.Helper()
	m, done, err := r.WithToken("x").Resolve(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(done)
	return m
}

// default_pool applies to the four VM-disk operations only, an empty pool
// only, and wins over the hypervisor's vmtool; RefreshPool, DeleteImage and
// MigrateDisk keep today's behaviour.
func TestDefaultPoolLocal(t *testing.T) {
	for _, tc := range []struct {
		role, pool string
		want       []string
	}{
		{config.RoleWorkstation, "", []string{"CloneImage nvme", "CreateVolume nvme", "AddDisk nvme", "DeleteImage ", "MigrateDisk ", "Define nvme ", "RefreshPool "}},
		{config.RoleWorkstation, "tp", []string{"CloneImage tp", "CreateVolume tp", "AddDisk tp", "DeleteImage tp", "MigrateDisk tp", "Define tp ", "RefreshPool tp"}},
		{config.RoleHypervisor, "", []string{"CloneImage nvme", "CreateVolume nvme", "AddDisk nvme", "DeleteImage vmtool", "MigrateDisk vmtool", "Define nvme ", "RefreshPool vmtool"}},
		{config.RoleHypervisor, "tp", []string{"CloneImage tp", "CreateVolume tp", "AddDisk tp", "DeleteImage tp", "MigrateDisk tp", "Define tp ", "RefreshPool tp"}},
	} {
		t.Run(tc.role+"/"+tc.pool, func(t *testing.T) {
			f := &poolFake{}
			r := fakeResolver(f, tc.role).WithDefaultPools("nvme", nil)
			exercise(t, resolveOne(t, r, "box"), tc.pool)
			if got, want := strings.Join(f.calls, "|"), strings.Join(tc.want, "|"); got != want {
				t.Errorf("fake saw\n%q\nwant\n%q", f.calls, tc.want)
			}
		})
	}
}

// Define leaves ExtraDiskPool alone: empty still means "same as Pool", which
// is now the default pool.
func TestDefaultPoolExtraDiskPool(t *testing.T) {
	f := &poolFake{}
	m := resolveOne(t, fakeResolver(f, config.RoleWorkstation).WithDefaultPools("nvme", nil), "box")
	if err := m.Define(vmtool.VMConfig{Name: "w", ExtraDiskPool: "slow"}); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[0]; got != "Define nvme slow" {
		t.Fatalf("got %q", got)
	}
}

// A remote host's default_pool wraps only that host's Manager.
func TestDefaultPoolRemote(t *testing.T) {
	far, plain := &poolFake{}, &poolFake{}
	r := fakeResolver(&poolFake{}, config.RoleWorkstation).
		WithDefaultPools("", map[string]string{"far": "nvme"}).
		WithRemote(func(host, token string) (app.Manager, func(), error) {
			if host == "far" {
				return far, func() {}, nil
			}
			return plain, func() {}, nil
		})
	exercise(t, resolveOne(t, r, "far"), "")
	exercise(t, resolveOne(t, r, "plain"), "")
	if got, want := strings.Join(far.calls, "|"), "CloneImage nvme|CreateVolume nvme|AddDisk nvme|DeleteImage |MigrateDisk |Define nvme |RefreshPool "; got != want {
		t.Errorf("far saw %q", far.calls)
	}
	if got, want := strings.Join(plain.calls, "|"), "CloneImage |CreateVolume |AddDisk |DeleteImage |MigrateDisk |Define  |RefreshPool "; got != want {
		t.Errorf("plain saw %q", plain.calls)
	}
}

// New copies default_pool from the config: top level for the local host,
// hosts.<name> for a remote.
func TestNewAppliesDefaultPools(t *testing.T) {
	cfg := &config.Config{Name: "box", Role: config.RoleWorkstation, DefaultPool: "nvme",
		Hosts: map[string]config.Host{"far": {Address: "a", DefaultPool: "fast"}}}
	r := New(cfg)
	if r.defaultPool != "nvme" || r.hostPools["far"] != "fast" {
		t.Fatalf("resolver pools: %q %v", r.defaultPool, r.hostPools)
	}
}

// refreshFake is a workstation libvirt Manager that has RefreshPools (C8).
type refreshFake struct {
	poolFake
	refreshed int
}

func (f *refreshFake) RefreshPools() error { f.refreshed++; return nil }

// The default_pool wrapper keeps the workstation's pool refresh (C8) that
// app.FindLocalImage looks for, so an image miss still rescans the pools.
func TestDefaultPoolKeepsRefreshPools(t *testing.T) {
	f := &refreshFake{}
	var m app.Manager = defaultPools{f, "nvme"}
	r, ok := m.(interface{ RefreshPools() error })
	if !ok {
		t.Fatal("defaultPools hides RefreshPools")
	}
	if err := r.RefreshPools(); err != nil || f.refreshed != 1 {
		t.Fatalf("RefreshPools: err %v, refreshed %d", err, f.refreshed)
	}
}
