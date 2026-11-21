package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// N12: a VM's notes (origin and comment) live in vm_notes, keyed (host,
// uuid), written only by PutNotes; no report touches them.

var testOrigin = Origin{Image: "img1", SHA256: hash, Template: "img1", Clone: "thin", Pool: "nvme-thin", Created: "2026-11-01T12:00:00Z"}

func notesOf(t *testing.T, d *DB, host, uuid string, withGone bool) (*Origin, string, bool) {
	t.Helper()
	v, ok := vmByUUID(t, d, host, uuid, withGone)
	return v.Origin, v.Comment, ok
}

// Create writes the notes with the UUID before the host's first report has
// made the row; the row then shows them, and later reports leave them.
func TestNotesBeforeFirstReportAndReportsLeaveThem(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	o := testOrigin
	n, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Origin: &o, Comment: str("placeholder comment")})
	if err != nil {
		t.Fatal(err)
	}
	if n.UUID != uuid1 || n.Origin == nil || *n.Origin != testOrigin || n.Comment != "placeholder comment" {
		t.Fatalf("notes %+v", n)
	}
	if _, _, ok := notesOf(t, d, "hv1", uuid1, true); ok {
		t.Fatal("notes alone made an inventory row")
	}
	for i := 0; i < 3; i++ {
		c.t = c.t.Add(time.Minute)
		if _, err := d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{row(uuid1, "vm1")}); err != nil {
			t.Fatal(err)
		}
		got, comment, ok := notesOf(t, d, "hv1", uuid1, false)
		if !ok || got == nil || *got != testOrigin || comment != "placeholder comment" {
			t.Fatalf("report %d: origin %+v comment %q ok %v", i, got, comment, ok)
		}
	}
	// A VM with no notes lists no origin and an empty comment.
	d.ApplyReport(ctx, "hv1", c.t.Add(time.Minute), "v", []VMRow{row(uuid1, "vm1"), row(uuid2, "vm2")})
	if o, comment, ok := notesOf(t, d, "hv1", uuid2, false); !ok || o != nil || comment != "" {
		t.Fatalf("vm2 origin %+v comment %q", o, comment)
	}
}

// Origin is written once: the same again changes nothing, a different one is
// refused; the comment is set and cleared at any time on a live row.
func TestNotesOriginOnceCommentAnyTime(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	o := testOrigin
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Origin: &o}); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Hour)
	same := testOrigin
	if n, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Origin: &same}); err != nil || *n.Origin != testOrigin {
		t.Fatalf("same origin again: %+v %v", n, err)
	}
	other := testOrigin
	other.Image = "img2"
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Origin: &other}); !errors.Is(err, ErrOriginRecorded) {
		t.Fatalf("different origin: %v", err)
	}
	if got, _, _ := notesOf(t, d, "hv1", uuid1, false); *got != testOrigin {
		t.Fatalf("origin changed to %+v", got)
	}
	// Without the UUID the live row of (host, name) gives it.
	if n, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", Comment: str("placeholder comment")}); err != nil || n.UUID != uuid1 {
		t.Fatalf("comment: %+v %v", n, err)
	}
	if _, comment, _ := notesOf(t, d, "hv1", uuid1, false); comment != "placeholder comment" {
		t.Fatalf("comment %q", comment)
	}
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", Comment: str("")}); err != nil {
		t.Fatal(err)
	}
	if got, comment, _ := notesOf(t, d, "hv1", uuid1, false); comment != "" || *got != testOrigin {
		t.Fatalf("after clear: origin %+v comment %q", got, comment)
	}
}

// A comment needs a live row when no UUID is given (not found otherwise),
// and a gone row takes no notes.
func TestNotesNeedLiveRow(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", Comment: str("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no row: %v", err)
	}
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	c.t = c.t.Add(time.Minute)
	d.ApplyReport(ctx, "hv1", c.t, "v", nil) // vm1 is gone
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", Comment: str("x")}); !errors.Is(err, ErrGone) {
		t.Fatalf("gone row, no uuid: %v", err)
	}
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Comment: str("x")}); !errors.Is(err, ErrGone) {
		t.Fatalf("gone row, uuid: %v", err)
	}
	if _, comment, _ := notesOf(t, d, "hv1", uuid1, true); comment != "" {
		t.Fatalf("gone row took a comment %q", comment)
	}
	// A UUID whose row has another name is not this VM.
	d.ApplyReport(ctx, "hv1", c.t.Add(time.Minute), "v", []VMRow{row(uuid2, "vm2")})
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid2, Comment: str("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uuid of another name: %v", err)
	}
}

