package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A VM's notes: what vmtool records about it besides what its host reports,
// its origin and its comment, in the vm_notes table keyed (host, uuid). Only
// PutNotes writes them; no report touches them. They go with their inventory
// row (DeleteVMRow, Sweep), and notes whose row never appears are swept
// GracePeriod after they were last written.

var (
	// ErrGone is PutNotes on a VM whose inventory row is gone.
	ErrGone = errors.New("the VM is gone from the inventory")
	// ErrOriginRecorded is PutNotes with an origin that differs from the one
	// already recorded.
	ErrOriginRecorded = errors.New("origin is recorded once, at create")
)

// Origin is what a VM was made from, as the create wrote it. Created is the
// creation time as text (RFC 3339).
type Origin struct {
	Image    string
	SHA256   string
	Template string
	Clone    string
	Pool     string
	Created  string
}

// Notes are a VM's notes as stored. Origin is nil when none was recorded.
type Notes struct {
	Host    string
	Name    string
	UUID    string
	Origin  *Origin
	Comment string
}

// NotesWrite is one write of a VM's notes. UUID empty: the live row of
// (Host, Name) gives it. Origin nil and Comment nil leave what is stored.
type NotesWrite struct {
	Host    string
	Name    string
	UUID    string
	Origin  *Origin
	Comment *string
}

// PutNotes writes a VM's notes in one transaction and returns them as
// stored.
//   - Without a UUID it needs a live row of (Host, Name) and takes its UUID:
//     ErrGone when the only rows of that name are gone, ErrNotFound when
//     there is none.
//   - With a UUID the notes are stored even before the host's first report
//     has made the row; a row of that UUID that is gone is ErrGone, and one
//     with another name is ErrNotFound.
//   - The origin is written only while none is recorded; the same origin
//     again changes nothing; another one is ErrOriginRecorded.
//   - The comment replaces the stored one ("" clears it).
func (d *DB) PutNotes(ctx context.Context, w NotesWrite) (Notes, error) {
	if w.Host == "" || w.Name == "" {
		return Notes{}, errors.New("put notes: a host and a name are needed")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Notes{}, fmt.Errorf("put notes: %w", err)
	}
	defer tx.Rollback()
	now := d.now().UTC().Format(time.RFC3339Nano)

	uuid := w.UUID
	if uuid == "" {
		live, err := queryStrings(ctx, tx,
			`SELECT uuid FROM vms WHERE host=? AND name=? AND gone_since IS NULL ORDER BY first_seen DESC, uuid`, w.Host, w.Name)
		if err != nil {
			return Notes{}, fmt.Errorf("put notes: %w", err)
		}
		if len(live) == 0 {
			gone, err := queryStrings(ctx, tx, `SELECT uuid FROM vms WHERE host=? AND name=?`, w.Host, w.Name)
			if err != nil {
				return Notes{}, fmt.Errorf("put notes: %w", err)
			}
			if len(gone) > 0 {
				return Notes{}, ErrGone
			}
			return Notes{}, ErrNotFound
		}
		uuid = live[0]
	} else {
		var name string
		var gone sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT name, gone_since FROM vms WHERE host=? AND uuid=?`, w.Host, uuid).Scan(&name, &gone)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// no row yet: create's write before the host's first report
		case err != nil:
			return Notes{}, fmt.Errorf("put notes: %w", err)
		case name != w.Name:
			return Notes{}, ErrNotFound
		case gone.Valid:
			return Notes{}, ErrGone
		}
	}

	cur, written, found, err := readNotes(ctx, tx, w.Host, uuid)
	if err != nil {
		return Notes{}, fmt.Errorf("put notes: %w", err)
	}
	if !found {
		cur = Notes{Host: w.Host, Name: w.Name, UUID: uuid}
	}
	cur.Name = w.Name
	if w.Origin != nil {
		switch {
		case !written:
			o := *w.Origin
			cur.Origin = &o
		case *cur.Origin != *w.Origin:
			return Notes{}, ErrOriginRecorded
		}
	}
	if w.Comment != nil {
		cur.Comment = *w.Comment
	}
	o := cur.Origin
	if o == nil {
		o = &Origin{}
	}
	var originWritten, commentUpdated any
	if w.Origin != nil && !written {
		originWritten = now
	}
	if w.Comment != nil {
		commentUpdated = now
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO vm_notes(host, uuid, name, origin_image, origin_sha256, origin_template, origin_clone,
		   origin_pool, origin_created, comment, origin_written, comment_updated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(host, uuid) DO UPDATE SET name=excluded.name,
		   origin_image=excluded.origin_image, origin_sha256=excluded.origin_sha256,
		   origin_template=excluded.origin_template, origin_clone=excluded.origin_clone,
		   origin_pool=excluded.origin_pool, origin_created=excluded.origin_created,
		   comment=excluded.comment,
		   origin_written=COALESCE(vm_notes.origin_written, excluded.origin_written),
		   comment_updated=COALESCE(excluded.comment_updated, vm_notes.comment_updated)`,
		w.Host, uuid, w.Name, o.Image, o.SHA256, o.Template, o.Clone, o.Pool, o.Created, cur.Comment,
		originWritten, commentUpdated)
	if err != nil {
		return Notes{}, fmt.Errorf("put notes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Notes{}, fmt.Errorf("put notes: %w", err)
	}
	return cur, nil
}

// readNotes reads the notes of (host, uuid); written reports whether an
// origin is recorded.
func readNotes(ctx context.Context, tx *sql.Tx, host, uuid string) (n Notes, written, found bool, err error) {
	var o Origin
	var ow sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT name, origin_image, origin_sha256, origin_template, origin_clone, origin_pool, origin_created,
		        comment, origin_written
		 FROM vm_notes WHERE host=? AND uuid=?`, host, uuid).
		Scan(&n.Name, &o.Image, &o.SHA256, &o.Template, &o.Clone, &o.Pool, &o.Created, &n.Comment, &ow)
	if errors.Is(err, sql.ErrNoRows) {
		return Notes{}, false, false, nil
	}
	if err != nil {
		return Notes{}, false, false, err
	}
	n.Host, n.UUID = host, uuid
	if ow.Valid {
		n.Origin = &o
	}
	return n, ow.Valid, true, nil
}
