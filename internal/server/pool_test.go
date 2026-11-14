package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// A workstation CreateVM on a remote hypervisor that names no pool makes the
// daemon clone the base image into its vmtool pool and put the extra disk
// there too; a named pool sends both to that pool. The workstation never
// fills in a pool: the daemon's host resolver does.
func TestCreateVMRemoteHostDefaultPool(t *testing.T) {
	for _, tc := range []struct{ pool, want string }{
		{"", vmtool.HypervisorPool},
		{"tp", "tp"},
	} {
		t.Run("pool="+tc.pool, func(t *testing.T) {
			t.Chdir(t.TempDir())
			fm := newRTMgr(t)
			seedCached(t, fm.pool, "ubuntu.qcow2")
			srv, cas, _ := hypervisor(t, fm, testToken)
			m := remoteFor(t, srv, cas, testToken)
			svc := &app.Service{IPTimeout: time.Second}
			if _, err := svc.CreateVM(context.Background(), m, app.CreateVMInput{
				Host: "veritech01", Name: "web", Image: "ubuntu.qcow2",
				Pool: tc.pool, ExtraDiskSizeGB: 5,
			}); err != nil {
				t.Fatalf("CreateVM: %v", err)
			}
			got := strings.Join(fm.got(), "\n")
			for _, want := range []string{
				"CloneImage " + filepath.Join(fm.pool, "ubuntu.qcow2") + " web " + tc.want,
				"CreateVolume " + tc.want + " web-2.qcow2 5",
			} {
				if !strings.Contains(got, want+"\n") {
					t.Errorf("hypervisor calls\n%s\nlack %q", got, want)
				}
			}
		})
	}
}

// The REST deleteImage with no ?pool= is the host's default pool: the
// workstation passes the empty pool to its own libvirt (which means
// "default" there); a hypervisor uses its vmtool pool; a named pool is used
// as it is.
func TestDeleteImageHostDefaultPool(t *testing.T) {
	for _, tc := range []struct{ role, query, want string }{
		{config.RoleWorkstation, "", "DeleteImage x.qcow2 "},
		{config.RoleWorkstation, "?pool=tp", "DeleteImage x.qcow2 tp"},
		{config.RoleHypervisor, "", "DeleteImage x.qcow2 vmtool"},
		{config.RoleHypervisor, "?pool=tp", "DeleteImage x.qcow2 tp"},
	} {
		t.Run(tc.role+tc.query, func(t *testing.T) {
			t.Chdir(t.TempDir())
			fm := newRTMgr(t)
			res := hosts.NewWith("box", tc.role, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
			h := &handlers{hosts: res, role: tc.role, imageManifest: filepath.Join(t.TempDir(), vmtool.ImageManifestFile)}
			rec := do(handlerFor(h), http.MethodDelete, "/v1/hosts/box/images/x.qcow2"+tc.query, "")
			if rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
			}
			if got := strings.Join(fm.got(), ","); got != tc.want {
				t.Errorf("Manager saw %q, want %q", got, tc.want)
			}
		})
	}
}

// noCachePool is a hypervisor whose libvirt has no vmtool pool (only the
// libvirt default one).
type noCachePool struct{ *poolMgr }

func (n noCachePool) ListPools() ([]vmtool.PoolInfo, error) {
	return []vmtool.PoolInfo{{Name: "default", Path: n.dirs["other"], Active: true}}, nil
}

// Without a vmtool pool a hypervisor cannot cache images, and says how to
// make one; nothing is written to any other pool.
func TestImageCacheNeedsVmtoolPool(t *testing.T) {
	f := newImgFixture(t, config.RoleHypervisor)
	nc := noCachePool{f.pm}
	f.h.hosts = hosts.NewWith("hv1", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return nc, func() {}, nil })
	srv := handlerFor(f.h)
	f.srv = srv

	data := []byte("image bytes")
	rec := f.put("u.qcow2", strings.NewReader(string(data)), int64(len(data)), hexSum(data))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	msg := errText(t, rec)
	for _, want := range []string{`no storage pool "vmtool"`, "just prereqs hv1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	rec = do(srv, http.MethodGet, "/v1/hosts/hv1/images/u.qcow2/manifest", "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(errText(t, rec), `no storage pool "vmtool"`) {
		t.Errorf("GET manifest: %d %s", rec.Code, rec.Body)
	}
	f.noPart(t, "u.qcow2")
}
