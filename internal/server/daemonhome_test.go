package server

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/spec"
)

// daemonHomeRefused is the set of operations a daemon with network_home: true
// refuses with 409; pinned here so a new VM-creating route is a decision.
var daemonHomeRefused = []string{"cloneVolume", "createTemplate", "createVolume", "defineVM"}

const daemonHomeMessage = "home1 is a network home and runs no VMs"

// homeDaemon is home1's own daemon: a hypervisor whose config says
// network_home: true, with a fake Manager that records every call.
func homeDaemon(t *testing.T, fm *fakeMgr, networkHome bool) http.Handler {
	t.Helper()
	r := hosts.NewWith("home1", config.RoleHypervisor, nil,
		func() (app.Manager, func(), error) { return fm, func() {}, nil }).WithToken("tok")
	return handlerFor(&handlers{hosts: r, role: config.RoleHypervisor, svc: &app.Service{}, networkHome: networkHome})
}

// Walk every operation of the spec: with network_home the four VM-creating
// ones answer 409 with the plain message, before any Manager call, and no
// other operation does; without it none does.
func TestDaemonNetworkHomeRefusesCreatingRoutes(t *testing.T) {
	t.Chdir(t.TempDir())
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatal(err)
	}
	type op struct{ method, path, id string }
	var ops []op
	for p, item := range doc.Paths.Map() {
		for m, o := range item.Operations() {
			ops = append(ops, op{m, p, o.OperationID})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].id < ops[j].id })
	skip := map[string]bool{"openVMTunnel": true} // takes over the connection

	for _, networkHome := range []bool{true, false} {
		var refused []string
		for _, o := range ops {
			if skip[o.id] {
				continue
			}
			fm := &fakeMgr{}
			h := homeDaemon(t, fm, networkHome)
			url := strings.ReplaceAll(o.path, "{host}", "home1")
			// Any other {param} is a plain placeholder.
			for strings.Contains(url, "{") {
				i, j := strings.Index(url, "{"), strings.Index(url, "}")
				url = url[:i] + "x" + url[j+1:]
			}
			req := httptest.NewRequest(strings.ToUpper(o.method), url, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer tok")
			rec := httptest.NewRecorder()
			func() {
				defer func() { _ = recover() }() // a bare fake may panic on a route that is not ours
				h.ServeHTTP(rec, req)
			}()
			got := rec.Code == 409 && strings.Contains(rec.Body.String(), daemonHomeMessage)
			if got {
				refused = append(refused, o.id)
				if len(fm.calls) != 0 {
					t.Errorf("%s: refused after calling the host: %v", o.id, fm.calls)
				}
			}
		}
		sort.Strings(refused)
		want := daemonHomeRefused
		if !networkHome {
			want = nil
		}
		if strings.Join(refused, ",") != strings.Join(want, ",") {
			t.Errorf("network_home=%v: refused %v, want %v", networkHome, refused, want)
		}
	}
}

// The refusal comes before the body is validated, and the other routes of a
// home (networks, inventory) are served.
func TestDaemonNetworkHomeServesOthers(t *testing.T) {
	fm := &fakeMgr{}
	h := homeDaemon(t, fm, true)
	req := httptest.NewRequest("GET", "/v1/hosts/home1/pools", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("listPools %d %s", rec.Code, rec.Body)
	}
}
