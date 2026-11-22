package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// An LV is /dev/<vg>/<lv>, a symlink to /dev/dm-N: for a pool of type logical
// the unresolved path is checked, and only as <pool path>/<one plain name>.
// Temp-dir stand-ins play /dev/vg-test; a symlink plays the LV.
func TestInPoolLogical(t *testing.T) {
	root := t.TempDir()
	vg := filepath.Join(root, "vg-test")
	otherVG := filepath.Join(root, "vg-other")
	dm := filepath.Join(root, "dm")
	dirPool := filepath.Join(root, "dirpool")
	for _, d := range []string{vg, otherVG, dm, dirPool} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"dm-1", "dm-2"} {
		if err := os.WriteFile(filepath.Join(dm, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// LVs: symlinks pointing outside the VG directory, as /dev/dm-N does.
	if err := os.Symlink(filepath.Join(dm, "dm-1"), filepath.Join(vg, "vm1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dm, "dm-2"), filepath.Join(otherVG, "vm2")); err != nil {
		t.Fatal(err)
	}
	// A dir pool with a symlink escaping to the VG's target.
	if err := os.Symlink(filepath.Join(dm, "dm-1"), filepath.Join(dirPool, "esc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(vg, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vg, "sub", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	m := &fakeMgr{pools: []vmtool.PoolInfo{
		{Name: "nvme", Path: vg, Active: true, Type: "logical"},
		{Name: "vmtool", Path: dirPool, Active: true, Type: "dir"},
	}}
	for _, tc := range []struct {
		name, p string
		ok      bool
	}{
		{"LV symlink in the logical pool", filepath.Join(vg, "vm1"), true},
		{"unclean but the same LV", vg + "/./vm1", true},
		{"LV that does not exist", filepath.Join(vg, "nope"), false},
		{"dotdot to another VG", vg + "/../vg-other/vm2", false},
		{"dotdot above", vg + "/../dm/dm-1", false},
		{"two components", filepath.Join(vg, "sub", "x"), false},
		{"a different VG", filepath.Join(otherVG, "vm2"), false},
		{"the pool itself", vg, false},
		{"dir pool symlink escape", filepath.Join(dirPool, "esc"), false},
	} {
		err := inPool(m, tc.p)
		if tc.ok && err != nil {
			t.Errorf("%s: %q rejected: %v", tc.name, tc.p, err)
		}
		if !tc.ok && !errors.Is(err, errOutsidePool) {
			t.Errorf("%s: %q: err = %v, want errOutsidePool", tc.name, tc.p, err)
		}
	}
}

// poolPath hands libvirt the cleaned LV path, not the resolved /dev/dm-N.
func TestPoolPathLogicalKeepsLVPath(t *testing.T) {
	root := t.TempDir()
	vg := filepath.Join(root, "vg-test")
	if err := os.Mkdir(vg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dm-1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "dm-1"), filepath.Join(vg, "vm1")); err != nil {
		t.Fatal(err)
	}
	m := &fakeMgr{pools: []vmtool.PoolInfo{{Name: "nvme", Path: vg, Type: "logical"}}}
	got, err := poolPath(m, vg+"/./vm1")
	if err != nil || got != filepath.Join(vg, "vm1") {
		t.Errorf("poolPath = %q, %v", got, err)
	}
}

// cloneVolume passes size_bytes to the Manager; absent is 0.
func TestCloneVolumeSizeBytesReachesManager(t *testing.T) {
	fm, pool := poolFixture(t)
	h := primServer(t, fm, "hypervisor")
	base := filepath.Join(pool, "base.qcow2")
	fm.volPath = filepath.Join(pool, "web.qcow2")
	for _, tc := range []struct {
		extra string
		want  uint64
	}{{``, 0}, {`,"size_bytes":21474836480`, 21474836480}} {
		fm.cloneSize = 99
		rec := do(h, "POST", "/v1/hosts/box/volumes/clone", `{"base_path":"`+base+`","new_name":"web"`+tc.extra+`}`)
		if rec.Code != 201 {
			t.Fatalf("clone: %d %s", rec.Code, rec.Body)
		}
		if fm.cloneSize != tc.want {
			t.Errorf("body %q: Manager got size %d, want %d", tc.extra, fm.cloneSize, tc.want)
		}
	}
}
