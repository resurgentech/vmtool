package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/testexec"
)

// networkHomeServer serves ws (local) and hv1 and home1; home1 is a network
// home. Every remote host is fm.
func networkHomeServer(t *testing.T, fm *fakeMgr) *hosts.Resolver {
	t.Helper()
	return hosts.NewWith("ws", config.RoleWorkstation, []string{"hv1", "home1"},
		func() (app.Manager, func(), error) { return fm, func() {}, nil }).
		WithNetworkHomes("home1").
		WithRemote(func(string, string) (app.Manager, func(), error) {
			return probeFake{Manager: fm, st: app.HostStatus{Reachable: true}}, func() {}, nil
		}).
		WithToken("tok")
}

// POST /v1/hosts/{host}/vms on a network home is 409 with a plain message and
// the host's Manager is never called; on another host it goes on.
func TestCreateVMOnNetworkHomeIs409(t *testing.T) {
	t.Chdir(t.TempDir())
	testexec.Ansible(t)
	fm := &fakeMgr{}
	h := handlerFor(&handlers{hosts: networkHomeServer(t, fm), role: config.RoleWorkstation, svc: &app.Service{}, tunnelExe: stubTunnelExe})
	req := httptest.NewRequest("POST", "/v1/hosts/home1/vms", strings.NewReader(`{"name":"web","image":"u.qcow2"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, w := range []string{"home1", "network home", "vms: false"} {
		if !strings.Contains(rec.Body.String(), w) {
			t.Errorf("body %s lacks %q", rec.Body, w)
		}
	}
	if len(fm.calls) != 0 {
		t.Errorf("the host was called: %v", fm.calls)
	}
}

// GET /v1/hosts marks the network home and only it.
func TestListHostsMarksNetworkHome(t *testing.T) {
	fm := &fakeMgr{}
	h := handlerFor(&handlers{hosts: networkHomeServer(t, fm)})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/hosts", nil)
	r.Header.Set("Authorization", "Bearer tok")
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got []api.HostInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("hosts %s", rec.Body)
	}
	for _, hi := range got {
		marked := hi.NetworkHome != nil && *hi.NetworkHome
		if want := hi.Name == "home1"; marked != want {
			t.Errorf("%s network_home = %v, want %v", hi.Name, marked, want)
		}
	}
}
