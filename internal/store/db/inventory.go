package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// GracePeriod is how long a delisted VM's row, and its login, are kept
// before Sweep removes them.
const GracePeriod = 7 * 24 * time.Hour

// VMRow is one VM of a host's report. Host is the report's host, not a
// field here. A report carries no origin and no comment: those are the VM's
// notes (PutNotes). Disks is a JSON array (nil is stored as []); it never
// holds a secret.
type VMRow struct {
	UUID      string
	Name      string
	State     string
	VCPUs     int
	MemoryGiB int
	IP        string
	Autostart bool
	Firmware  string
	Disks     json.RawMessage
	// DisksUnreadable: the host could not read this VM's disks. ApplyReport
	// then keeps the stored disks and sets the flag; Disks is ignored.
	DisksUnreadable bool
	// Networks is a JSON array of the VM's cards on vmtool networks
	// ({network, mac, ip}); nil is stored as []. Each report replaces it.
	Networks json.RawMessage
}

// ReportCounts is what ApplyReport did. Stale is true when the report was
// older than the last applied one for the host and was ignored (all counts
// are then zero).
type ReportCounts struct {
	Listed    int // distinct VMs in the report
	NewlyGone int // live rows marked gone (including orphan credential records)
	Relisted  int // gone rows that came back
	Stale     bool
}

// HostRow is one host of the inventory. Live and Gone count its VM rows.
type HostRow struct {
	Name          string
	LastReport    time.Time
	GeneratedAt   time.Time
	DaemonVersion string
	Live          int
	Gone          int
}

// InventoryVM is a VMRow as stored, with its host, times, its notes (Origin
// nil and Comment empty when it has none) and the computed HasCredentials (a
// credential record of the same host, name and UUID).
type InventoryVM struct {
	Host string
	VMRow
	Origin         *Origin
	Comment        string
	FirstSeen      time.Time
	LastSeen       time.Time
	GoneSince      *time.Time
	HasCredentials bool
}

// Inventory is what ListInventory returns.
type Inventory struct {
	Hosts []HostRow
	VMs   []InventoryVM
}

// Delisted is what Delist did: the UUID of the row marked gone and when its
// login will be removed.
type Delisted struct {
	UUID      string
	KeptUntil time.Time
}

// SweepCounts is what Sweep removed. Notes counts notes whose row never
// appeared (notes of a removed row go with it and are not counted).
type SweepCounts struct {
	Rows        int
	Credentials int
	Notes       int
}

const vmCols = `uuid, name, state, vcpus, memory_gib, ip, autostart, firmware, disks,
	first_seen, last_seen, gone_since`

