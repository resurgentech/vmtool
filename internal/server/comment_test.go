package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
)

// N12: PUT /v1/hosts/{host}/vms/{name}/comment is workstation-only and writes
// the store (the VM's notes); the host is not asked. A hypervisor refuses it
// with 403 (TestWorkstationOnlyRefusedByHypervisor covers every such route).

func TestSetVMCommentWritesTheStore(t *testing.T) {
	s := newSrvStore(t)
	m := &vmMgr{}
	h := delistHandler(t, s, m, true)
	rec := do(h, "PUT", "/v1/hosts/hv1/vms/vm1/comment", `{"comment":"placeholder comment"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var n api.VMNotes
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	if n.Host != "hv1" || n.Name != "vm1" || n.Comment != "placeholder comment" || n.Origin == nil || *n.Origin.Image != "img1" {
		t.Errorf("notes %+v", n)
	}
	if s.notes.Load() != 1 {
		t.Errorf("store notes writes %d", s.notes.Load())
	}
	if strings.Contains(rec.Body.String(), srvStoreTok) {
		t.Error("the store token is in the response")
	}
	// The store has the row but it is gone: 409.
	if rec := do(h, "PUT", "/v1/hosts/hv1/vms/vm1/comment", `{"comment":"gone"}`); rec.Code != 409 {
		t.Errorf("gone row: %d %s", rec.Code, rec.Body)
	}
	// A bad comment: 400 naming the field, the store not asked.
	before := s.notes.Load()
	if rec := do(h, "PUT", "/v1/hosts/hv1/vms/vm1/comment", `{"comment":"a\nb"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "comment") {
		t.Errorf("bad comment: %d %s", rec.Code, rec.Body)
	}
	if s.notes.Load() != before {
		t.Error("a bad comment reached the store")
	}
}

// createVM with a comment and no store in use: 503 before anything is made
// (vmMgr's embedded nil Manager would panic on any call).
func TestCreateVMCommentNeedsStore(t *testing.T) {
	s := newSrvStore(t)
	rec := do(delistHandler(t, s, &vmMgr{}, false), "POST", "/v1/hosts/hv1/vms",
		`{"name":"vm1","image":"img1","comment":"placeholder comment"}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "comments are kept in the store") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// No store configured, or one that does not answer: 503 saying comments are
// kept in the store.
func TestSetVMCommentNeedsStore(t *testing.T) {
	s := newSrvStore(t)
	rec := do(delistHandler(t, s, &vmMgr{}, false), "PUT", "/v1/hosts/hv1/vms/vm1/comment", `{"comment":"x"}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "comments are kept in the store") {
		t.Fatalf("no store: %d %s", rec.Code, rec.Body)
	}
	s.down.Store(true)
	rec = do(delistHandler(t, s, &vmMgr{}, true), "PUT", "/v1/hosts/hv1/vms/vm1/comment", `{"comment":"x"}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "no store is in use") {
		t.Fatalf("store down: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Values(warnHeader); len(got) != 0 {
		t.Errorf("warnings %v", got)
	}
}
