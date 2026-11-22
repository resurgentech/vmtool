package app

import (
	"context"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/storeclient"
)

// V5: a card on a network that spans hosts. The reservation is made on the
// network's home (createReservation, which picks the MAC and the address), then
// the card is attached on the VM's host with that MAC; a failure undoes the
// other; delete and detach remove the reservation on the home. The store keeps
// a record of each reservation.

var spanRef = VMRef{Host: "hv1", Name: "vm1"}

// memberEnv is a registry, a home with net1 and hv1 joined to it.
func memberEnv(t *testing.T) *spanEnv {
	t.Helper()
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	if _, err := e.s.JoinNetwork(context.Background(), "hv1", "net1", ""); err != nil {
		t.Fatal(err)
	}
	for _, h := range e.hosts {
		h.order = nil
	}
	e.opened = nil
	return e
}

func (e *spanEnv) recorded(t *testing.T, network string) []storeclient.NetworkReservation {
	t.Helper()
	rs, err := e.cl.NetworkReservations(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestAttachOnAMemberReservesOnTheHomeAndAttachesOnTheMember(t *testing.T) {
	e := memberEnv(t)
	att, err := e.s.AttachNetwork(context.Background(), e.hosts["hv1"], spanRef, "net1", "")
	if err != nil {
		t.Fatal(err)
	}
	if att.Network != "net1" || att.MAC != "52:54:00:00:00:01" || att.IP != "10.77.0.100" {
		t.Errorf("attachment = %+v, want the home's MAC and address", att)
	}
	home, hv1 := e.hosts["home1"], e.hosts["hv1"]
	if home.index("AddReservation net1 vm1") < 0 {
		t.Errorf("the home was not asked for the reservation:\n%s", home.orderText())
	}
	if hv1.index("AddReservation") >= 0 || hv1.index("AttachInterface vm1 net1 52:54:00:00:00:01") < 0 {
		t.Errorf("hv1 must attach the card with the home's MAC and make no reservation:\n%s", hv1.orderText())
	}
	if home.reservations("net1") != 1 || len(hv1.cards) != 1 {
		t.Errorf("home reservations %d, hv1 cards %d", home.reservations("net1"), len(hv1.cards))
	}
	rs := e.recorded(t, "net1")
	if len(rs) != 1 || rs[0].Mac != "52:54:00:00:00:01" || rs[0].Ip != "10.77.0.100" || rs[0].Host != "hv1" || rs[0].Vm != "vm1" {
		t.Errorf("registry records %+v", rs)
	}
}

func TestAttachOnAMemberAsksForTheAddress(t *testing.T) {
	e := memberEnv(t)
	att, err := e.s.AttachNetwork(context.Background(), e.hosts["hv1"], spanRef, "net1", "10.77.0.50")
	if err != nil || att.IP != "10.77.0.50" {
		t.Fatalf("%+v, %v", att, err)
	}
	// The home's refusal of a taken address comes back, before any card.
	_, err = e.s.AttachNetwork(context.Background(), e.hosts["hv1"], VMRef{Host: "hv1", Name: "vm2"}, "net1", "10.77.0.50")
	if err == nil || kindOfErr(t, err) != KindConflict {
		t.Fatalf("taken address: %v", err)
	}
	if len(e.hosts["hv1"].cards) != 1 {
		t.Errorf("a card was attached for the refused address")
	}
}

func TestAttachOnAMemberUndoesTheReservationWhenTheCardFails(t *testing.T) {
	e := memberEnv(t)
	hv1 := e.hosts["hv1"]
	hv1.failAt = "AttachInterface"
	_, err := e.s.AttachNetwork(context.Background(), hv1, spanRef, "net1", "")
	if err == nil || !strings.Contains(err.Error(), "AttachInterface failed") {
		t.Fatalf("err = %v", err)
	}
	if got := e.hosts["home1"].reservations("net1"); got != 0 {
		t.Errorf("the home keeps %d reservation(s) with no card", got)
	}
	if rs := e.recorded(t, "net1"); len(rs) != 0 {
		t.Errorf("registry records %+v for a card that was not made", rs)
	}
}

func TestAttachOnAMemberUndoesTheCardWhenTheHomeFails(t *testing.T) {
	e := memberEnv(t)
	e.hosts["home1"].failAt = "AddReservation"
	_, err := e.s.AttachNetwork(context.Background(), e.hosts["hv1"], spanRef, "net1", "")
	if err == nil || !strings.Contains(err.Error(), "AddReservation failed") {
		t.Fatalf("err = %v", err)
	}
	if len(e.hosts["hv1"].cards) != 0 || e.hosts["hv1"].index("AttachInterface") >= 0 {
		t.Errorf("a card was attached without a reservation:\n%s", e.hosts["hv1"].orderText())
	}
}

func TestAttachOnAMemberNeedsTheHome(t *testing.T) {
	e := memberEnv(t)
	e.down["home1"] = true
	_, err := e.s.AttachNetwork(context.Background(), e.hosts["hv1"], spanRef, "net1", "")
	if err == nil || kindOfErr(t, err) != KindUnavailable || !strings.Contains(err.Error(), "home1") {
		t.Fatalf("err = %v, want unavailable, naming the home", err)
	}
	if len(e.hosts["hv1"].cards) != 0 {
		t.Error("a card was attached with the home down")
	}
}

func TestAttachOnAMemberInLocalModeSaysItNeedsTheStore(t *testing.T) {
	e := memberEnv(t)
	s := &Service{OpenHost: e.open}
	_, err := s.AttachNetwork(context.Background(), e.hosts["hv1"], spanRef, "net1", "")
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("err = %v, want a message naming the store", err)
	}
	if len(e.hosts["hv1"].cards) != 0 {
		t.Error("a card was attached")
	}
}

func TestDetachOnAMemberRemovesTheCardThenTheHomesReservation(t *testing.T) {
	e := memberEnv(t)
	ctx := context.Background()
	if _, err := e.s.AttachNetwork(ctx, e.hosts["hv1"], spanRef, "net1", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DetachNetwork(ctx, e.hosts["hv1"], spanRef, "net1"); err != nil {
		t.Fatal(err)
	}
	if len(e.hosts["hv1"].cards) != 0 || e.hosts["home1"].reservations("net1") != 0 {
		t.Errorf("cards %d, home reservations %d", len(e.hosts["hv1"].cards), e.hosts["home1"].reservations("net1"))
	}
	if rs := e.recorded(t, "net1"); len(rs) != 0 {
		t.Errorf("registry still records %+v", rs)
	}
}

func TestDetachOnAMemberPutsTheCardBackWhenTheHomeKeepsTheReservation(t *testing.T) {
	e := memberEnv(t)
	ctx := context.Background()
	if _, err := e.s.AttachNetwork(ctx, e.hosts["hv1"], spanRef, "net1", ""); err != nil {
		t.Fatal(err)
	}
	e.hosts["home1"].failAt = "RemoveReservation"
	err := e.s.DetachNetwork(ctx, e.hosts["hv1"], spanRef, "net1")
	if err == nil || !strings.Contains(err.Error(), "RemoveReservation failed") {
		t.Fatalf("err = %v", err)
	}
	if len(e.hosts["hv1"].cards) != 1 {
		t.Errorf("the card was not put back: %d cards", len(e.hosts["hv1"].cards))
	}
	if rs := e.recorded(t, "net1"); len(rs) != 1 {
		t.Errorf("registry records %+v, want the reservation kept", rs)
	}
}

func TestDeleteVMOnAMemberRemovesTheReservationOnTheHome(t *testing.T) {
	e := memberEnv(t)
	ctx := context.Background()
	if _, err := e.s.AttachNetwork(ctx, e.hosts["hv1"], spanRef, "net1", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteVM(ctx, e.hosts["hv1"], spanRef, false); err != nil {
		t.Fatal(err)
	}
	if got := e.hosts["home1"].reservations("net1"); got != 0 {
		t.Errorf("the home keeps %d reservation(s) of a deleted VM", got)
	}
	if rs := e.recorded(t, "net1"); len(rs) != 0 {
		t.Errorf("registry still records %+v", rs)
	}
}

// With the home down the VM is deleted all the same, and the error says which
// reservation is left on which network.
func TestDeleteVMOnAMemberReportsAReservationTheHomeCouldNotRemove(t *testing.T) {
	e := memberEnv(t)
	ctx := context.Background()
	if _, err := e.s.AttachNetwork(ctx, e.hosts["hv1"], spanRef, "net1", ""); err != nil {
		t.Fatal(err)
	}
	e.down["home1"] = true
	err := e.s.DeleteVM(ctx, e.hosts["hv1"], spanRef, false)
	if err == nil || !strings.Contains(err.Error(), "deleted") || !strings.Contains(err.Error(), "52:54:00:00:00:01") || !strings.Contains(err.Error(), "net1") {
		t.Fatalf("err = %v, want the VM deleted and the reservation named", err)
	}
}

// A VM on the home of a registered network: its reservation is on the home as
// ever, and the registry has a record of it.
func TestAttachOnTheHomeRecordsTheReservation(t *testing.T) {
	e := memberEnv(t)
	ctx := context.Background()
	ref := VMRef{Host: "home1", Name: "vm9"}
	if _, err := e.s.AttachNetwork(ctx, e.hosts["home1"], ref, "net1", ""); err != nil {
		t.Fatal(err)
	}
	rs := e.recorded(t, "net1")
	if len(rs) != 1 || rs[0].Host != "home1" || rs[0].Vm != "vm9" {
		t.Fatalf("registry records %+v", rs)
	}
	if err := e.s.DetachNetwork(ctx, e.hosts["home1"], ref, "net1"); err != nil {
		t.Fatal(err)
	}
	if rs := e.recorded(t, "net1"); len(rs) != 0 {
		t.Errorf("registry still records %+v", rs)
	}
}

// A card on a network the registry does not have (made without a store) is
// attached as in V2 and records nothing.
func TestAttachOnAnUnregisteredNetworkRecordsNothing(t *testing.T) {
	e := newSpanEnv(t)
	h := e.hosts["hv1"]
	h.nets = newCardMgr().nets
	if _, err := e.s.AttachNetwork(context.Background(), h, spanRef, "net1", ""); err != nil {
		t.Fatal(err)
	}
	if h.reservations("net1") != 1 || len(h.cards) != 1 {
		t.Errorf("reservations %d, cards %d", h.reservations("net1"), len(h.cards))
	}
}

func TestCreateWithACardOnAMemberNetwork(t *testing.T) {
	e := memberEnv(t)
	t.Chdir(t.TempDir())
	hv1 := e.hosts["hv1"]
	in := CreateVMInput{Host: "hv1", Name: "web", Image: "u.qcow2", NoWait: true, Networks: []NetworkRequest{{Network: "net1"}}}
	info, err := e.s.CreateVM(context.Background(), hv1, in)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || len(info.Networks) != 1 || info.Networks[0].MAC != "52:54:00:00:00:01" {
		t.Fatalf("info = %+v", info)
	}
	if def, att, st := hv1.index("Define"), hv1.index("AttachInterface web net1"), hv1.index("Start"); !(def >= 0 && def < att && att < st) {
		t.Errorf("want Define, the card, then Start:\n%s", hv1.orderText())
	}
	if e.hosts["home1"].index("AddReservation net1 web") < 0 || e.hosts["home1"].reservations("net1") != 1 {
		t.Errorf("the home did not reserve:\n%s", e.hosts["home1"].orderText())
	}
	if rs := e.recorded(t, "net1"); len(rs) != 1 || rs[0].Vm != "web" || rs[0].Host != "hv1" {
		t.Errorf("registry records %+v", rs)
	}
}
