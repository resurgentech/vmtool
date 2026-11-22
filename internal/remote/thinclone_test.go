package remote

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
)

var (
	_ app.TemplateCloner = (*Manager)(nil)
	_ app.TemplateFinder = (*Manager)(nil)
)

// CloneImageFrom sends the image name as `template`; CloneImage sends none.
func TestRemoteCloneSendsTemplate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clone    func(m *Manager) (string, error)
		template string
		present  bool
	}{
		{"from image", func(m *Manager) (string, error) {
			return m.CloneImageFrom("img1", "/pool/img1", "web", "thin-test", 5<<30)
		}, "img1", true},
		{"empty image", func(m *Manager) (string, error) {
			return m.CloneImageFrom("", "/pool/img1", "web", "thin-test", 0)
		}, "", false},
		{"plain CloneImage", func(m *Manager) (string, error) {
			return m.CloneImage("/pool/img1", "web", "thin-test", 0)
		}, "", false},
	} {
		var body map[string]any
		f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			jsonReply(w, 201, map[string]string{"path": "/dev/vg-test/web"})
		})
		p, err := tc.clone(f.manager(t))
		if err != nil || p != "/dev/vg-test/web" {
			t.Fatalf("%s: %q, %v", tc.name, p, err)
		}
		got, has := body["template"]
		if has != tc.present || (has && got != tc.template) {
			t.Errorf("%s: body %v, template = %v (present %v), want %q (present %v)", tc.name, body, got, has, tc.template, tc.present)
		}
		if body["base_path"] != "/pool/img1" || body["new_name"] != "web" || body["target_pool"] != "thin-test" {
			t.Errorf("%s: the rest of the request changed: %v", tc.name, body)
		}
	}
}

// A 409 from cloneVolume (a thin pool without room) is a conflict with the
// daemon's numbers in the message.
func TestRemoteCloneNoRoomIsConflict(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 409, map[string]string{"error": "thin pool thin-test (vg-test) has no room: data use is 80.00%, at or above the 80% limit"})
	})
	_, err := f.manager(t).CloneImageFrom("img1", "/pool/img1", "web", "thin-test", 0)
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindConflict || !strings.Contains(err.Error(), "80.00%") {
		t.Fatalf("err = %v", err)
	}
}

// TemplatePath is getImagePath.
func TestRemoteTemplatePath(t *testing.T) {
	var path string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		jsonReply(w, 200, map[string]string{"path": "/dev/vg-test/vmtpl-000000000001"})
	})
	p, err := f.manager(t).TemplatePath("proj")
	if err != nil || p != "/dev/vg-test/vmtpl-000000000001" || !strings.HasSuffix(path, "/images/proj/path") {
		t.Errorf("TemplatePath = %q, %v (path %q)", p, err, path)
	}
}
