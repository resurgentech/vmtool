package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeapi"
)

// N12: PUT /v1/inventory/{host}/{name}/notes, the one route that writes a
// VM's origin and comment.

const notesOriginJSON = `{"image":"img1","image_sha256":"` + invSHA + `","template":"img1","clone":"thin","pool":"nvme-thin","created":"2026-11-21T00:00:00Z"}`

func (e *invEnv) notes(t *testing.T, host, name, body string) (int, storeapi.VMNotes, string) {
	t.Helper()
	code, resp := e.call(t, "PUT", "/v1/inventory/"+host+"/"+name+"/notes", tokA, body)
	var n storeapi.VMNotes
	if code == 200 {
		if err := json.Unmarshal([]byte(resp), &n); err != nil {
			t.Fatal(err)
		}
	}
	return code, n, resp
}

// Create writes origin and comment with the UUID before the first report; a
// report (which has no origin or comment field) leaves them.
func TestNotesRouteBeforeFirstReport(t *testing.T) {
	e := newInvEnv(t)
	code, n, body := e.notes(t, "hv1", "vm1", `{"uuid":"`+invU1+`","origin":`+notesOriginJSON+`,"comment":"placeholder comment"}`)
	if code != 200 || n.Uuid != invU1 || n.Origin == nil || *n.Origin.Image != "img1" || n.Comment != "placeholder comment" {
		t.Fatalf("= %d %s", code, body)
	}
	for i := 0; i < 2; i++ {
		e.clock.Advance(time.Minute)
		e.report(t, "hv1", tokH, vm("vm1", invU1))
		inv := e.inventory(t, "")
		if len(inv.Vms) != 1 {
			t.Fatalf("inventory %+v", inv)
		}
		v := inv.Vms[0]
		if v.Origin == nil || *v.Origin.Clone != "thin" || *v.Origin.ImageSha256 != invSHA || v.Comment != "placeholder comment" {
			t.Fatalf("report %d: row %+v", i, v)
		}
	}
	e.noSecretsInLogs(t)
}

func TestNotesRouteOriginOnceCommentRules(t *testing.T) {
	e := newInvEnv(t)
	e.report(t, "hv1", tokH, vm("vm1", invU1))
	withOrigin := `{"uuid":"` + invU1 + `","origin":` + notesOriginJSON + `}`
	if code, _, body := e.notes(t, "hv1", "vm1", withOrigin); code != 200 {
		t.Fatalf("origin = %d %s", code, body)
	}
	if code, _, body := e.notes(t, "hv1", "vm1", withOrigin); code != 200 {
		t.Fatalf("same origin again = %d %s, want 200", code, body)
	}
	other := strings.Replace(withOrigin, `"image":"img1"`, `"image":"img2"`, 1)
	code, _, body := e.notes(t, "hv1", "vm1", other)
	if code != 409 || !strings.Contains(body, "origin is recorded once, at create") {
		t.Fatalf("different origin = %d %s, want 409", code, body)
	}
	if o := e.inventory(t, "").Vms[0].Origin; o == nil || *o.Image != "img1" {
		t.Fatalf("origin changed: %+v", o)
	}
	// The comment: set and clear without the UUID (the live row gives it).
	if code, n, body := e.notes(t, "hv1", "vm1", `{"comment":"placeholder comment"}`); code != 200 || n.Uuid != invU1 || n.Comment != "placeholder comment" {
		t.Fatalf("comment = %d %s", code, body)
	}
	if code, n, _ := e.notes(t, "hv1", "vm1", `{"comment":""}`); code != 200 || n.Comment != "" || n.Origin == nil {
		t.Fatalf("clear = %d %+v", code, n)
	}
	for name, c := range map[string]struct{ body, field string }{
		"comment too long":  {`{"comment":"` + strings.Repeat("a", 513) + `"}`, "comment"},
		"comment newline":   {`{"comment":"a\nb"}`, "comment"},
		"comment control":   {`{"comment":"a\u0007b"}`, "comment"},
		"bad uuid":          {`{"uuid":"PLACEHOLDER-not-a-uuid","comment":"x"}`, "uuid"},
		"origin field":      {`{"origin":{"image":"a\nb","clone":"file"}}`, "origin"},
		"origin clone kind": {`{"origin":{"image":"img1","clone":"copy"}}`, "origin"},
		"nothing to write":  {`{}`, "body"},
	} {
		if code, _, body := e.notes(t, "hv1", "vm1", c.body); code != 400 || !strings.Contains(body, "invalid field: "+c.field) {
			t.Errorf("%s = %d %s, want 400 naming %s", name, code, body, c.field)
		}
	}
	if code, _, _ := e.notes(t, "hv1", "vm1/x", `{"comment":"x"}`); code != 400 && code != 404 {
		t.Errorf("bad name = %d", code)
	}
}

