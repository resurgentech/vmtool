package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// defineVM carries disk_cache as given; omitted means no attribute (the
// daemon does not guess; the workstation's app resolved the default).
func TestDefineVMDiskCache(t *testing.T) {
	for _, tc := range []struct {
		name, field, want string
		code              int
	}{
		{"omitted is empty", ``, "", 204},
		{"unsafe", `,"disk_cache":"unsafe"`, "unsafe", 204},
		{"none", `,"disk_cache":"none"`, "none", 204},
		{"invalid", `,"disk_cache":"directsync"`, "", 400},
	} {
		fm, pool := poolFixture(t)
		h := primServer(t, fm, config.RoleHypervisor)
		body := `{"vcpus":1,"memory_gib":1,"disk_path":"` + filepath.Join(pool, "a.qcow2") + `","network":{"type":"nat"}` + tc.field + `}`
		rec := do(h, "PUT", "/v1/hosts/box/vms/web/definition", body)
		if rec.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
		if tc.code == 204 && fm.defined.DiskCache != tc.want {
			t.Errorf("%s: Define got %q, want %q", tc.name, fm.defined.DiskCache, tc.want)
		}
		if tc.code == 400 && strings.Contains(strings.Join(fm.calls, ","), "Define") {
			t.Errorf("%s: invalid disk_cache reached Define", tc.name)
		}
	}
}

func TestToAPIDiskCache(t *testing.T) {
	if v := toAPI(&vmtool.VMInfo{Name: "a", DiskCache: "unsafe"}); v.DiskCache == nil || string(*v.DiskCache) != "unsafe" {
		t.Errorf("unsafe: %+v", v.DiskCache)
	}
	if v := toAPI(&vmtool.VMInfo{Name: "a"}); v.DiskCache != nil {
		t.Errorf("unset disk cache must be omitted, got %q", *v.DiskCache)
	}
}
