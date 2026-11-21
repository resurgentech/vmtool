package remote

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// SetComment is PUT .../comment with {"comment": text}; empty is sent as an
// empty string (it clears).
func TestRemoteSetComment(t *testing.T) {
	var bodies []string
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		jsonReply(w, 200, map[string]any{"name": "vm1", "state": "running", "vcpus": 1, "memory": 1, "ip": "", "autostart": false})
	})
	m := f.manager(t)
	if err := m.SetComment("vm1", "placeholder comment"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetComment("vm1", ""); err != nil {
		t.Fatal(err)
	}
	paths, _ := f.seen()
	var puts []string
	for _, p := range paths {
		if strings.HasPrefix(p, "PUT") {
			puts = append(puts, p)
		}
	}
	want := "PUT /v1/hosts/veritech01/vms/vm1/comment"
	if len(puts) != 2 || puts[0] != want || puts[1] != want {
		t.Errorf("requests %q", puts)
	}
	if len(bodies) != 2 || bodies[0] != `{"comment":"placeholder comment"}` || bodies[1] != `{"comment":""}` {
		t.Errorf("bodies %q", bodies)
	}
}

// A 400 from the daemon keeps its message, which names the field.
func TestRemoteSetCommentBadRequest(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 400, map[string]string{"error": "comment: must be at most 512 bytes"})
	})
	err := f.manager(t).SetComment("vm1", "x")
	if err == nil || !strings.Contains(err.Error(), "comment") {
		t.Errorf("err = %v", err)
	}
}

// Info reads origin and comment; a daemon that predates them leaves both empty.
func TestRemoteInfoOriginAndComment(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 200, map[string]any{"name": "vm1", "state": "running", "vcpus": 1, "memory": 1, "ip": "", "autostart": false,
			"comment": "placeholder comment",
			"origin": map[string]any{"image": "img1", "image_sha256": strings.Repeat("a", 64), "template": "img1", "template_lv": "vmtpl-aaaaaaaaaaaa",
				"clone_kind": "thin", "pool": "nvme-thin", "created": "2026-11-21T04:05:06Z"}})
	})
	info, err := f.manager(t).Info("vm1")
	if err != nil {
		t.Fatal(err)
	}
	want := &vmtool.VMOrigin{Image: "img1", ImageSHA256: strings.Repeat("a", 64), Template: "img1", TemplateLV: "vmtpl-aaaaaaaaaaaa",
		CloneKind: vmtool.CloneKindThin, Pool: "nvme-thin", Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}
	if info.Comment != "placeholder comment" || info.Origin == nil || *info.Origin != *want {
		t.Errorf("Info = %+v origin %+v", info, info.Origin)
	}

	old := newFakeAPI(t, "2.5.0", func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 200, map[string]any{"name": "vm1", "state": "running", "vcpus": 1, "memory": 1, "ip": "", "autostart": false})
	})
	info, err = old.manager(t).Info("vm1")
	if err != nil || info.Origin != nil || info.Comment != "" {
		t.Errorf("old daemon: %+v %v", info, err)
	}
}

// Define sends origin and comment only when there are some: a config without
// them is the body a 2.5.0 client sent.
func TestRemoteDefineSendsOriginAndCommentOnlyWhenSet(t *testing.T) {
	var bodies []map[string]any
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		w.WriteHeader(204)
	})
	m := f.manager(t)
	cfg := vmtool.VMConfig{Name: "vm1", VCPUs: 1, Memory: 1, DiskPath: "/p/vm1.qcow2"}
	if err := m.Define(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := bodies[0]["origin"]; ok {
		t.Errorf("origin sent with none: %v", bodies[0])
	}
	if _, ok := bodies[0]["comment"]; ok {
		t.Errorf("comment sent with none: %v", bodies[0])
	}
	cfg.Comment = "placeholder comment"
	cfg.Origin = &vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindNone, Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}
	if err := m.Define(cfg); err != nil {
		t.Fatal(err)
	}
	o, _ := bodies[1]["origin"].(map[string]any)
	if bodies[1]["comment"] != "placeholder comment" || o["image"] != "img1" || o["clone_kind"] != "none" || o["created"] != "2026-11-21T04:05:06Z" {
		t.Errorf("body %v", bodies[1])
	}
}