// ApplyReport applies one host's full VM list in one transaction. Each listed
// VM is inserted or updated (last_seen = now, gone_since cleared); each live
// row of the host not listed is marked gone; a credential record of the host
// whose UUID matches no row gets a gone row. The VM's notes (vm_notes) are
// never touched. A VM with DisksUnreadable keeps its stored disks and is
// marked; a report that reads them clears the mark. Reports are ordered by
// the store's own clock, read when the call starts (the time the request was
// taken): a report received before the last applied one for the host changes
// nothing (Stale). generatedAt, the daemon's clock, is stored and shown only,
// so a host with a wrong clock is neither refused nor able to block its later
// reports. An empty list marks every live
// row gone and deletes nothing. Nothing reads last_seen or last_report.
func (d *DB) ApplyReport(ctx context.Context, host string, generatedAt time.Time, version string, vms []VMRow) (ReportCounts, error) {
	var cnt ReportCounts
	for _, v := range vms {
		if v.UUID == "" || v.Name == "" {
			return cnt, errors.New("apply report: a VM needs a uuid and a name")
		}
		if len(v.Disks) > 0 && !json.Valid(v.Disks) {
			return cnt, errors.New("apply report: disks is not valid JSON")
		}
		if len(v.Networks) > 0 && !json.Valid(v.Networks) {
			return cnt, errors.New("apply report: networks is not valid JSON")
		}
	}
	// The receive time is taken before the transaction waits for its turn, so
	// of two overlapping requests the one taken first is the older.
	received := d.now().UTC()
	now := received.Format(time.RFC3339Nano)
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return cnt, fmt.Errorf("apply report: %w", err)
	}
	defer tx.Rollback()
	gen := generatedAt.UTC()

	var prev string
	err = tx.QueryRowContext(ctx, `SELECT last_report FROM hosts WHERE name=?`, host).Scan(&prev)
	switch {
	case err == nil:
		if received.Before(parseTime(prev)) {
			return ReportCounts{Stale: true}, nil
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return cnt, fmt.Errorf("apply report: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO hosts(name, last_report, generated_at, daemon_version) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET last_report=excluded.last_report,
		   generated_at=excluded.generated_at, daemon_version=excluded.daemon_version`,
		host, now, gen.Format(time.RFC3339Nano), version); err != nil {
		return cnt, fmt.Errorf("apply report: %w", err)
	}

	listed := map[string]bool{}
	for _, v := range vms {
		if listed[v.UUID] {
			continue
		}
		listed[v.UUID] = true
		cnt.Listed++
		disks := string(v.Disks)
		if disks == "" {
			disks = "[]"
		}
		networks := string(v.Networks)
		if networks == "" {
			networks = "[]"
		}
		var gone sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT gone_since FROM vms WHERE host=? AND uuid=?`, host, v.UUID).Scan(&gone)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return cnt, fmt.Errorf("apply report: %w", err)
		}
		if !exists {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO vms(host, `+vmCols+`, disks_unreadable, networks) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
				host, v.UUID, v.Name, v.State, v.VCPUs, v.MemoryGiB, v.IP, v.Autostart, v.Firmware, disks,
				now, now, v.DisksUnreadable, networks)
		} else {
			if gone.Valid {
				cnt.Relisted++
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE vms SET name=?, state=?, vcpus=?, memory_gib=?, ip=?, autostart=?, firmware=?,
				   disks=CASE WHEN ? THEN disks ELSE ? END, disks_unreadable=?, networks=?,
				   last_seen=?, gone_since=NULL
				 WHERE host=? AND uuid=?`,
				v.Name, v.State, v.VCPUs, v.MemoryGiB, v.IP, v.Autostart, v.Firmware,
				v.DisksUnreadable, disks, v.DisksUnreadable, networks, now, host, v.UUID)
		}
		if err != nil {
			return cnt, fmt.Errorf("apply report: %w", err)
		}
	}

	// Live rows of this host that the report omits.
	live, err := queryStrings(ctx, tx, `SELECT uuid FROM vms WHERE host=? AND gone_since IS NULL`, host)
	if err != nil {
		return cnt, fmt.Errorf("apply report: %w", err)
	}
	for _, u := range live {
		if listed[u] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vms SET gone_since=? WHERE host=? AND uuid=?`, now, host, u); err != nil {
			return cnt, fmt.Errorf("apply report: %w", err)
		}
		cnt.NewlyGone++
	}

	// Credential records of this host with no row at all get a gone row, so
	// logins orphaned before the inventory existed fall under the grace period.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO vms(host, `+vmCols+`)
		 SELECT c.host, c.uuid, c.name, 'unknown', 0, 0, '', 0, '', '[]', ?, ?, ?
		 FROM vm_credentials c
		 WHERE c.host=? AND c.uuid<>'' AND NOT EXISTS (SELECT 1 FROM vms v WHERE v.host=c.host AND v.uuid=c.uuid)`,
		now, now, now, host)
	if err != nil {
		return cnt, fmt.Errorf("apply report: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return cnt, fmt.Errorf("apply report: %w", err)
	}
	cnt.NewlyGone += int(n)

	if err := tx.Commit(); err != nil {
		return ReportCounts{}, fmt.Errorf("apply report: %w", err)
	}
	return cnt, nil
}

func queryStrings(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Delist marks the live row(s) of (host, name) gone now and returns the UUID
// (the most recently first seen, if more than one) and the time the login
// is kept until (now + GracePeriod). It returns ErrNotFound when there is no
// live row.
func (d *DB) Delist(ctx context.Context, host, name string) (Delisted, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Delisted{}, fmt.Errorf("delist: %w", err)
	}
	defer tx.Rollback()
	uuids, err := queryStrings(ctx, tx,
		`SELECT uuid FROM vms WHERE host=? AND name=? AND gone_since IS NULL ORDER BY first_seen DESC, uuid`, host, name)
	if err != nil {
		return Delisted{}, fmt.Errorf("delist: %w", err)
	}
	if len(uuids) == 0 {
		return Delisted{}, ErrNotFound
	}
	now := d.now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE vms SET gone_since=? WHERE host=? AND name=? AND gone_since IS NULL`,
		now.Format(time.RFC3339Nano), host, name); err != nil {
		return Delisted{}, fmt.Errorf("delist: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Delisted{}, fmt.Errorf("delist: %w", err)
	}
	return Delisted{UUID: uuids[0], KeptUntil: now.Add(GracePeriod)}, nil
}

