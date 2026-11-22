package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/store/db"
)

// G3: a reservation left on a home (a VM deleted while the home was down, a
// failed undo) is orphaned only on a positive fact the store holds, and the
// next command that makes, lists or removes a reservation on that network, or
// shows it, with the home answering, removes it on the home and then its
// record. Hosts home1, hv1; network net1; placeholders only.

const (
	orphanUUID1 = "00000000-0000-4000-8000-000000000001"
	orphanUUID2 = "00000000-0000-4000-8000-000000000002"
	orphanMAC1  = "52:54:00:00:00:01"
)

var orphanT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// orphanEnv is memberEnv with the notices the command printed collected.
type orphanEnv struct {
	*spanEnv
	lines []string
	clock *time.Time
}

func newOrphanEnv(t *testing.T) *orphanEnv {
	t.Helper()
	e := &orphanEnv{spanEnv: memberEnv(t)}
	e.s.Store.notify = func(line string) { e.lines = append(e.lines, line) }
	now := orphanT0
	e.clock = &now
	e.db.SetClock(func() time.Time { return *e.clock })
	return e
}

// attach makes vm's card on hv1 with the VM's UUID as hv1 reports it.
func (e *orphanEnv) attach(t *testing.T, vm, uuid string) {
	t.Helper()
	e.hosts["hv1"].uuid = uuid
	if _, err := e.s.AttachNetwork(context.Background(), e.hosts["hv1"], VMRef{Host: "hv1", Name: vm}, "net1", ""); err != nil {
		t.Fatalf("attach %s: %v", vm, err)
	}
}

func (e *orphanEnv) orphans(t *testing.T) []string {
	t.Helper()
	rs, err := e.cl.OrphanedNetworkReservations(context.Background(), "net1")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.Mac)
	}
	return out
}

