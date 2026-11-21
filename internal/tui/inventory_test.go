package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/storeapi"
)

// N9: I opens a read-only view of the store's inventory, read through
// app.Service.Inventory.

func strp(s string) *string { return &s }

func invFixture(gone bool) *app.Inventory {
	t0 := time.Date(2026, 11, 21, 1, 2, 3, 0, time.UTC)
	inv := &app.Inventory{
		Hosts:    []storeapi.InventoryHost{{Name: "hv1", LastReport: t0, DaemonVersion: "2.6.0", Live: 1, Gone: 1}},
		Machines: []storeapi.MachineSummary{{Name: "nuc1", Kind: "metal"}},
		Vms: []storeapi.InventoryVM{{Host: "hv1", Name: "vm1", State: "running", Vcpus: 2, Memory: 4, Ip: "10.0.0.5",
			Disks:   []storeapi.Disk{{Target: "vda", Pool: "nvme", CapacityBytes: 20 << 30, Kind: "thin"}},
			Origin:  &storeapi.VMOrigin{Image: strp("img1"), Clone: strp("thin"), Created: strp("2026-11-20T00:00:00Z")},
			Comment: "placeholder comment", FirstSeen: t0, LastSeen: t0}},
	}
	if gone {
		ku := t0.Add(7 * 24 * time.Hour)
		inv.Vms = append(inv.Vms, storeapi.InventoryVM{Host: "hv1", Name: "vm2", State: "shutoff", Vcpus: 1, Memory: 1,
			FirstSeen: t0, LastSeen: t0, GoneSince: &t0, CredentialsKeptUntil: &ku})
	}
	return inv
}

func inventoryModel() (model, *[]bool) {
	var asked []bool
	m := initialModel(&tuiFake{host: "hv1"}, "hv1", "ansible/playbooks", "ansible/inventory.yml")
	m.readInventory = func(_ context.Context, host string, gone bool) (*app.Inventory, error) {
		asked = append(asked, gone)
		return invFixture(gone), nil
	}
	return m, &asked
}

func TestInventoryViewIsReadOnlyAndShowsTheStoresRecord(t *testing.T) {
	m, asked := inventoryModel()
	next, cmd := m.Update(key("I"))
	m = next.(model)
	if m.mode != modeInventory || cmd == nil {
		t.Fatalf("I: mode %v, cmd %v", m.mode, cmd)
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	out := m.View()
	for _, want := range []string{"hv1", "2026-11-21T01:02:03Z", "2.6.0", "hv1/vm1", "running", "10.0.0.5", "vda 20G thin",
		"img1 (thin)", "placeholder comment", "nuc1", "metal"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"vm2", " ago", "stale", "offline"} {
		if strings.Contains(out, bad) {
			t.Errorf("view has %q:\n%s", bad, out)
		}
	}
	// g adds the gone rows (a second read with gone=true) and shows when they go.
	next, cmd = m.Update(key("g"))
	m = next.(model)
	if cmd == nil {
		t.Fatal("g: no read")
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	out = m.View()
	if !strings.Contains(out, "hv1/vm2") || !strings.Contains(out, "2026-11-28T01:02:03Z") {
		t.Errorf("gone view:\n%s", out)
	}
	if len(*asked) != 2 || (*asked)[0] || !(*asked)[1] {
		t.Errorf("reads asked gone=%v, want [false true]", *asked)
	}
	// Read-only: keys that change things do nothing here, esc is back.
	for _, k := range []string{"D", "s", "c", "enter"} {
		next, cmd = m.Update(key(k))
		if next.(model).mode != modeInventory || cmd != nil {
			t.Errorf("%s in the inventory view: mode %v, cmd %v", k, next.(model).mode, cmd)
		}
	}
	next, _ = m.Update(key("esc"))
	if next.(model).mode != modeList {
		t.Errorf("esc left mode %v", next.(model).mode)
	}
}

// With no store, or one that does not answer, the view says why (the same
// text as the CLI) and shows no table.
func TestInventoryViewWithoutStore(t *testing.T) {
	m, _ := inventoryModel()
	m.readInventory = nil // the real path: m.svc().Inventory, no session
	next, cmd := m.Update(key("I"))
	next, _ = next.(model).Update(cmd())
	out := next.(model).View()
	if !strings.Contains(out, "no store is configured") || strings.Contains(out, "HOSTS") {
		t.Errorf("view:\n%s", out)
	}

	m, _ = inventoryModel()
	m.readInventory = func(context.Context, string, bool) (*app.Inventory, error) {
		return nil, errors.New("inventory: the store is not reachable")
	}
	next, cmd = m.Update(key("I"))
	next, _ = next.(model).Update(cmd())
	if out := next.(model).View(); !strings.Contains(out, "the store is not reachable") {
		t.Errorf("view:\n%s", out)
	}
}
