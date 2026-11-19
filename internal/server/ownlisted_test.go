package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const ownToken = "PLACEHOLDER-not-a-secret-1"

// ownImgMgr is a daemon's manager that answers image manifests; any other
// method panics (the embedded interface is nil).
type ownImgMgr struct {
	app.Manager
	asked *atomic.Int32
}

func (m ownImgMgr) ImageManifest(name string) (app.RemoteImage, error) {
	m.asked.Add(1)
	return app.RemoteImage{Name: name, SHA256: "abc", Size: 3}, nil
}

func (m ownImgMgr) UploadImage(context.Context, string, io.Reader, int64, string, func(int64)) error {
	return errors.New("unused")
}

// A workstation whose resolver lists the own name: the local connector fails
// the test, every operation on the own name goes to the daemon.
func ownListedWorkstation(t *testing.T, opened *atomic.Int32, remote func(host string) app.Manager) *hosts.Resolver {
	t.Helper()
	return hosts.NewWith("ws", config.RoleWorkstation, []string{"ws", "hv1"}, func() (app.Manager, func(), error) {
		opened.Add(1)
		t.Error("workstation libvirt opened although the own name is listed")
		return nil, nil, errors.New("no libvirt")
	}).WithRemote(func(host, _ string) (app.Manager, func(), error) {
		return remote(host), func() {}, nil
	})
}

func manifestReq(t *testing.T, h http.Handler, host string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/hosts/"+host+"/images/u.qcow2/manifest", nil)
	r.Header.Set("Authorization", "Bearer "+ownToken)
	h.ServeHTTP(rec, r)
	return rec
}

// A workstation server with the own name listed forwards its image manifest
// requests to the daemon, as for any listed host; not listed, it is still 403.
func TestWorkstationOwnListedForwardsManifest(t *testing.T) {
	var opened, asked atomic.Int32
	res := ownListedWorkstation(t, &opened, func(string) app.Manager { return ownImgMgr{asked: &asked} })
	h := &handlers{hosts: res, role: config.RoleWorkstation}
	srv := chain(h, nil)
	if rec := manifestReq(t, srv, "ws"); rec.Code != 200 {
		t.Fatalf("own name listed: %d %s", rec.Code, rec.Body)
	}
	if asked.Load() != 1 {
		t.Errorf("daemon asked %d times, want 1", asked.Load())
	}
	if opened.Load() != 0 {
		t.Errorf("libvirt opened %d times", opened.Load())
	}

	// Not listed: 403, and still no libvirt.
	var o2 atomic.Int32
	h2 := &handlers{role: config.RoleWorkstation, hosts: hosts.NewWith("ws", config.RoleWorkstation, []string{"hv1"}, func() (app.Manager, func(), error) {
		o2.Add(1)
		return nil, nil, errors.New("no libvirt")
	}).WithRemote(func(string, string) (app.Manager, func(), error) { return ownImgMgr{asked: &asked}, func() {}, nil })}
	if rec := manifestReq(t, chain(h2, nil), "ws"); rec.Code != http.StatusForbidden {
		t.Errorf("own name not listed: %d, want 403", rec.Code)
	}
	if o2.Load() != 0 {
		t.Errorf("libvirt opened %d times for the 403", o2.Load())
	}
}

// The base-image lookup behind create-on-a-remote-host opens no libvirt when
// the own name is listed, and is "not found" with no libvirt either.
func TestServerLocalImagesOwnListed(t *testing.T) {
	var opened atomic.Int32
	res := ownListedWorkstation(t, &opened, func(string) app.Manager { return ownImgMgr{asked: &atomic.Int32{}} })
	_, err := localImages(res, true)("u.qcow2")
	if !vmtool.IsNotFound(err) {
		t.Fatalf("localImages = %v, want not found", err)
	}
	if opened.Load() != 0 {
		t.Errorf("libvirt opened %d times", opened.Load())
	}

	var n atomic.Int32
	neither := hosts.NewWith("laptop", config.RoleWorkstation, []string{"hv1"}, func() (app.Manager, func(), error) {
		n.Add(1)
		return nil, nil, errors.New("libvirt: no socket")
	})
	if _, err := localImages(neither, true)("u.qcow2"); !vmtool.IsNotFound(err) {
		t.Fatalf("no libvirt: localImages = %v, want not found", err)
	}
	if n.Load() != 1 {
		t.Errorf("connector called %d times, want the one lazy open", n.Load())
	}
}

// AddMachine's libvirt name check: skipped when local libvirt is not used.
func TestAddMachineNameCheckSkipped(t *testing.T) {
	var opened atomic.Int32
	h := &handlers{hosts: ownListedWorkstation(t, &opened, func(string) app.Manager { return nil })}
	vms, release := h.libvirtNameCheck()
	release()
	if vms != nil || opened.Load() != 0 {
		t.Fatalf("manager %v, opened %d", vms, opened.Load())
	}
	h = &handlers{hosts: hosts.NewWith("laptop", config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
		opened.Add(1)
		return nil, nil, errors.New("libvirt: no socket")
	})}
	vms, release = h.libvirtNameCheck()
	release()
	if vms != nil || opened.Load() != 1 {
		t.Fatalf("manager %v, opened %d", vms, opened.Load())
	}
}

func TestServerImageDirHelper(t *testing.T) {
	if got := imageDir(nil); got != "" {
		t.Fatalf("nil config: %q", got)
	}
	c := &config.Config{Dir: "/cfg", ImageDir: "imgs"}
	if got := imageDir(c); got != "/cfg/imgs" {
		t.Fatalf("imageDir = %q", got)
	}
}