func (e *orphanEnv) said(sub string) int {
	n := 0
	for _, l := range e.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// The reservation records the VM's UUID from the attach.
func TestAttachRecordsTheVMsUUID(t *testing.T) {
	e := newOrphanEnv(t)
	e.attach(t, "vm1", orphanUUID1)
	rs := e.recorded(t, "net1")
	if len(rs) != 1 || rs[0].Uuid == nil || *rs[0].Uuid != orphanUUID1 {
		t.Fatalf("records = %+v, want the UUID", rs)
	}
}

// Delete with the home down leaves the reservation, marks it, and a later
// attach on that network removes it on the home and then its record.
func TestDeleteWithTheHomeDownThenAnAttachRemovesTheReservation(t *testing.T) {
	e := newOrphanEnv(t)
	ctx := context.Background()
	e.attach(t, "vm1", orphanUUID1)
	e.down["home1"] = true
	if err := e.s.DeleteVM(ctx, e.hosts["hv1"], VMRef{Host: "hv1", Name: "vm1"}, false); err == nil {
		t.Fatal("delete with the home down: want the error naming the reservation left")
	}
	if got := e.orphans(t); len(got) != 1 || got[0] != orphanMAC1 {
		t.Fatalf("orphans after the delete = %v, want the reservation marked", got)
	}
	home := e.hosts["home1"]
	if home.reservations("net1") != 1 {
		t.Fatalf("the home lost the reservation while down: %d", home.reservations("net1"))
	}

	// The home is still down: an attach is refused and nothing changes.
	e.lines = nil
	if _, err := e.s.AttachNetwork(ctx, e.hosts["hv1"], VMRef{Host: "hv1", Name: "vm2"}, "net1", ""); err == nil {
		t.Fatal("attach with the home down: want an error")
	}
	if home.reservations("net1") != 1 || len(e.orphans(t)) != 1 || len(e.lines) != 0 {
		t.Errorf("a silent home changed something: reservations %d, orphans %v, lines %v", home.reservations("net1"), e.orphans(t), e.lines)
	}

	// The home answers again: the next attach removes the orphan first.
	delete(e.down, "home1")
	e.attach(t, "vm2", orphanUUID2)
	if home.reservations("net1") != 1 {
		t.Errorf("the home has %d reservations, want only vm2's", home.reservations("net1"))
	}
	for _, r := range home.nets["net1"].Reservations {
		if r.MAC == orphanMAC1 {
			t.Errorf("the orphaned reservation %s is still on the home", r.MAC)
		}
	}
	rs := e.recorded(t, "net1")
	if len(rs) != 1 || rs[0].Vm != "vm2" || len(e.orphans(t)) != 0 {
		t.Errorf("records %+v, orphans %v; want vm2's only", rs, e.orphans(t))
	}
	if e.said(orphanMAC1) != 1 {
		t.Errorf("want one line for the removed reservation, got %v", e.lines)
	}
	// The order: the home first, then the record.
	if i, j := home.index("RemoveReservation net1 "+orphanMAC1), home.index("AddReservation net1 vm2"); i < 0 || i > j {
		t.Errorf("want the orphan removed before the new reservation:\n%s", home.orderText())
	}
}

// A gone row under 7 days orphans nothing; after the sweep that removes it, the
// next command removes the reservation.
func TestGoneRowOrphansOnlyAfterTheSweep(t *testing.T) {
	e := newOrphanEnv(t)
	ctx := context.Background()
	e.attach(t, "vm1", orphanUUID1)
	row := db.VMRow{UUID: orphanUUID1, Name: "vm1", State: "running", VCPUs: 1, MemoryGiB: 1, IP: "192.0.2.10", Firmware: "efi"}
	if _, err := e.db.ApplyReport(ctx, "hv1", orphanT0, "v", []db.VMRow{row}); err != nil {
		t.Fatal(err)
	}
	*e.clock = orphanT0.Add(time.Minute)
	if _, err := e.db.ApplyReport(ctx, "hv1", *e.clock, "v", nil); err != nil {
		t.Fatal(err)
	}
	gone := *e.clock

	if _, err := e.db.Sweep(ctx, gone.Add(6*24*time.Hour+23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.attach(t, "vm2", orphanUUID2)
	if home := e.hosts["home1"]; home.reservations("net1") != 2 {
		t.Fatalf("a gone row under 7 days removed a reservation: %d left", home.reservations("net1"))
	}

	if _, err := e.db.Sweep(ctx, gone.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// hv1's fake keeps one card list for every VM, so the card is named by MAC.
	if err := e.s.DetachNetwork(ctx, e.hosts["hv1"], VMRef{Host: "hv1", Name: "vm2"}, "52:54:00:00:00:02"); err != nil {
		t.Fatal(err)
	}
	if home := e.hosts["home1"]; home.reservations("net1") != 0 {
		t.Errorf("after the sweep and a detach the home keeps %d reservation(s)", home.reservations("net1"))
	}
	if rs := e.recorded(t, "net1"); len(rs) != 0 {
		t.Errorf("records left: %+v", rs)
	}
}

// A VM with no inventory row yet (a new VM before its first report) is never
// orphaned, however long the store sweeps.
func TestAVMWithNoRowYetIsNeverOrphaned(t *testing.T) {
	e := newOrphanEnv(t)
	ctx := context.Background()
	e.attach(t, "vm1", orphanUUID1)
	if n, err := e.db.Sweep(ctx, orphanT0.Add(400*24*time.Hour)); err != nil || n.Orphaned != 0 {
		t.Fatalf("sweep = %+v, %v", n, err)
	}
	e.attach(t, "vm2", orphanUUID2)
	if home := e.hosts["home1"]; home.reservations("net1") != 2 {
		t.Errorf("the home has %d reservations, want both", home.reservations("net1"))
	}
	if len(e.orphans(t)) != 0 {
		t.Errorf("orphans = %v", e.orphans(t))
	}
}

// A failed undo marks the reservation it could not remove.
func TestAFailedUndoMarksTheReservation(t *testing.T) {
	e := newOrphanEnv(t)
	e.hosts["hv1"].uuid = orphanUUID1
	e.hosts["hv1"].failAt = "AttachInterface"
	e.hosts["home1"].failAt = "RemoveReservation"
	_, err := e.s.AttachNetwork(context.Background(), e.hosts["hv1"], VMRef{Host: "hv1", Name: "vm1"}, "net1", "")
	if err == nil || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("err = %v", err)
	}
	if got := e.orphans(t); len(got) != 1 || got[0] != orphanMAC1 {
		t.Fatalf("orphans = %v, want the reservation the undo left", got)
	}
	rs, _ := e.cl.OrphanedNetworkReservations(context.Background(), "net1")
	if len(rs) != 1 || rs[0].Vm != "vm1" || rs[0].Uuid == nil || *rs[0].Uuid != orphanUUID1 {
		t.Errorf("record = %+v", rs)
	}
	// With the home well again, the next command cleans it.
	e.hosts["home1"].failAt, e.hosts["hv1"].failAt = "", ""
	e.attach(t, "vm2", orphanUUID2)
	if e.hosts["home1"].reservations("net1") != 1 || len(e.orphans(t)) != 0 {
		t.Errorf("the orphan was not cleaned: home %d, orphans %v", e.hosts["home1"].reservations("net1"), e.orphans(t))
	}
}

// net show cleans when the home answers, and lists what it could not.
func TestShowNetworkCleansOrphansOrListsThem(t *testing.T) {
	e := newOrphanEnv(t)
	ctx := context.Background()
	e.attach(t, "vm1", orphanUUID1)
	if err := e.cl.MarkNetworkReservationOrphaned(ctx, "net1", orphanMAC1); err != nil {
		t.Fatal(err)
	}
	hv1 := e.hosts["hv1"]

	e.down["home1"] = true
	got, err := e.s.ShowNetwork(ctx, "hv1", hv1, "net1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orphans) != 1 || got.Orphans[0].Mac != orphanMAC1 || e.hosts["home1"].reservations("net1") != 1 {
		t.Fatalf("with the home down: orphans %+v, home reservations %d; want it listed and nothing changed", got.Orphans, e.hosts["home1"].reservations("net1"))
	}

	delete(e.down, "home1")
	got, err = e.s.ShowNetwork(ctx, "hv1", hv1, "net1")
	if err != nil || len(got.Orphans) != 0 || e.hosts["home1"].reservations("net1") != 0 || len(e.recorded(t, "net1")) != 0 {
		t.Fatalf("with the home up: %+v, %v, home reservations %d", got, err, e.hosts["home1"].reservations("net1"))
	}
	if e.said(orphanMAC1) != 1 {
		t.Errorf("want one line, got %v", e.lines)
	}
}

// A create with --network on a network with an orphan removes it first, so its
// address is free for the new card.
func TestCreateWithANetworkRemovesOrphansFirst(t *testing.T) {
	e := newOrphanEnv(t)
	ctx := context.Background()
	t.Chdir(t.TempDir())
	e.attach(t, "vm1", orphanUUID1)
	if err := e.cl.MarkNetworkReservationOrphaned(ctx, "net1", orphanMAC1); err != nil {
		t.Fatal(err)
	}
	in := CreateVMInput{Host: "hv1", Name: "web", Image: "u.qcow2", NoWait: true, Networks: []NetworkRequest{{Network: "net1", IP: "10.77.0.100"}}}
	if _, err := e.s.CreateVM(ctx, e.hosts["hv1"], in); err != nil {
		t.Fatalf("create asking for the orphan's address: %v", err)
	}
	if rs := e.recorded(t, "net1"); len(rs) != 1 || rs[0].Vm != "web" {
		t.Errorf("records = %+v, want web's only", rs)
	}
}

// A one-host network the registry has: the home is the host itself.
func TestOrphansOnTheHomeItself(t *testing.T) {
	e := newOrphanEnv(t)
	ctx := context.Background()
	home := e.hosts["home1"]
	home.uuid = orphanUUID1
	if _, err := e.s.AttachNetwork(ctx, home, VMRef{Host: "home1", Name: "vm9"}, "net1", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.cl.MarkNetworkReservationOrphaned(ctx, "net1", orphanMAC1); err != nil {
		t.Fatal(err)
	}
	home.uuid = orphanUUID2
	if _, err := e.s.AttachNetwork(ctx, home, VMRef{Host: "home1", Name: "vm8"}, "net1", ""); err != nil {
		t.Fatal(err)
	}
	if rs := e.recorded(t, "net1"); len(rs) != 1 || rs[0].Vm != "vm8" {
		t.Errorf("records = %+v", rs)
	}
}
