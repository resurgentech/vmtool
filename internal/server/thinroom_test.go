package server

import (
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const roomMsg = "thin pool thin-test (vg-test) has no room: data use is 80.00%, at or above the 80% limit"

// A full thin pool is a 409 with the pool's numbers on every route that checks
// room: addDisk and resizeDisk (T6a) and the createVM clone path. The Manager
// returns the bare ThinRoomError and, as the hosts layer does, the same error
// already classified as a conflict; both are 409.
func TestThinNoRoomIs409OnDiskAndCreateRoutes(t *testing.T) {
	routes := []struct {
		name, path, body, call string
	}{
		{"addDisk", "/v1/hosts/box/vms/vm1/add-disk", `{"size":50,"pool":"thin-test"}`, "AddDisk"},
		{"resizeDisk", "/v1/hosts/box/vms/vm1/resize-disk", `{"size":50}`, "ResizeDisk"},
	}
	for _, rt := range routes {
		for name, err := range map[string]error{
			"bare":       &vmtool.ThinRoomError{Msg: roomMsg},
			"classified": &app.Error{Kind: app.KindConflict, Op: "x", Err: &vmtool.ThinRoomError{Msg: roomMsg}},
		} {
			fm := &fakeMgr{err: err}
			h := primServer(t, fm, config.RoleWorkstation)
			rec := do(h, "POST", rt.path, rt.body)
			if rec.Code != 409 || !strings.Contains(rec.Body.String(), "80.00%") || !strings.Contains(rec.Body.String(), "thin-test") {
				t.Errorf("%s/%s: %d %s, want 409 with the numbers", rt.name, name, rec.Code, rec.Body)
			}
		}
	}
}

// The createVM clone path: a clone refused for room reaches the caller as 409.
func TestCreateVMCloneNoRoomIs409(t *testing.T) {
	fm := &fakeMgr{imagePath: "/var/lib/vmtool/u.qcow2"}
	refusal := &app.Error{Kind: app.KindConflict, Op: "clone", Err: &vmtool.ThinRoomError{Msg: roomMsg}}
	h := createServer(t, cloneFailMgr{fm, refusal}, &app.Service{})
	rec := do(h, "POST", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2","pool":"thin-test"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "80.00%") {
		t.Fatalf("%d %s, want 409 with the numbers", rec.Code, rec.Body)
	}
	for _, c := range fm.calls {
		if strings.HasPrefix(c, "Define") {
			t.Errorf("a VM was defined after the refusal: %q", fm.calls)
		}
	}
}

// cloneFailMgr fails only the clone, so the workflow reaches it.
type cloneFailMgr struct {
	*fakeMgr
	err error
}

func (f cloneFailMgr) CloneImage(base, n, pool string, size uint64) (string, error) {
	f.rec("CloneImage %s %s %s", base, n, pool)
	return "", f.err
}

// The daemon primitive resizeVMVolume is what a workstation's resize-disk
// calls on a host: a full thin pool is 409 with the numbers there too.
func TestResizeVMVolumeNoRoomIs409(t *testing.T) {
	for name, err := range map[string]error{
		"bare":       &vmtool.ThinRoomError{Msg: roomMsg},
		"classified": &app.Error{Kind: app.KindConflict, Op: "x", Err: &vmtool.ThinRoomError{Msg: roomMsg}},
	} {
		h := primServer(t, &fakeMgr{err: err}, config.RoleHypervisor)
		rec := do(h, "POST", "/v1/hosts/box/vms/vm1/resize-volume", `{"size_gb":50}`)
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "80.00%") || !strings.Contains(rec.Body.String(), "thin-test") {
			t.Errorf("%s: %d %s, want 409 with the numbers", name, rec.Code, rec.Body)
		}
	}
}
