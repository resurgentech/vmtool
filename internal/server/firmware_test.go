package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// defineVM carries firmware explicitly; a body that omits it (an older
// workstation) is defined with efi, the default.
func TestDefineVMFirmware(t *testing.T) {
	for _, tc := range []struct {
		name, field, want string
		code              int
	}{
		{"omitted is efi", ``, vmtool.FirmwareEFI, 204},
		{"efi", `,"firmware":"efi"`, vmtool.FirmwareEFI, 204},
		{"bios", `,"firmware":"bios"`, vmtool.FirmwareBIOS, 204},
		{"invalid", `,"firmware":"coreboot"`, "", 400},
	} {
		fm, pool := poolFixture(t)
		h := primServer(t, fm, config.RoleHypervisor)
		body := `{"vcpus":1,"memory_gib":1,"disk_path":"` + filepath.Join(pool, "a.qcow2") + `","network":{"type":"nat"}` + tc.field + `}`
		rec := do(h, "PUT", "/v1/hosts/box/vms/web/definition", body)
		if rec.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
		if tc.code == 204 && fm.defined.Firmware != tc.want {
			t.Errorf("%s: Define got firmware %q, want %q", tc.name, fm.defined.Firmware, tc.want)
		}
		if tc.code == 400 && strings.Contains(strings.Join(fm.calls, ","), "Define") {
			t.Errorf("%s: invalid firmware reached Define", tc.name)
		}
	}
}

func TestToAPIFirmware(t *testing.T) {
	if v := toAPI(&vmtool.VMInfo{Name: "a", Firmware: "efi"}); v.Firmware == nil || string(*v.Firmware) != "efi" {
		t.Errorf("efi: %+v", v.Firmware)
	}
	if v := toAPI(&vmtool.VMInfo{Name: "a"}); v.Firmware != nil {
		t.Errorf("unset firmware must be omitted, got %q", *v.Firmware)
	}
}