// Notes go with their row: an explicit delete and the sweep remove them.
func TestNotesGoWithTheirRow(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1"), row(uuid2, "vm2")})
	for _, u := range []string{uuid1, uuid2} {
		o := testOrigin
		if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: map[string]string{uuid1: "vm1", uuid2: "vm2"}[u], UUID: u, Origin: &o, Comment: str("placeholder comment")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.DeleteVMRow(ctx, "hv1", "vm1", uuid1); err != nil {
		t.Fatal(err)
	}
	if notesRows(t, d, "hv1", uuid1) != 0 {
		t.Fatal("explicit delete left the notes")
	}
	// The same UUID reported again starts with no notes.
	c.t = c.t.Add(time.Minute)
	d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{row(uuid1, "vm1"), row(uuid2, "vm2")})
	if o, comment, _ := notesOf(t, d, "hv1", uuid1, false); o != nil || comment != "" {
		t.Fatalf("notes came back: %+v %q", o, comment)
	}
	c.t = c.t.Add(time.Minute)
	d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{row(uuid1, "vm1")}) // vm2 gone
	n, err := d.Sweep(ctx, c.t.Add(GracePeriod))
	if err != nil {
		t.Fatal(err)
	}
	if n.Rows != 1 || notesRows(t, d, "hv1", uuid2) != 0 {
		t.Fatalf("sweep %+v left vm2's notes", n)
	}
}

// Notes whose row never appears are removed 7 days after they were last
// written, not before.
func TestOrphanNotesSweptAfterSevenDays(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	o := testOrigin
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Origin: &o}); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(24 * time.Hour)
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Comment: str("placeholder comment")}); err != nil {
		t.Fatal(err)
	}
	written := c.t
	if n, _ := d.Sweep(ctx, written.Add(GracePeriod-time.Second)); n.Notes != 0 || notesRows(t, d, "hv1", uuid1) != 1 {
		t.Fatalf("swept before 7 days after the last write: %+v", n)
	}
	n, err := d.Sweep(ctx, written.Add(GracePeriod))
	if err != nil {
		t.Fatal(err)
	}
	if n.Notes != 1 || notesRows(t, d, "hv1", uuid1) != 0 {
		t.Fatalf("orphan notes kept: %+v", n)
	}
	// Notes with a live row are never orphans.
	d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{row(uuid2, "vm2")})
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm2", Comment: str("x")}); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Sweep(ctx, c.t.Add(30*GracePeriod)); n.Notes != 0 || notesRows(t, d, "hv1", uuid2) != 1 {
		t.Fatalf("notes of a live row swept: %+v", n)
	}
}

func notesRows(t *testing.T, d *DB, host, uuid string) int {
	t.Helper()
	var n int
	if err := d.sql.QueryRow(`SELECT count(*) FROM vm_notes WHERE host=? AND uuid=?`, host, uuid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Migration 4 copies the origin and comment columns of vms into vm_notes and
// drops them.
func TestMigration4CarriesNotesOver(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	d, err := openMigrations(ctx, path, key(1), migrations[:3])
	if err != nil {
		t.Fatal(err)
	}
	stamp := t0.Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO hosts(name, last_report, generated_at, daemon_version) VALUES ('hv1', '` + stamp + `', '` + stamp + `', '2.6.0')`,
		`INSERT INTO vms(host, uuid, name, state, vcpus, memory_gib, ip, autostart, firmware, disks,
		   origin_image, origin_sha256, origin_template, origin_clone, origin_pool, created, comment, first_seen, last_seen, gone_since)
		 VALUES ('hv1', '` + uuid1 + `', 'vm1', 'running', 2, 4, '', 0, '', '[]',
		   'img1', '` + hash + `', 'img1', 'thin', 'nvme-thin', '2026-11-01T12:00:00Z', 'placeholder comment', '` + stamp + `', '` + stamp + `', NULL)`,
		`INSERT INTO vms(host, uuid, name, state, vcpus, memory_gib, ip, autostart, firmware, disks,
		   origin_image, origin_sha256, origin_template, origin_clone, origin_pool, created, comment, first_seen, last_seen, gone_since)
		 VALUES ('hv1', '` + uuid2 + `', 'vm2', 'running', 2, 4, '', 0, '', '[]',
		   '', '', '', '', '', '', '', '` + stamp + `', '` + stamp + `', NULL)`,
	} {
		if _, err := d.sql.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	d, err = Open(ctx, path, key(1))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer d.Close()
	if v, _ := d.version(ctx); v != 4 {
		t.Fatalf("version %d want 4", v)
	}
	o, comment, ok := notesOf(t, d, "hv1", uuid1, false)
	if !ok || o == nil || *o != testOrigin || comment != "placeholder comment" {
		t.Fatalf("vm1 after upgrade: origin %+v comment %q ok %v", o, comment, ok)
	}
	if o, comment, ok := notesOf(t, d, "hv1", uuid2, false); !ok || o != nil || comment != "" || notesRows(t, d, "hv1", uuid2) != 0 {
		t.Fatalf("vm2 (no notes) after upgrade: origin %+v comment %q", o, comment)
	}
	// The origin was recorded: it is not written a second time.
	other := testOrigin
	other.Image = "img2"
	if _, err := d.PutNotes(ctx, NotesWrite{Host: "hv1", Name: "vm1", UUID: uuid1, Origin: &other}); !errors.Is(err, ErrOriginRecorded) {
		t.Fatalf("origin after upgrade: %v", err)
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT name FROM pragma_table_info('vms')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		rows.Scan(&col)
		switch col {
		case "origin_image", "origin_sha256", "origin_template", "origin_clone", "origin_pool", "created", "comment":
			t.Errorf("vms still has column %s", col)
		}
	}
}