// No row at all: 404 without a UUID. A gone row: 409.
func TestNotesRouteMissingAndGoneRows(t *testing.T) {
	e := newInvEnv(t)
	if code, _, body := e.notes(t, "hv1", "vm1", `{"comment":"x"}`); code != 404 {
		t.Fatalf("no row = %d %s, want 404", code, body)
	}
	e.report(t, "hv1", tokH, vm("vm1", invU1))
	e.clock.Advance(time.Minute)
	e.report(t, "hv1", tokH) // vm1 gone
	for _, body := range []string{`{"comment":"x"}`, `{"uuid":"` + invU1 + `","comment":"x"}`} {
		if code, _, resp := e.notes(t, "hv1", "vm1", body); code != 409 {
			t.Errorf("%s on a gone row = %d %s, want 409", body, code, resp)
		}
	}
}

// Host tokens may not write notes: 401, as for any route off their
// allow-list.
func TestNotesRouteRefusesHostToken(t *testing.T) {
	e := newInvEnv(t)
	e.report(t, "hv1", tokH, vm("vm1", invU1))
	code, body := e.call(t, "PUT", "/v1/inventory/hv1/vm1/notes", tokH, `{"comment":"x"}`)
	if code != 401 || strings.Contains(body, tokH) {
		t.Fatalf("host token = %d %s, want 401", code, body)
	}
	if v := e.inventory(t, "").Vms[0]; v.Comment != "" {
		t.Fatalf("host token wrote %q", v.Comment)
	}
	e.noSecretsInLogs(t)
}

// Explicit delete and the sweep remove the notes with the row; orphan notes
// go 7 days after they were written.
func TestNotesRouteDeleteAndSweep(t *testing.T) {
	e := newInvEnv(t)
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	for _, n := range []struct{ name, uuid string }{{"vm1", invU1}, {"vm2", invU2}, {"vm3", invU3}} {
		if code, _, body := e.notes(t, "hv1", n.name, `{"uuid":"`+n.uuid+`","origin":`+notesOriginJSON+`,"comment":"placeholder comment"}`); code != 200 {
			t.Fatalf("%s = %d %s", n.name, code, body)
		}
	}
	if code, body := e.call(t, "DELETE", "/v1/inventory/hv1/vm1?uuid="+invU1, tokA, ""); code != 204 {
		t.Fatalf("delete = %d %s", code, body)
	}
	e.clock.Advance(time.Minute)
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	if v := e.inventory(t, "").Vms[0]; v.Name != "vm1" || v.Origin != nil || v.Comment != "" {
		t.Fatalf("vm1's notes survived the delete: %+v", v)
	}
	e.clock.Advance(time.Minute)
	e.report(t, "hv1", tokH, vm("vm1", invU1)) // vm2 gone
	n, err := RunSweep(t.Context(), SweepDeps{DB: e.db, Now: func() time.Time { return e.clock.Now().Add(db.GracePeriod) }})
	if err != nil {
		t.Fatal(err)
	}
	// vm2's row and notes; vm3's notes, written over 7 days before, never had a row.
	if n.Rows != 1 || n.Notes != 1 {
		t.Fatalf("sweep %+v", n)
	}
	if code, _, _ := e.notes(t, "hv1", "vm2", `{"uuid":"`+invU2+`","comment":"x"}`); code != 200 {
		t.Fatalf("vm2's uuid after the sweep = %d (notes and row gone, a fresh write is accepted)", code)
	}
}
