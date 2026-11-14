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

func (f *poolFake) CloneImage(base, name, pool string) (string, error) {
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
	if _, err := m.CloneImage("/b", "web", pool); err != nil {
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
