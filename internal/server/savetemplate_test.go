package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// SaveTemplate makes tmplMgr an app.TemplateSaver. The fake records the call
// and answers a fixed saved template (or err).
func (f *tmplMgr) SaveTemplate(vm, name string, replace bool) (app.SavedTemplate, error) {
	f.rec("SaveTemplate %s %s %v", vm, name, replace)
	if f.err != nil {
		return app.SavedTemplate{}, f.err
	}
	return app.SavedTemplate{
		TemplateInfo: tplInfo("vmtpl-000000000009", name, vmtool.TemplateOriginSaved, false),
		ExtraDisks:   []string{"vdb"},
	}, nil
}

func saveRoutes(t *testing.T) (http.Handler, *tmplMgr) {
	h, m := templateRoutes(t)
	m.info.State = vmtool.StateShutoff
	return h, m
}

func TestSaveTemplateRoute(t *testing.T) {
	h, m := saveRoutes(t)
	rec := do(h, "POST", "/v1/hosts/box/templates", `{"from_vm":"vm1","name":"proj2","replace":true}`)
	if rec.Code != 201 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var got api.TemplateInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "proj2" || got.Origin != api.TemplateInfoOrigin("saved") || got.Lv != "vmtpl-000000000009" ||
		got.ExtraDisks == nil || len(*got.ExtraDisks) != 1 || (*got.ExtraDisks)[0] != "vdb" {
		t.Errorf("answer %+v", got)
	}
	if g := strings.Join(m.got(), "|"); g != "Info vm1|SaveTemplate vm1 proj2 true" {
		t.Errorf("calls %q", g)
	}
	// the other routes never carry extra_disks
	if rec := do(h, "GET", "/v1/hosts/box/templates", ""); strings.Contains(rec.Body.String(), "extra_disks") {
		t.Errorf("list carries extra_disks: %s", rec.Body)
	}
}

func TestSaveTemplateRouteStatuses(t *testing.T) {
	h, m := saveRoutes(t)
	body := `{"from_vm":"vm1","name":"proj2"}`
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"name in use", &app.Error{Kind: app.KindConflict, Err: errors.New("name in use; --replace")}, 409},
		{"no room", &app.Error{Kind: app.KindConflict, Err: vmtool.ErrThinNoRoom}, 409},
		{"not on a thin pool", &app.Error{Kind: app.KindInvalid, Err: errors.New("templates are saved from VMs on a thin pool")}, 400},
		{"vm not found", fmt.Errorf("domain: %w", vmtool.ErrNotFound), 404},
		{"helper failed", errors.New("helper exploded"), 500},
	} {
		m.err = tc.err
		if rec := do(h, "POST", "/v1/hosts/box/templates", body); rec.Code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.code)
		}
	}
	// a VM that is not shut off is a 409 naming the fix; the host is not asked
	m.err = nil
	m.info.State = vmtool.StateRunning
	m.reset()
	rec := do(h, "POST", "/v1/hosts/box/templates", body)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "shut it down first") {
		t.Errorf("running: %d %s", rec.Code, rec.Body)
	}
	for _, c := range m.got() {
		if strings.HasPrefix(c, "SaveTemplate") {
			t.Errorf("the host was asked to save a running VM: %q", m.got())
		}
	}
}

func TestSaveTemplateRouteBadInput(t *testing.T) {
	h, m := saveRoutes(t)
	for name, body := range map[string]string{
		"image and from_vm":  `{"image":"img1","from_vm":"vm1","name":"proj"}`,
		"neither":            `{"name":"proj"}`,
		"no name":            `{"from_vm":"vm1"}`,
		"empty name":         `{"from_vm":"vm1","name":""}`,
		"path as name":       `{"from_vm":"vm1","name":"../x"}`,
		"bad vm":             `{"from_vm":"a b","name":"proj"}`,
		"name without vm":    `{"image":"img1","name":"proj"}`,
		"replace with image": `{"image":"img1","replace":true}`,
		"pool with from_vm":  `{"from_vm":"vm1","name":"proj","pool":"thin-test"}`,
	} {
		if rec := do(h, "POST", "/v1/hosts/box/templates", body); rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	if g := m.got(); len(g) != 0 {
		t.Errorf("the host was asked for bad input: %q", g)
	}
}

// A save moves no image, so it never decides the store mode (no probe of the
// store), while building an image template does.
func TestSaveTemplateDoesNotProbeTheStore(t *testing.T) {
	s := newSrvStore(t)
	m := &tmplMgr{rtMgr: newRTMgr(t)}
	m.info.State = vmtool.StateShutoff
	t.Chdir(t.TempDir())
	res := hosts.NewWith("box", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return m, func() {}, nil })
	pool := s.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	h := handlerFor(&handlers{hosts: res, role: config.RoleHypervisor, storeURL: s.ts.URL,
		storeToken: func() (string, error) { return srvStoreTok, nil }, storeOpts: []storeclient.Option{storeclient.WithRootCAs(pool)}})

	if rec := do(h, "POST", "/v1/hosts/box/templates", `{"from_vm":"vm1","name":"proj2"}`); rec.Code != 201 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if n := s.whoami.Load(); n != 0 {
		t.Errorf("a save probed the store %d times", n)
	}
	if rec := do(h, "POST", "/v1/hosts/box/templates", `{"image":"img1"}`); rec.Code == 0 {
		t.Fatal("no answer")
	}
	if n := s.whoami.Load(); n != 1 {
		t.Errorf("an image template request probed the store %d times, want 1 (the control)", n)
	}
}

// A workstation forwards a save for a listed host to the daemon, with the
// caller's bearer, and the extra-disks list comes back.
func TestWorkstationForwardsSaveTemplate(t *testing.T) {
	t.Chdir(t.TempDir())
	const callerToken = "test-caller-token-91b7"
	m := &tmplMgr{rtMgr: newRTMgr(t)}
	m.info.State = vmtool.StateShutoff
	hv, pool, hvLog := hypervisor(t, m, callerToken)
	ws := workstation(t, hv.URL, pool, callerToken, &handlers{})
	r := httptest.NewRequest("POST", "/v1/hosts/veritech01/templates", strings.NewReader(`{"from_vm":"vm1","name":"proj2","replace":true}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+callerToken)
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, r)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"extra_disks":["vdb"]`) {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(m.got(), "|"); !strings.Contains(got, "SaveTemplate vm1 proj2 true") {
		t.Errorf("daemon calls %q", got)
	}
	for _, s := range hvLog.seen() {
		if !strings.HasSuffix(s, " Bearer "+callerToken) {
			t.Errorf("daemon saw %q, want the caller's bearer", s)
		}
	}
}
