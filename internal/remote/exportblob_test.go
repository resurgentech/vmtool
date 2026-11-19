package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// exportTemplate and getImageBlob, the client side (API 2.4.0).

func exportND(w http.ResponseWriter, evs ...api.ExportTemplateEvent) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	for _, e := range evs {
		_ = json.NewEncoder(w).Encode(e)
	}
}

func TestRemoteExportTemplateStreams(t *testing.T) {
	var method, path, query string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		sz, sha, p := int64(1000), fetchSHA, "/var/lib/vmtool/images/proj"
		stage := api.Export
		exportND(w,
			api.ExportTemplateEvent{Event: api.ExportTemplateEventEventProgress, Stage: &stage, Received: ptr(int64(0)), Total: ptr(int64(4096))},
			api.ExportTemplateEvent{Event: api.ExportTemplateEventEventDone,
				Image: &api.ImageManifestEntry{Name: "proj", Sha256: &sha, Size: &sz, Path: &p}})
	})
	var x app.TemplateExporter = f.manager(t) // the remote client exports templates
	var evs []app.ExportEvent
	img, err := x.ExportTemplate(context.Background(), "vmtpl-000000000001", "thin-test", func(e app.ExportEvent) { evs = append(evs, e) })
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/v1/hosts/veritech01/templates/vmtpl-000000000001/export" || query != "pool=thin-test" {
		t.Errorf("request %s %s ?%s", method, path, query)
	}
	if img.Name != "proj" || img.SHA256 != fetchSHA || img.Size != 1000 || img.Path != "/var/lib/vmtool/images/proj" {
		t.Errorf("image %+v", img)
	}
	if len(evs) != 1 || evs[0].Stage != "export" || evs[0].Total != 4096 {
		t.Errorf("events %+v", evs)
	}
	if _, auths := f.seen(); len(auths) != 1 || auths[0] != "Bearer "+testToken {
		t.Errorf("auth %v", auths)
	}
	// no pool: no query
	query = "x"
	if _, err := x.ExportTemplate(context.Background(), "proj", "", nil); err != nil || query != "" {
		t.Errorf("no pool: %v ?%s", err, query)
	}
}

func TestRemoteExportTemplateFailures(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
		kind app.Kind
		msg  string
	}{
		{"error line", func(w http.ResponseWriter, _ *http.Request) {
			exportND(w, api.ExportTemplateEvent{Event: api.ExportTemplateEventEventError, Error: ptr("qemu-img: no space")})
		}, app.KindBadGateway, "no space"},
		{"stream ends", func(w http.ResponseWriter, _ *http.Request) {
			exportND(w, api.ExportTemplateEvent{Event: api.ExportTemplateEventEventProgress, Received: ptr(int64(1))})
		}, app.KindBadGateway, "without a done or error"},
		{"not JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte("not json\n"))
		}, app.KindBadGateway, "not JSON"},
		{"409", func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, 409, api.Error{Error: "image proj already exists"})
		}, app.KindConflict, "already exists"},
		{"404", func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, 404, api.Error{Error: "proj is not a template"})
		}, app.KindNotFound, "not a template"},
		{"old daemon", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, app.KindNotFound, "predates"},
	}
	for _, tc := range cases {
		f := newFakeAPI(t, specVersion(t), tc.h)
		_, err := f.manager(t).ExportTemplate(context.Background(), "proj", "", nil)
		var ae *app.Error
		if !errors.As(err, &ae) || ae.Kind != tc.kind || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v (kind %v), want kind %v containing %q", tc.name, err, ae, tc.kind, tc.msg)
		}
	}
}

func TestRemoteOpenImageBlob(t *testing.T) {
	data := []byte("PLACEHOLDER image bytes")
	var method, path string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("X-Image-SHA256", strings.ToUpper(fetchSHA))
		_, _ = w.Write(data)
	})
	var src app.ImageBlobSource = f.manager(t)
	rc, info, err := src.OpenImageBlob(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if method != "GET" || path != "/v1/hosts/veritech01/images/proj/blob" {
		t.Errorf("request %s %s", method, path)
	}
	if info.Size != int64(len(data)) || info.SHA256 != fetchSHA {
		t.Errorf("info %+v (the digest is lowercased)", info)
	}
	if b, _ := io.ReadAll(rc); string(b) != string(data) {
		t.Errorf("body %q", b)
	}
	if _, auths := f.seen(); len(auths) != 1 || auths[0] != "Bearer "+testToken {
		t.Errorf("auth %v", auths)
	}
}

func TestRemoteOpenImageBlobRefusals(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
		kind app.Kind
	}{
		{"404", func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 404, api.Error{Error: "no such image"}) }, app.KindNotFound},
		{"409 foreign", func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 409, api.Error{Error: "will not serve it"}) }, app.KindConflict},
		{"no Content-Length", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Image-SHA256", fetchSHA)
			w.WriteHeader(200)
			f, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("abc"))
			f.Flush() // chunked: no Content-Length
			_, _ = w.Write([]byte("def"))
		}, app.KindBadGateway},
		{"no digest", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "3")
			_, _ = w.Write([]byte("abc"))
		}, app.KindBadGateway},
		{"bad digest", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "3")
			w.Header().Set("X-Image-SHA256", "zz")
			_, _ = w.Write([]byte("abc"))
		}, app.KindBadGateway},
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://elsewhere.invalid/x", 302) }, app.KindBadGateway},
	}
	for _, tc := range cases {
		f := newFakeAPI(t, specVersion(t), tc.h)
		rc, _, err := f.manager(t).OpenImageBlob(context.Background(), "proj")
		if err == nil {
			rc.Close()
			t.Errorf("%s: no error", tc.name)
			continue
		}
		var ae *app.Error
		if !errors.As(err, &ae) || ae.Kind != tc.kind {
			t.Errorf("%s: %v, want kind %v", tc.name, err, tc.kind)
		}
		if tc.kind == app.KindNotFound && !vmtool.IsNotFound(err) {
			t.Errorf("%s: not matching ErrNotFound", tc.name)
		}
	}
}
