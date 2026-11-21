package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Migration 7 adds the reservations table to a version-6 database and keeps
// its networks.
func TestMigration7AddsReservationTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	d, err := openMigrations(ctx, path, key(1), migrations[:6])
	if err != nil {
		t.Fatal(err)
	}
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	d.Close()
	d, err = Open(ctx, path, key(1))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v, _ := d.version(ctx); v != len(migrations) || len(migrations) < 7 {
		t.Fatalf("version %d want %d (at least 7)", v, len(migrations))
	}
	if _, err := d.GetNetwork(ctx, "net1"); err != nil {
		t.Fatalf("network lost in migration: %v", err)
	}
	r, err := d.PutNetworkReservation(ctx, NetworkReservation{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.0.100", Host: "hv1", VM: "web1"})
	if err != nil || r.Created.IsZero() {
		t.Fatalf("put = %+v, %v", r, err)
	}
	if _, err := d.PutNetworkReservation(ctx, NetworkReservation{Network: "net1", MAC: "52:54:00:00:00:02", IP: "10.77.0.100", Host: "hv1", VM: "web2"}); !errors.Is(err, ErrAddressTaken) {
		t.Errorf("taken address: %v", err)
	}
	if _, err := d.PutNetworkReservation(ctx, NetworkReservation{Network: "nonet", MAC: "52:54:00:00:00:02", IP: "10.77.0.101", Host: "hv1", VM: "web2"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown network: %v", err)
	}
	// An address is numerically ordered: .99 before .100.
	if _, err := d.PutNetworkReservation(ctx, NetworkReservation{Network: "net1", MAC: "52:54:00:00:00:03", IP: "10.77.0.99", Host: "hv1", VM: "web3"}); err != nil {
		t.Fatal(err)
	}
	got, err := d.NetworkReservations(ctx, "net1")
	if err != nil || len(got) != 2 || got[0].IP != "10.77.0.99" {
		t.Fatalf("list = %+v, %v", got, err)
	}
}
