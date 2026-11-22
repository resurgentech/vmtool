package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// tmplMgr is a thin-pool host as the server sees one: it answers the template
// operations from a fixed table and records every call. It never removes
// anything: it is a fake.
type tmplMgr struct {
	*rtMgr
	tpls []app.TemplateInfo
	err  error
}

func tplInfo(lv, name, origin string, stale bool) app.TemplateInfo {
	t := vmtool.Template{Name: name, LV: lv, Pool: "thin-test", Origin: origin, SizeBytes: 10 << 30, UsedBytes: 1 << 30, Clones: 2,
		Created: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	if origin == vmtool.TemplateOriginImage {
		t.Image, t.SHA256 = name, strings.Repeat("a", 64)
	}
	return app.TemplateInfo{Template: t, Stale: stale}
}

func (f *tmplMgr) ListTemplates() ([]app.TemplateInfo, error) {
	f.rec("ListTemplates")
	return f.tpls, f.err
}

func (f *tmplMgr) PrepareTemplate(image, pool string) (app.TemplateInfo, error) {
	f.rec("PrepareTemplate %s %s", image, pool)
	if f.err != nil {
		return app.TemplateInfo{}, f.err
	}
	return tplInfo("vmtpl-aaaaaaaaaaaa", image, vmtool.TemplateOriginImage, false), nil
}

func (f *tmplMgr) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	f.rec("DeleteTemplate %s %s", name, pool)
	if f.err != nil {
		return app.TemplateInfo{}, f.err
	}
	return tplInfo("vmtpl-aaaaaaaaaaaa", "img1", vmtool.TemplateOriginImage, true), nil
}

func templateRoutes(t *testing.T) (http.Handler, *tmplMgr) {
	t.Helper()
	m := &tmplMgr{rtMgr: newRTMgr(t), tpls: []app.TemplateInfo{
		tplInfo("vmtpl-aaaaaaaaaaaa", "img1", vmtool.TemplateOriginImage, true),
		tplInfo("vmtpl-000000000001", "proj", vmtool.TemplateOriginSaved, false),
	}}
	return thinServer(t, m), m
}

func TestTemplateRoutes(t *testing.T) {
	h, m := templateRoutes(t)

	rec := do(h, "GET", "/v1/hosts/box/templates", "")
	var got []api.TemplateInfo
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got) != 2 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	a := got[0]
	if a.Name != "img1" || a.Origin != api.TemplateInfoOrigin("image") || a.Image == nil || *a.Image != "img1" || a.Sha256 == nil ||
		a.Pool != "thin-test" || a.Lv != "vmtpl-aaaaaaaaaaaa" || a.SizeBytes != 10<<30 || a.UsedBytes != 1<<30 || a.Clones != 2 ||
		!a.Stale || a.Created == nil || a.Created.Year() != 2026 {
		t.Errorf("first = %+v", a)
	}
	if b := got[1]; b.Stale || b.Image != nil || b.Sha256 != nil || b.Origin != api.TemplateInfoOrigin("saved") {
		t.Errorf("saved = %+v", b)
	}

	rec = do(h, "POST", "/v1/hosts/box/templates", `{"image":"img1","pool":"thin-test"}`)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), "vmtpl-aaaaaaaaaaaa") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "DELETE", "/v1/hosts/box/templates/img1?pool=thin-test", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "vmtpl-aaaaaaaaaaaa") {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "DELETE", "/v1/hosts/box/templates/vmtpl-aaaaaaaaaaaa", "")
	if rec.Code != 200 {
		t.Fatalf("delete by LV name: %d %s", rec.Code, rec.Body)
	}
	want := []string{"ListTemplates", "PrepareTemplate img1 thin-test", "DeleteTemplate img1 thin-test", "DeleteTemplate vmtpl-aaaaaaaaaaaa "}
	if g := m.got(); strings.Join(g, "|") != strings.Join(want, "|") {
		t.Errorf("calls %q, want %q", g, want)
	}
}

func TestTemplateRouteStatuses(t *testing.T) {
	h, m := templateRoutes(t)
	for _, tc := range []struct {
		name, method, path, body string
		err                      error
		code                     int
	}{
		{"not a template", "DELETE", "/v1/hosts/box/templates/vm1", "", fmt.Errorf("%q is not a template: %w", "vm1", vmtool.ErrNotFound), 404},
		{"ambiguous", "DELETE", "/v1/hosts/box/templates/img1", "", &app.Error{Kind: app.KindConflict, Err: errors.New("matches 2 templates")}, 409},
		{"no room", "POST", "/v1/hosts/box/templates", `{"image":"img1","pool":"thin-test"}`, &app.Error{Kind: app.KindConflict, Err: vmtool.ErrThinNoRoom}, 409},
		{"not a thin pool", "POST", "/v1/hosts/box/templates", `{"image":"img1","pool":"vmtool"}`, &app.Error{Kind: app.KindInvalid, Err: errors.New("not a thin pool")}, 400},
		{"image not on the host", "POST", "/v1/hosts/box/templates", `{"image":"img1","pool":"thin-test"}`, fmt.Errorf("image: %w", vmtool.ErrNotFound), 404},
		{"helper failed", "GET", "/v1/hosts/box/templates", "", errors.New("helper exploded"), 500},
	} {
		m.err = tc.err
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.code)
		}
	}
}

