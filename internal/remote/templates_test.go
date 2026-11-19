package remote

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const tplJSON = `{"name":"img1","origin":"image","image":"img1","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
	`"pool":"thin-test","lv":"vmtpl-aaaaaaaaaaaa","size_bytes":10737418240,"used_bytes":1073741824,"clones":2,"stale":true,"created":"2026-11-18T10:00:00Z"}`

func TestRemoteListTemplates(t *testing.T) {
	var method, path string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + tplJSON + `,{"name":"proj","origin":"saved","pool":"thin-test","lv":"vmtpl-000000000001","size_bytes":1,"used_bytes":0,"clones":0,"stale":false}]`))
	})
	m := f.manager(t)
	var tm app.TemplateManager = m // the remote client is a TemplateManager
	got, err := tm.ListTemplates()
	if err != nil || len(got) != 2 {
		t.Fatalf("ListTemplates = %+v, %v", got, err)
	}
	if method != "GET" || !strings.HasSuffix(path, "/templates") {
		t.Errorf("request %s %s", method, path)
	}
	a := got[0]
	if a.Name != "img1" || a.Origin != vmtool.TemplateOriginImage || a.Image != "img1" || a.SHA256 != strings.Repeat("a", 64) ||
		a.Pool != "thin-test" || a.LV != "vmtpl-aaaaaaaaaaaa" || a.SizeBytes != 10<<30 || a.UsedBytes != 1<<30 ||
		a.Clones != 2 || !a.Stale || a.Created.Year() != 2026 {
		t.Errorf("first = %+v", a)
	}
	if b := got[1]; b.Origin != vmtool.TemplateOriginSaved || b.Stale || !b.Created.IsZero() || b.Image != "" {
		t.Errorf("saved = %+v", b)
	}
}

func TestRemotePrepareTemplate(t *testing.T) {
	var method, path, body string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(tplJSON))
	})
	got, err := f.manager(t).PrepareTemplate("img1", "thin-test")
	if err != nil || got.LV != "vmtpl-aaaaaaaaaaaa" {
		t.Fatalf("PrepareTemplate = %+v, %v", got, err)
	}
	if method != "POST" || !strings.HasSuffix(path, "/templates") || !strings.Contains(body, `"image":"img1"`) || !strings.Contains(body, `"pool":"thin-test"`) {
		t.Errorf("request %s %s %s", method, path, body)
	}
	// an empty pool is not sent: the host's default
	if _, err := f.manager(t).PrepareTemplate("img1", ""); err != nil || strings.Contains(body, "pool") {
		t.Errorf("empty pool: %v, body %s", err, body)
	}
}

func TestRemoteDeleteTemplate(t *testing.T) {
	var method, path, query string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tplJSON))
	})
	got, err := f.manager(t).DeleteTemplate("vmtpl-aaaaaaaaaaaa", "thin-test")
	if err != nil || got.LV != "vmtpl-aaaaaaaaaaaa" {
		t.Fatalf("DeleteTemplate = %+v, %v", got, err)
	}
	if method != "DELETE" || !strings.HasSuffix(path, "/templates/vmtpl-aaaaaaaaaaaa") || query != "pool=thin-test" {
		t.Errorf("request %s %s ?%s", method, path, query)
	}
	// no pool: no query
	if _, err := f.manager(t).DeleteTemplate("img1", ""); err != nil || query != "" {
		t.Errorf("no pool: %v ?%s", err, query)
	}
}

// Statuses map to the app's kinds: 404 not found, 400 invalid, 409 conflict.
func TestRemoteTemplateErrors(t *testing.T) {
	for status, kind := range map[int]app.Kind{404: app.KindNotFound, 400: app.KindInvalid, 409: app.KindConflict, 500: app.KindInternal} {
		f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		})
		m := f.manager(t)
		_, e1 := m.DeleteTemplate("x", "")
		_, e2 := m.PrepareTemplate("x", "")
		_, e3 := m.ListTemplates()
		for i, err := range []error{e1, e2, e3} {
			var ae *app.Error
			if !app.AsError(err, &ae) || ae.Kind != kind {
				t.Errorf("status %d call %d: %v, want kind %v", status, i, err, kind)
			}
		}
	}
}

// A save is createTemplate with from_vm and name (and replace when set), no
// image and no pool; the extra disks come back.
func TestRemoteSaveTemplate(t *testing.T) {
	var method, path, body string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"proj","origin":"saved","pool":"thin-test","lv":"vmtpl-000000000009","size_bytes":1,"used_bytes":0,"clones":0,"stale":false,"extra_disks":["vdb"]}`))
	})
	var ts app.TemplateSaver = f.manager(t) // the remote client saves
	got, err := ts.SaveTemplate("vm1", "proj", true)
	if err != nil || got.LV != "vmtpl-000000000009" || got.Origin != vmtool.TemplateOriginSaved || len(got.ExtraDisks) != 1 || got.ExtraDisks[0] != "vdb" {
		t.Fatalf("SaveTemplate = %+v, %v", got, err)
	}
	if method != "POST" || !strings.HasSuffix(path, "/templates") || !strings.Contains(body, `"from_vm":"vm1"`) ||
		!strings.Contains(body, `"name":"proj"`) || !strings.Contains(body, `"replace":true`) || strings.Contains(body, "image") || strings.Contains(body, "pool") {
		t.Errorf("request %s %s %s", method, path, body)
	}
	if _, err := f.manager(t).SaveTemplate("vm1", "proj", false); err != nil || strings.Contains(body, "replace") {
		t.Errorf("no replace: %v %s", err, body)
	}
}