// DeleteVMRow removes the row (host, uuid) at once, if it also has the given
// name, and the VM's notes with it (also notes whose row never appeared);
// ErrNotFound when there was neither. It leaves the credential record alone.
func (d *DB) DeleteVMRow(ctx context.Context, host, name, uuid string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete vm row: %w", err)
	}
	defer tx.Rollback()
	n := int64(0)
	for _, q := range []string{
		`DELETE FROM vms WHERE host=? AND name=? AND uuid=?`,
		`DELETE FROM vm_notes WHERE host=? AND name=? AND uuid=?`,
	} {
		res, err := tx.ExecContext(ctx, q, host, name, uuid)
		if err != nil {
			return fmt.Errorf("delete vm row: %w", err)
		}
		k, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete vm row: %w", err)
		}
		n += k
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete vm row: %w", err)
	}
	return nil
}

// ListInventory returns the host rows and VM rows, for one host or (host "")
// all, ordered by host then name. Gone VMs are included only with withGone.
// Host counts cover live and gone rows either way.
func (d *DB) ListInventory(ctx context.Context, host string, withGone bool) (Inventory, error) {
	var inv Inventory
	hq := `SELECT h.name, h.last_report, h.generated_at, h.daemon_version,
	         (SELECT count(*) FROM vms v WHERE v.host=h.name AND v.gone_since IS NULL),
	         (SELECT count(*) FROM vms v WHERE v.host=h.name AND v.gone_since IS NOT NULL)
	       FROM hosts h WHERE (?='' OR h.name=?) ORDER BY h.name`
	rows, err := d.sql.QueryContext(ctx, hq, host, host)
	if err != nil {
		return inv, fmt.Errorf("list inventory: %w", err)
	}
	for rows.Next() {
		var h HostRow
		var lr, ga string
		if err := rows.Scan(&h.Name, &lr, &ga, &h.DaemonVersion, &h.Live, &h.Gone); err != nil {
			rows.Close()
			return inv, fmt.Errorf("list inventory: %w", err)
		}
		h.LastReport, h.GeneratedAt = parseTime(lr), parseTime(ga)
		inv.Hosts = append(inv.Hosts, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inv, fmt.Errorf("list inventory: %w", err)
	}

	vq := `SELECT v.host, v.uuid, v.name, v.state, v.vcpus, v.memory_gib, v.ip, v.autostart, v.firmware, v.disks,
	         n.origin_written IS NOT NULL, COALESCE(n.origin_image, ''), COALESCE(n.origin_sha256, ''),
	         COALESCE(n.origin_template, ''), COALESCE(n.origin_clone, ''), COALESCE(n.origin_pool, ''),
	         COALESCE(n.origin_created, ''), COALESCE(n.comment, ''),
	         v.first_seen, v.last_seen, v.gone_since, v.disks_unreadable, v.networks,
	         EXISTS (SELECT 1 FROM vm_credentials c WHERE c.host=v.host AND c.name=v.name AND c.uuid=v.uuid)
	       FROM vms v LEFT JOIN vm_notes n ON n.host=v.host AND n.uuid=v.uuid
	       WHERE (?='' OR v.host=?) AND (? OR v.gone_since IS NULL)
	       ORDER BY v.host, v.name, v.first_seen, v.uuid`
	rows, err = d.sql.QueryContext(ctx, vq, host, host, withGone)
	if err != nil {
		return inv, fmt.Errorf("list inventory: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v InventoryVM
		var disks, networks, first, last string
		var gone sql.NullString
		var hasOrigin bool
		var o Origin
		if err := rows.Scan(&v.Host, &v.UUID, &v.Name, &v.State, &v.VCPUs, &v.MemoryGiB, &v.IP, &v.Autostart,
			&v.Firmware, &disks, &hasOrigin, &o.Image, &o.SHA256, &o.Template, &o.Clone, &o.Pool, &o.Created,
			&v.Comment, &first, &last, &gone, &v.DisksUnreadable, &networks, &v.HasCredentials); err != nil {
			return inv, fmt.Errorf("list inventory: %w", err)
		}
		if hasOrigin {
			v.Origin = &o
		}
		v.Disks = json.RawMessage(disks)
		v.Networks = json.RawMessage(networks)
		v.FirstSeen, v.LastSeen = parseTime(first), parseTime(last)
		if gone.Valid {
			g := parseTime(gone.String)
			v.GoneSince = &g
		}
		inv.VMs = append(inv.VMs, v)
	}
	return inv, rows.Err()
}

// Sweep deletes rows gone for GracePeriod or more as of now, and with each
// the credential record of the same (host, name) only when its UUID equals
// the row's. It reads gone_since only, never last_seen or last_report.
func (d *DB) Sweep(ctx context.Context, now time.Time) (SweepCounts, error) {
	var out SweepCounts
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("sweep: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT host, name, uuid, gone_since FROM vms WHERE gone_since IS NOT NULL`)
	if err != nil {
		return out, fmt.Errorf("sweep: %w", err)
	}
	type key struct{ host, name, uuid string }
	var due []key
	cutoff := now.UTC().Add(-GracePeriod)
	for rows.Next() {
		var k key
		var gone string
		if err := rows.Scan(&k.host, &k.name, &k.uuid, &gone); err != nil {
			rows.Close()
			return out, fmt.Errorf("sweep: %w", err)
		}
		if !parseTime(gone).After(cutoff) {
			due = append(due, k)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("sweep: %w", err)
	}
	for _, k := range due {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vms WHERE host=? AND uuid=?`, k.host, k.uuid); err != nil {
			return out, fmt.Errorf("sweep: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM vm_notes WHERE host=? AND uuid=?`, k.host, k.uuid); err != nil {
			return out, fmt.Errorf("sweep: %w", err)
		}
		out.Rows++
		res, err := tx.ExecContext(ctx, `DELETE FROM vm_credentials WHERE host=? AND name=? AND uuid=?`, k.host, k.name, k.uuid)
		if err != nil {
			return out, fmt.Errorf("sweep: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return out, fmt.Errorf("sweep: %w", err)
		}
		out.Credentials += int(n)
	}
	orphans, err := orphanNotesDue(ctx, tx, cutoff)
	if err != nil {
		return out, fmt.Errorf("sweep: %w", err)
	}
	for _, k := range orphans {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vm_notes WHERE host=? AND uuid=?`, k[0], k[1]); err != nil {
			return out, fmt.Errorf("sweep: %w", err)
		}
		out.Notes++
	}
	if err := tx.Commit(); err != nil {
		return SweepCounts{}, fmt.Errorf("sweep: %w", err)
	}
	return out, nil
}

// orphanNotesDue lists the (host, uuid) of notes that have no inventory row
// and were last written (origin or comment) at or before cutoff.
func orphanNotesDue(ctx context.Context, tx *sql.Tx, cutoff time.Time) ([][2]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT n.host, n.uuid, COALESCE(n.origin_written, ''), COALESCE(n.comment_updated, '')
		 FROM vm_notes n WHERE NOT EXISTS (SELECT 1 FROM vms v WHERE v.host=n.host AND v.uuid=n.uuid)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due [][2]string
	for rows.Next() {
		var host, uuid, ow, cu string
		if err := rows.Scan(&host, &uuid, &ow, &cu); err != nil {
			return nil, err
		}
		last := parseTime(ow)
		if c := parseTime(cu); c.After(last) {
			last = c
		}
		if !last.After(cutoff) {
			due = append(due, [2]string{host, uuid})
		}
	}
	return due, rows.Err()
}
