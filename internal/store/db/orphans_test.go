package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// G3: a reservation carries its VM's UUID, and is orphaned only on a positive
// fact the store holds: a client marked it, or the sweep removed its VM's gone
// row. Silence marks nothing.

func orphanMACs(t *testing.T, d *DB, network string) []string {
	t.Helper()
	rs, err := d.OrphanedReservations(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.MAC)
	}
	return out
}

// Migration 8 adds uuid and orphaned_at; an existing record gets the UUID of
// the live inventory row of the same (host, name), else stays without.
func TestMigration8GivesExistingReservationsTheirUUID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	d, err := openMigrations(ctx, path, key(1), migrations[:7])
	if err != nil {
		t.Fatal(err)
	}
	d.SetClock((&clock{t: t0}).now)
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	if _, err := d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")}); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][4]string{
		{"52:54:00:00:00:01", "10.77.0.100", "hv1", "vm1"}, // matches the live row
		{"52:54:00:00:00:02", "10.77.0.101", "hv1", "vm2"}, // no row
		{"52:54:00:00:00:03", "10.77.0.102", "hv2", "vm1"}, // same name, another host
	} {
		if _, err := d.sql.ExecContext(ctx, `INSERT INTO network_reservations(network, mac, ip, host, vm, created) VALUES ('net1', ?, ?, ?, ?, ?)`,
			r[0], r[1], r[2], r[3], t0.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	d, err = Open(ctx, path, key(1))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(migrations) < 8 {
		t.Fatalf("%d migrations, want at least 8", len(migrations))
	}
	rs, err := d.NetworkReservations(ctx, "net1")
	if err != nil || len(rs) != 3 {
		t.Fatalf("list = %+v, %v", rs, err)
	}
	want := map[string]string{"52:54:00:00:00:01": uuid1, "52:54:00:00:00:02": "", "52:54:00:00:00:03": ""}
	for _, r := range rs {
		if r.UUID != want[r.MAC] || !r.OrphanedAt.IsZero() {
			t.Errorf("%s: uuid %q orphaned %v, want uuid %q and not orphaned", r.MAC, r.UUID, r.OrphanedAt, want[r.MAC])
		}
	}
}

func TestReservationKeepsItsUUIDAndMarkingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	put := func(uuid string) NetworkReservation {
		t.Helper()
		r, err := d.PutNetworkReservation(ctx, NetworkReservation{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.0.100", Host: "hv1", VM: "vm1", UUID: uuid})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := put(uuid1); r.UUID != uuid1 {
		t.Fatalf("put = %+v", r)
	}
	if r := put(""); r.UUID != uuid1 {
		t.Errorf("a put with no uuid dropped it: %+v", r)
	}
	c.t = t0.Add(time.Hour)
	r, err := d.MarkReservationOrphaned(ctx, "net1", "52:54:00:00:00:01")
	if err != nil || !r.OrphanedAt.Equal(c.t) {
		t.Fatalf("mark = %+v, %v", r, err)
	}
	c.t = t0.Add(2 * time.Hour)
	if r, err := d.MarkReservationOrphaned(ctx, "net1", "52:54:00:00:00:01"); err != nil || !r.OrphanedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("a second mark = %+v, %v; want the first time kept", r, err)
	}
	if got := orphanMACs(t, d, "net1"); len(got) != 1 {
		t.Errorf("orphans = %v", got)
	}
	// A new put of the MAC (a card made again) is live again.
	if r := put(uuid2); !r.OrphanedAt.IsZero() || r.UUID != uuid2 {
		t.Errorf("a put after the mark = %+v, want it live again", r)
	}
	if _, err := d.MarkReservationOrphaned(ctx, "net1", "52:54:00:00:00:09"); !errors.Is(err, ErrNotFound) {
		t.Errorf("mark of a missing record: %v", err)
	}
	if _, err := d.OrphanedReservations(ctx, "nonet"); !errors.Is(err, ErrNotFound) {
		t.Errorf("orphans of a missing network: %v", err)
	}
}

// The sweep marks the reservations of the VM whose gone row it removes, after
// the grace period and not before; a record with no UUID, a record of a VM
// that has no row, and a record of a live VM are left alone.
func TestSweepMarksTheReservationsOfARemovedRow(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	put := func(mac, ip, vm, uuid string) {
		t.Helper()
		if _, err := d.PutNetworkReservation(ctx, NetworkReservation{Network: "net1", MAC: mac, IP: ip, Host: "hv1", VM: vm, UUID: uuid}); err != nil {
			t.Fatal(err)
		}
	}
	put("52:54:00:00:00:01", "10.77.0.100", "vm1", uuid1) // its row goes gone
	put("52:54:00:00:00:02", "10.77.0.101", "vm1", uuid1) // a second card of the same VM
	put("52:54:00:00:00:03", "10.77.0.102", "vm2", uuid2) // a live VM
	put("52:54:00:00:00:04", "10.77.0.103", "vm3", "")    // no uuid: never orphaned by the sweep
	put("52:54:00:00:00:05", "10.77.0.104", "vm4", uuid3) // a new VM, no row yet
	if _, err := d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1"), row(uuid2, "vm2")}); err != nil {
		t.Fatal(err)
	}
	c.t = t0.Add(time.Minute)
	if _, err := d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{row(uuid2, "vm2")}); err != nil {
		t.Fatal(err)
	}
	gone := c.t

	n, err := d.Sweep(ctx, gone.Add(6*24*time.Hour+23*time.Hour))
	if err != nil || n.Rows != 0 || n.Orphaned != 0 {
		t.Fatalf("before 7 days: %+v, %v", n, err)
	}
	if got := orphanMACs(t, d, "net1"); len(got) != 0 {
		t.Fatalf("a gone row under 7 days orphaned %v", got)
	}

	n, err = d.Sweep(ctx, gone.Add(7*24*time.Hour))
	if err != nil || n.Rows != 1 || n.Orphaned != 2 {
		t.Fatalf("at 7 days: %+v, %v", n, err)
	}
	got := orphanMACs(t, d, "net1")
	if len(got) != 2 || got[0] != "52:54:00:00:00:01" || got[1] != "52:54:00:00:00:02" {
		t.Errorf("orphans = %v, want the two cards of the removed VM", got)
	}
}

// A host being silent marks nothing: a live row that is never reported again
// is not gone, so nothing is swept and nothing is orphaned.
func TestSilenceMarksNothing(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	if _, err := d.PutNetworkReservation(ctx, NetworkReservation{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.0.100", Host: "hv1", VM: "vm1", UUID: uuid1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")}); err != nil {
		t.Fatal(err)
	}
	c.t = t0.Add(400 * 24 * time.Hour)
	if n, err := d.Sweep(ctx, c.t); err != nil || n.Orphaned != 0 {
		t.Fatalf("sweep after a year of silence: %+v, %v", n, err)
	}
	if got := orphanMACs(t, d, "net1"); len(got) != 0 {
		t.Errorf("silence orphaned %v", got)
	}
}
