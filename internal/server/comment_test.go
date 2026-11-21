package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// PUT .../comment sets the comment with one metadata call and answers the VM;
// it never redefines. Empty clears. On a hypervisor and a workstation alike.
func TestSetVMCommentRoute(t *testing.T) {
	for _, role := range []string{config.RoleHypervisor, config.RoleWorkstation} {
		fm := &fakeMgr{}
		h := primServer(t, fm, role)
		rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/comment", `{"comment":"placeholder comment"}`)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", role, rec.Code, rec.Body)
		}
		if want := []string{"SetComment vm1 placeholder comment", "Info vm1"}; !reflect.DeepEqual(fm.calls, want) {
			t.Errorf("%s: calls %q, want %q", role, fm.calls, want)
		}
		fm.calls = nil
		if rec = do(h, "PUT", "/v1/hosts/box/vms/vm1/comment", `{"comment":""}`); rec.Code != 200 {
			t.Fatalf("clear: %d %s", rec.Code, rec.Body)
		}
		if want := []string{"SetComment vm1 ", "Info vm1"}; !reflect.DeepEqual(fm.calls, want) {
			t.Errorf("clear calls %q", fm.calls)
		}
	}
}

func TestSetVMCommentRefusals(t *testing.T) {
	long := strings.Repeat("a", 513)
	for name, body := range map[string]string{
		"too long": `{"comment":"` + long + `"}`,
		"newline":  `{"comment":"a\nb"}`,
		"control":  `{"comment":"a\u0001b"}`,
	} {
		fm := &fakeMgr{}
		h := primServer(t, fm, config.RoleHypervisor)
		rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/comment", body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "comment") {
			t.Errorf("%s: %d %s, want 400 naming comment", name, rec.Code, rec.Body)
		}
		if len(fm.calls) != 0 {
			t.Errorf("%s: libvirt was called: %q", name, fm.calls)
		}
	}
	fm := &fakeMgr{}
	h := primServer(t, fm, config.RoleHypervisor)
	if rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/comment", ""); rec.Code != 400 {
		t.Errorf("no body: %d", rec.Code)
	}
	fm = &fakeMgr{err: fmt.Errorf("looking up domain: %w", vmtool.ErrNotFound)}
	h = primServer(t, fm, config.RoleHypervisor)
	if rec := do(h, "PUT", "/v1/hosts/box/vms/ghost/comment", `{"comment":"x"}`); rec.Code != 404 {
		t.Errorf("unknown VM: %d", rec.Code)
	}
}

// defineVM takes the origin and comment into the config; an older client that
// sends neither still defines (no origin, no comment); bad ones are 400.
func TestDefineVMOriginAndComment(t *testing.T) {
	fm, pool := poolFixture(t)
	h := primServer(t, fm, config.RoleHypervisor)
	disk := filepath.Join(pool, "vm1.qcow2")
	def := func(extra string) string {
		return `{"vcpus":1,"memory_gib":1,"disk_path":"` + disk + `","network":{"type":"nat"}` + extra + `}`
	}
	sha := strings.Repeat("a", 64)
	rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/definition", def(`,"comment":"placeholder comment","origin":{"image":"img1","image_sha256":"`+sha+`","template":"img1","template_lv":"vmtpl-aaaaaaaaaaaa","clone_kind":"thin","pool":"nvme-thin","created":"2026-11-21T04:05:06Z"}`))
	if rec.Code != 204 {
		t.Fatalf("define: %d %s", rec.Code, rec.Body)
	}
	want := &vmtool.VMOrigin{Image: "img1", ImageSHA256: sha, Template: "img1", TemplateLV: "vmtpl-aaaaaaaaaaaa",
		CloneKind: vmtool.CloneKindThin, Pool: "nvme-thin", Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}
	if !reflect.DeepEqual(fm.defined.Origin, want) || fm.defined.Comment != "placeholder comment" {
		t.Errorf("Define got origin %+v comment %q", fm.defined.Origin, fm.defined.Comment)
	}

	fm.defined = vmtool.VMConfig{}
	if rec = do(h, "PUT", "/v1/hosts/box/vms/vm1/definition", def("")); rec.Code != 204 {
		t.Fatalf("older client define: %d %s", rec.Code, rec.Body)
	}
	if fm.defined.Origin != nil || fm.defined.Comment != "" {
		t.Errorf("no field sent but Define got origin %+v comment %q", fm.defined.Origin, fm.defined.Comment)
	}

	for field, extra := range map[string]string{
		"comment":           `,"comment":"a\nb"`,
		"origin.clone_kind": `,"origin":{"clone_kind":"copy"}`,
		"origin.image":      `,"origin":{"clone_kind":"file","image":"a\u0001b"}`,
	} {
		fm.calls = nil
		rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/definition", def(extra))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), field) {
			t.Errorf("%s: %d %s, want 400 naming %s", field, rec.Code, rec.Body, field)
		}
		for _, c := range fm.calls {
			if strings.HasPrefix(c, "Define") {
				t.Errorf("%s: Define ran", field)
			}
		}
	}
}

// getVM and listVMs carry origin and comment; a VM with none omits both.
func TestVMInfoJSONOriginComment(t *testing.T) {
	with := toAPI(&vmtool.VMInfo{Name: "vm1", Comment: "placeholder comment", Origin: &vmtool.VMOrigin{
		Image: "img1", CloneKind: vmtool.CloneKindFile, Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}})
	b, _ := json.Marshal(with)
	for _, want := range []string{`"comment":"placeholder comment"`, `"origin":{`, `"clone_kind":"file"`, `"created":"2026-11-21T04:05:06Z"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
	b, _ = json.Marshal(toAPI(&vmtool.VMInfo{Name: "vm1"}))
	if strings.Contains(string(b), "origin") || strings.Contains(string(b), "comment") {
		t.Errorf("a VM with neither sent them: %s", b)
	}
}