// Input is checked before the host is asked.
func TestTemplateRouteBadInput(t *testing.T) {
	h, m := templateRoutes(t)
	for _, tc := range []struct{ name, method, path, body string }{
		{"no image", "POST", "/v1/hosts/box/templates", `{"pool":"thin-test"}`},
		{"empty image", "POST", "/v1/hosts/box/templates", `{"image":""}`},
		{"path as image", "POST", "/v1/hosts/box/templates", `{"image":"../img1"}`},
		{"markup in pool", "POST", "/v1/hosts/box/templates", `{"image":"img1","pool":"a<b"}`},
		{"no body", "POST", "/v1/hosts/box/templates", ""},
		{"delete: name with dots", "DELETE", "/v1/hosts/box/templates/..%2Fvm1", ""},
		{"delete: name starting with a dot", "DELETE", "/v1/hosts/box/templates/.hidden", ""},
		{"delete: markup in pool", "DELETE", "/v1/hosts/box/templates/img1?pool=a%3Cb", ""},
	} {
		rec := do(h, tc.method, tc.path, tc.body)
		if rec.Code != 400 && rec.Code != 404 {
			t.Errorf("%s: %d %s, want a refusal", tc.name, rec.Code, rec.Body)
		}
		if tc.name == "no image" || tc.name == "empty image" || tc.name == "path as image" || tc.name == "markup in pool" {
			if rec.Code != 400 {
				t.Errorf("%s: %d, want 400", tc.name, rec.Code)
			}
		}
	}
	if g := m.got(); len(g) != 0 {
		t.Errorf("the host was asked for bad input: %q", g)
	}
}

// A host with no thin pools (local libvirt): the list is empty, prepare is
// 400, delete is 404.
func TestTemplateRoutesOnHostWithoutThinPools(t *testing.T) {
	fm, _ := poolFixture(t)
	h := primServer(t, fm, "workstation")
	rec := do(h, "GET", "/v1/hosts/box/templates", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "POST", "/v1/hosts/box/templates", `{"image":"img1"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "no thin pools") {
		t.Errorf("create: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "DELETE", "/v1/hosts/box/templates/img1", "")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "no thin pools") {
		t.Errorf("delete: %d %s", rec.Code, rec.Body)
	}
	if len(fm.calls) != 0 {
		t.Errorf("libvirt calls %q", fm.calls)
	}
	// and an unknown host is a 404 on every route
	for _, r := range []struct{ m, p, b string }{
		{"GET", "/v1/hosts/nope/templates", ""}, {"POST", "/v1/hosts/nope/templates", `{"image":"img1"}`}, {"DELETE", "/v1/hosts/nope/templates/img1", ""},
	} {
		if rec := do(h, r.m, r.p, r.b); rec.Code != 404 {
			t.Errorf("%s %s: %d", r.m, r.p, rec.Code)
		}
	}
}

// A workstation forwards the three routes for a listed host to that host's
// daemon, with the caller's bearer; the daemon's statuses come back.
func TestWorkstationForwardsTemplates(t *testing.T) {
	t.Chdir(t.TempDir())
	const callerToken = "test-caller-token-91b7"
	m := &tmplMgr{rtMgr: newRTMgr(t), tpls: []app.TemplateInfo{tplInfo("vmtpl-aaaaaaaaaaaa", "img1", vmtool.TemplateOriginImage, true)}}
	seedCached(t, m.pool, "img1")
	hv, pool, hvLog := hypervisor(t, m, callerToken)
	ws := workstation(t, hv.URL, pool, callerToken, &handlers{})

	req := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+callerToken)
		rec := httptest.NewRecorder()
		ws.ServeHTTP(rec, r)
		return rec
	}
	if rec := req("GET", "/v1/hosts/veritech01/templates", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stale":true`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	// the daemon already caches img1 and this machine has none: nothing is
	// uploaded, the daemon is asked to build
	if rec := req("POST", "/v1/hosts/veritech01/templates", `{"image":"img1","pool":"thin-test"}`); rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := req("DELETE", "/v1/hosts/veritech01/templates/vmtpl-aaaaaaaaaaaa?pool=thin-test", ""); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	m.err = fmt.Errorf("%q is not a template: %w", "vm1", vmtool.ErrNotFound)
	if rec := req("DELETE", "/v1/hosts/veritech01/templates/vm1", ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), "not a template") {
		t.Fatalf("not a template: %d %s", rec.Code, rec.Body)
	}
	got := strings.Join(m.got(), "|")
	for _, w := range []string{"ListTemplates", "PrepareTemplate img1 thin-test", "DeleteTemplate vmtpl-aaaaaaaaaaaa thin-test", "DeleteTemplate vm1 "} {
		if !strings.Contains(got, w) {
			t.Errorf("daemon calls %q lack %q", got, w)
		}
	}
	for _, s := range hvLog.seen() {
		if !strings.HasSuffix(s, " Bearer "+callerToken) {
			t.Errorf("daemon saw %q, want the caller's bearer", s)
		}
	}
}

// ListPools adds the thin pool to the fixed libvirt pools.
func (f *tmplMgr) ListPools() ([]vmtool.PoolInfo, error) {
	ps, err := f.rtMgr.ListPools()
	return append(ps, vmtool.PoolInfo{Name: "thin-test", Path: "/dev/vg-test", Active: true, Type: vmtool.PoolTypeThin}), err
}
