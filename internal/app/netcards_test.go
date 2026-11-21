package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// V2: VMs on a vmtool network. The workflows order a card's steps (reservation,
// then card; detach in reverse) and undo the ones before a failed step, so a
// network never has a reservation without a card nor a card without one.

// cardMgr is a host with networks, reservations and the cards of VMs, over the
// create fake. failAt names the primitive that fails ("AddReservation",
// "AttachInterface", "RemoveReservation", "DetachInterface"); failNth is which
// call of it (1 is the first; 0 is every call).
type cardMgr struct {
	nwVMs
	nets    map[string]*vmtool.NetworkInfo
	cards   []vmtool.NetAttachment // the VM's cards, in order (the NAT card is not here)
	order   []string               // every call, in order, with its arguments
	failAt  string
	failNth int
	count   map[string]int
	macSeq  int
	missing bool // Info answers not found
}

func newCardMgr() *cardMgr {
	m := &cardMgr{nets: map[string]*vmtool.NetworkInfo{}, count: map[string]int{}}
	for i, n := range []string{"net1", "net2"} {
		m.nets[n] = &vmtool.NetworkInfo{
			Name: n, Bridge: fmt.Sprintf("vmt-%06x", i+1), Vmtool: true, VNI: uint32(i + 1),
			Subnet: fmt.Sprintf("10.77.%d.0/24", i+1), Gateway: fmt.Sprintf("10.77.%d.1", i+1),
			DHCPStart: fmt.Sprintf("10.77.%d.100", i+1), DHCPEnd: fmt.Sprintf("10.77.%d.200", i+1),
		}
	}
	m.nets["default"] = &vmtool.NetworkInfo{Name: "default", Bridge: "virbr0", Subnet: "192.168.122.0/24"}
	return m
}

func (m *cardMgr) log(f string, a ...any) { m.order = append(m.order, fmt.Sprintf(f, a...)) }

func (m *cardMgr) fails(op string) error {
	m.count[op]++
	if m.failAt == op && (m.failNth == 0 || m.failNth == m.count[op]) {
		return errors.New(op + " failed")
	}
	return nil
}

func (m *cardMgr) Network(name string) (*vmtool.NetworkInfo, error) {
	m.log("Network %s", name)
	n, ok := m.nets[name]
	if !ok {
		return nil, fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
	}
	c := *n
	return &c, nil
}

func (m *cardMgr) AddReservation(network, vm, mac, ip string) (*vmtool.Reservation, error) {
	m.log("AddReservation %s %s %s", network, vm, ip)
	if err := m.fails("AddReservation"); err != nil {
		return nil, err
	}
	n := m.nets[network]
	if mac == "" {
		m.macSeq++
		mac = fmt.Sprintf("52:54:00:00:00:%02x", m.macSeq)
	}
	res, err := vmtool.PlanReservation(*n, vm, mac, ip, nil)
	if err != nil {
		return nil, err
	}
	n.Reservations = append(n.Reservations, res)
	return &res, nil
}

func (m *cardMgr) RemoveReservation(network, mac string) error {
	m.log("RemoveReservation %s %s", network, mac)
	if err := m.fails("RemoveReservation"); err != nil {
		return err
	}
	n := m.nets[network]
	for i, r := range n.Reservations {
		if r.MAC == mac {
			n.Reservations = append(n.Reservations[:i:i], n.Reservations[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("network %q has no reservation for %s: %w", network, mac, vmtool.ErrNotFound)
}

func (m *cardMgr) ip(network, mac string) string {
	for _, r := range m.nets[network].Reservations {
		if r.MAC == mac {
			return r.IP
		}
	}
	return ""
}

func (m *cardMgr) AttachInterface(vm, network, mac string) (*vmtool.NetAttachment, error) {
	m.log("AttachInterface %s %s %s", vm, network, mac)
	if err := m.fails("AttachInterface"); err != nil {
		return nil, err
	}
	a := vmtool.NetAttachment{Network: network, MAC: mac, IP: m.ip(network, mac)}
	m.cards = append(m.cards, a)
	return &a, nil
}

func (m *cardMgr) DetachInterface(vm, mac string) error {
	m.log("DetachInterface %s %s", vm, mac)
	if err := m.fails("DetachInterface"); err != nil {
		return err
	}
	for i, c := range m.cards {
		if c.MAC == mac {
			m.cards = append(m.cards[:i:i], m.cards[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("VM %q has no card %s: %w", vm, mac, vmtool.ErrNotFound)
}

func (m *cardMgr) Info(name string) (*vmtool.VMInfo, error) {
	m.log("Info %s", name)
	if m.missing {
		return nil, fmt.Errorf("looking up domain %q: %w", name, vmtool.ErrNotFound)
	}
	vi := &vmtool.VMInfo{Name: name, State: vmtool.StateRunning, IP: "192.0.2.12"}
	vi.Networks = append(vi.Networks, m.cards...)
	for i := range vi.Networks {
		vi.Networks[i].IP = m.ip(vi.Networks[i].Network, vi.Networks[i].MAC)
	}
	return vi, nil
}

func (m *cardMgr) Delete(name string, noclone bool) error { m.log("Delete %s", name); return nil }
func (m *cardMgr) Define(vmtool.VMConfig) error           { m.log("Define"); return nil }
func (m *cardMgr) Start(string) error                     { m.log("Start"); return nil }
func (m *cardMgr) CloneImage(b, n, p string, sz uint64) (string, error) {
	m.log("CloneImage")
	return "/pool/" + n + ".qcow2", nil
}
func (m *cardMgr) ResizeVolume(string, uint64) error { return nil }

func (m *cardMgr) reservations(network string) int { return len(m.nets[network].Reservations) }

func (m *cardMgr) orderText() string { return strings.Join(m.order, "\n") }

func (m *cardMgr) index(prefix string) int {
	for i, o := range m.order {
		if strings.HasPrefix(o, prefix) {
			return i
		}
	}
	return -1
}

var cardRef = VMRef{Host: "hv1", Name: "vm1"}

func TestAttachNetworkReservesThenAttaches(t *testing.T) {
	m := newCardMgr()
	att, err := (&Service{}).AttachNetwork(context.Background(), m, cardRef, "net1", "")
	if err != nil {
		t.Fatal(err)
	}
	if att.Network != "net1" || att.IP != "10.77.1.100" || att.MAC != "52:54:00:00:00:01" {
		t.Fatalf("attachment = %+v", att)
	}
	if r, c := m.index("AddReservation net1 vm1"), m.index("AttachInterface vm1 net1 52:54:00:00:00:01"); r < 0 || c < 0 || r > c {
		t.Fatalf("want the reservation before the card (and carrying the VM's name):\n%s", m.orderText())
	}
	if m.reservations("net1") != 1 || len(m.cards) != 1 {
		t.Fatalf("reservations %d, cards %d", m.reservations("net1"), len(m.cards))
	}
}

func TestAttachNetworkAsksForTheAddress(t *testing.T) {
	m := newCardMgr()
	att, err := (&Service{}).AttachNetwork(context.Background(), m, cardRef, "net1", "10.77.1.50")
	if err != nil || att.IP != "10.77.1.50" {
		t.Fatalf("%+v, %v", att, err)
	}
}

// A failed card removes the reservation made before it.
func TestAttachNetworkUndoesTheReservationWhenTheCardFails(t *testing.T) {
	m := newCardMgr()
	m.failAt = "AttachInterface"
	_, err := (&Service{}).AttachNetwork(context.Background(), m, cardRef, "net1", "")
	if err == nil || !strings.Contains(err.Error(), "AttachInterface failed") {
		t.Fatalf("err = %v", err)
	}
	if m.reservations("net1") != 0 || len(m.cards) != 0 {
		t.Fatalf("left behind: %d reservations, %d cards\n%s", m.reservations("net1"), len(m.cards), m.orderText())
	}
	if m.index("RemoveReservation net1 52:54:00:00:00:01") < 0 {
		t.Fatalf("the reservation was not removed:\n%s", m.orderText())
	}
}

func TestAttachNetworkUndoFailureIsReported(t *testing.T) {
	m := newCardMgr()
	m.failAt = "RemoveReservation"
	m2 := &failBoth{cardMgr: m}
	_, err := (&Service{}).AttachNetwork(context.Background(), m2, cardRef, "net1", "")
	if err == nil || !strings.Contains(err.Error(), "AttachInterface failed") || !strings.Contains(err.Error(), "10.77.1.100") || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("err = %v: want both failures, naming the reservation left", err)
	}
}

// failBoth also fails the card, so the undo's own failure shows.
type failBoth struct{ *cardMgr }

func (f *failBoth) AttachInterface(vm, network, mac string) (*vmtool.NetAttachment, error) {
	f.log("AttachInterface %s %s %s", vm, network, mac)
	return nil, errors.New("AttachInterface failed")
}

func TestAttachNetworkFailedReservationAttachesNothing(t *testing.T) {
	m := newCardMgr()
	m.failAt = "AddReservation"
	if _, err := (&Service{}).AttachNetwork(context.Background(), m, cardRef, "net1", ""); err == nil {
		t.Fatal("no error")
	}
	if m.index("AttachInterface") >= 0 || m.index("RemoveReservation") >= 0 {
		t.Fatalf("a card or an undo after a failed reservation:\n%s", m.orderText())
	}
}

func TestAttachNetworkRefusals(t *testing.T) {
	cases := []struct {
		name, network, ip string
		missing           bool
		kind              Kind
	}{
		{"unknown network", "net9", "", false, KindNotFound},
		{"not vmtool's", "default", "", false, KindConflict},
		{"address outside the subnet", "net1", "10.99.0.5", false, KindInvalid},
		{"address of the gateway", "net1", "10.77.1.1", false, KindConflict},
		{"no VM", "net1", "", true, KindNotFound},
		{"empty network", "", "", false, KindInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newCardMgr()
			m.missing = c.missing
			_, err := (&Service{}).AttachNetwork(context.Background(), m, cardRef, c.network, c.ip)
			if err == nil || kindOfErr(t, err) != c.kind {
				t.Fatalf("err = %v, want kind %d", err, c.kind)
			}
			if m.index("AddReservation") >= 0 {
				t.Errorf("the host was asked to reserve before the refusal:\n%s", m.orderText())
			}
		})
	}
}

func TestAttachNetworkRefusesATakenAddress(t *testing.T) {
	m := newCardMgr()
	if _, err := (&Service{}).AttachNetwork(context.Background(), m, VMRef{Host: "hv1", Name: "vm2"}, "net1", "10.77.1.100"); err != nil {
		t.Fatal(err)
	}
	_, err := (&Service{}).AttachNetwork(context.Background(), m, cardRef, "net1", "10.77.1.100")
	if err == nil || kindOfErr(t, err) != KindConflict {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if m.reservations("net1") != 1 {
		t.Fatalf("reservations = %d", m.reservations("net1"))
	}
}

func attached(t *testing.T, m *cardMgr, network string) vmtool.NetAttachment {
	t.Helper()
	a, err := (&Service{}).AttachNetwork(context.Background(), m, cardRef, network, "")
	if err != nil {
		t.Fatal(err)
	}
	m.order = nil
	return *a
}

func TestDetachNetworkRemovesTheCardThenTheReservation(t *testing.T) {
	m := newCardMgr()
	a := attached(t, m, "net1")
	if err := (&Service{}).DetachNetwork(context.Background(), m, cardRef, "net1"); err != nil {
		t.Fatal(err)
	}
	if d, r := m.index("DetachInterface vm1 "+a.MAC), m.index("RemoveReservation net1 "+a.MAC); d < 0 || r < 0 || d > r {
		t.Fatalf("want the card removed before the reservation:\n%s", m.orderText())
	}
	if m.reservations("net1") != 0 || len(m.cards) != 0 {
		t.Fatalf("left: %d reservations, %d cards", m.reservations("net1"), len(m.cards))
	}
}

func TestDetachNetworkByMAC(t *testing.T) {
	m := newCardMgr()
	a := attached(t, m, "net1")
	if err := (&Service{}).DetachNetwork(context.Background(), m, cardRef, strings.ToUpper(a.MAC)); err != nil {
		t.Fatal(err)
	}
	if len(m.cards) != 0 || m.reservations("net1") != 0 {
		t.Fatalf("left: %d reservations, %d cards", m.reservations("net1"), len(m.cards))
	}
}

// A failed reservation removal puts the card back: no reservation without a card
// (the reservation stays, so the card does too).
func TestDetachNetworkRestoresTheCardWhenTheReservationStays(t *testing.T) {
	m := newCardMgr()
	a := attached(t, m, "net1")
	m.failAt, m.count = "RemoveReservation", map[string]int{}
	err := (&Service{}).DetachNetwork(context.Background(), m, cardRef, "net1")
	if err == nil || !strings.Contains(err.Error(), "RemoveReservation failed") {
		t.Fatalf("err = %v", err)
	}
	if len(m.cards) != 1 || m.cards[0].MAC != a.MAC || m.reservations("net1") != 1 {
		t.Fatalf("card %+v, reservations %d: the card was not put back\n%s", m.cards, m.reservations("net1"), m.orderText())
	}
}

func TestDetachNetworkFailedCardLeavesTheReservation(t *testing.T) {
	m := newCardMgr()
	attached(t, m, "net1")
	m.failAt, m.count = "DetachInterface", map[string]int{}
	if err := (&Service{}).DetachNetwork(context.Background(), m, cardRef, "net1"); err == nil {
		t.Fatal("no error")
	}
	if m.index("RemoveReservation") >= 0 || len(m.cards) != 1 || m.reservations("net1") != 1 {
		t.Fatalf("reservation removed after a failed card removal:\n%s", m.orderText())
	}
}

func TestDetachNetworkAReservationAlreadyGoneIsFine(t *testing.T) {
	m := newCardMgr()
	a := attached(t, m, "net1")
	m.nets["net1"].Reservations = nil // someone removed it by hand
	if err := (&Service{}).DetachNetwork(context.Background(), m, cardRef, a.MAC); err != nil {
		t.Fatal(err)
	}
	if len(m.cards) != 0 {
		t.Fatal("card kept")
	}
}

func TestDetachNetworkRefusals(t *testing.T) {
	m := newCardMgr()
	if e := (&Service{}).DetachNetwork(context.Background(), m, cardRef, "net1"); e == nil || kindOfErr(t, e) != KindNotFound {
		t.Fatalf("no card on net1: err = %v, want not found", e)
	}
	attached(t, m, "net1")
	m.AttachInterface("vm1", "net1", "52:54:00:00:00:99") // a second card, by hand
	if e := (&Service{}).DetachNetwork(context.Background(), m, cardRef, "net1"); e == nil || kindOfErr(t, e) != KindInvalid || !strings.Contains(e.Error(), "MAC") {
		t.Fatalf("two cards on net1: err = %v, want a refusal naming the MAC", e)
	}
	if e := (&Service{}).DetachNetwork(context.Background(), m, cardRef, ""); e == nil || kindOfErr(t, e) != KindInvalid {
		t.Fatalf("empty: err = %v", e)
	}
}

func TestNetworkRequestsParse(t *testing.T) {
	got, err := ParseNetworkRequests([]string{"net1", "net2=10.77.2.50"})
	if err != nil || len(got) != 2 || got[0] != (NetworkRequest{Network: "net1"}) || got[1] != (NetworkRequest{Network: "net2", IP: "10.77.2.50"}) {
		t.Fatalf("%+v, %v", got, err)
	}
	for _, bad := range [][]string{{""}, {"=10.0.0.1"}, {"net1="}, {"net1", "net1=10.77.1.9"}} {
		if _, err := ParseNetworkRequests(bad); err == nil || kindOfErr(t, err) != KindInvalid {
			t.Errorf("%v: err = %v, want KindInvalid", bad, err)
		}
	}
}

// runCreate is a no-wait create of "web" on the card fake.
func runCreate(t *testing.T, m *cardMgr, nets ...NetworkRequest) nwRun {
	t.Helper()
	t.Chdir(t.TempDir())
	r := nwRun{}
	in := CreateVMInput{Host: "jetfire", Name: "web", Image: "u.qcow2", NoWait: true, Networks: nets}
	in.OnProgress = func(ev ProgressEvent) { r.evs = append(r.evs, ev) }
	r.info, r.err = (&Service{}).CreateVM(context.Background(), m, in)
	return r
}

func TestCreateAttachesCardsBetweenDefineAndStart(t *testing.T) {
	m := newCardMgr()
	r := runCreate(t, m, NetworkRequest{Network: "net1"}, NetworkRequest{Network: "net2", IP: "10.77.2.50"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	def, st := m.index("Define"), m.index("Start")
	r1, c1 := m.index("AddReservation net1 web"), m.index("AttachInterface web net1")
	r2, c2 := m.index("AddReservation net2 web 10.77.2.50"), m.index("AttachInterface web net2")
	if !(def < r1 && r1 < c1 && c1 < r2 && r2 < c2 && c2 < st) || def < 0 {
		t.Fatalf("want Define, then each network's reservation and card in order, then Start:\n%s", m.orderText())
	}
	if m.reservations("net1") != 1 || m.reservations("net2") != 1 || len(m.cards) != 2 {
		t.Fatalf("reservations %d/%d, cards %d", m.reservations("net1"), m.reservations("net2"), len(m.cards))
	}
	if r.info == nil || len(r.info.Networks) != 2 || r.info.Networks[1].IP != "10.77.2.50" {
		t.Fatalf("info = %+v", r.info)
	}
	var seen bool
	for _, ev := range r.evs {
		if ev.Stage == "network" && ev.Status == "done" {
			seen = true
		}
	}
	if !seen {
		t.Error("no network progress event")
	}
}

// Everything that can be refused is refused before anything is created.
func TestCreateRefusesBadNetworksBeforeCloning(t *testing.T) {
	cases := map[string]struct {
		nets []NetworkRequest
		kind Kind
	}{
		"unknown":       {[]NetworkRequest{{Network: "net9"}}, KindNotFound},
		"not vmtool's":  {[]NetworkRequest{{Network: "default"}}, KindConflict},
		"outside":       {[]NetworkRequest{{Network: "net1", IP: "10.99.0.1"}}, KindInvalid},
		"twice":         {[]NetworkRequest{{Network: "net1"}, {Network: "net1"}}, KindInvalid},
		"empty network": {[]NetworkRequest{{Network: ""}}, KindInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := newCardMgr()
			r := runCreate(t, m, c.nets...)
			if r.err == nil || kindOfErr(t, r.err) != c.kind {
				t.Fatalf("err = %v, want kind %d", r.err, c.kind)
			}
			if m.index("CloneImage") >= 0 || m.index("Define") >= 0 || m.index("AddReservation") >= 0 {
				t.Fatalf("something was created before the refusal:\n%s", m.orderText())
			}
		})
	}
}

// A failure on a later network undoes the earlier cards and their reservations
// and never starts the VM.
func TestCreateUndoesEarlierCardsWhenALaterOneFails(t *testing.T) {
	for _, at := range []string{"AttachInterface", "AddReservation"} {
		t.Run(at, func(t *testing.T) {
			m := newCardMgr()
			m.failAt, m.failNth = at, 2
			r := runCreate(t, m, NetworkRequest{Network: "net1"}, NetworkRequest{Network: "net2"})
			if r.err == nil || !strings.Contains(r.err.Error(), at+" failed") {
				t.Fatalf("err = %v", r.err)
			}
			if m.reservations("net1") != 0 || m.reservations("net2") != 0 || len(m.cards) != 0 {
				t.Fatalf("left behind: reservations %d/%d, cards %d\n%s", m.reservations("net1"), m.reservations("net2"), len(m.cards), m.orderText())
			}
			if m.index("Start") >= 0 {
				t.Fatalf("the VM was started:\n%s", m.orderText())
			}
		})
	}
}

func TestCreateWithoutNetworksMakesNoReservation(t *testing.T) {
	m := newCardMgr()
	if r := runCreate(t, m); r.err != nil {
		t.Fatal(r.err)
	}
	if m.index("AddReservation") >= 0 || m.index("Network ") >= 0 {
		t.Fatalf("network calls without networks:\n%s", m.orderText())
	}
}

func TestDeleteVMRemovesItsReservations(t *testing.T) {
	m := newCardMgr()
	attached(t, m, "net1")
	attached(t, m, "net2")
	if err := (&Service{}).DeleteVM(context.Background(), m, cardRef, false); err != nil {
		t.Fatal(err)
	}
	if m.reservations("net1") != 0 || m.reservations("net2") != 0 {
		t.Fatalf("reservations left: %d, %d\n%s", m.reservations("net1"), m.reservations("net2"), m.orderText())
	}
	if d, r := m.index("Delete vm1"), m.index("RemoveReservation"); d < 0 || r < d {
		t.Fatalf("want the VM deleted, then its reservations:\n%s", m.orderText())
	}
}

func TestDeleteVMReportsAReservationItCouldNotRemove(t *testing.T) {
	m := newCardMgr()
	attached(t, m, "net1")
	m.failAt, m.count = "RemoveReservation", map[string]int{}
	err := (&Service{}).DeleteVM(context.Background(), m, cardRef, false)
	if err == nil || !strings.Contains(err.Error(), "reservation") || !strings.Contains(err.Error(), "net1") {
		t.Fatalf("err = %v, want one naming the reservation and network", err)
	}
	if m.index("Delete vm1") < 0 {
		t.Fatal("the VM was not deleted")
	}
}

func TestDeleteVMIgnoresAReservationAlreadyGone(t *testing.T) {
	m := newCardMgr()
	attached(t, m, "net1")
	m.nets["net1"].Reservations = nil
	if err := (&Service{}).DeleteVM(context.Background(), m, cardRef, false); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteVMWithoutCardsRemovesNothing(t *testing.T) {
	m := newCardMgr()
	if err := (&Service{}).DeleteVM(context.Background(), m, cardRef, true); err != nil {
		t.Fatal(err)
	}
	if m.index("RemoveReservation") >= 0 {
		t.Fatalf("a reservation call for a VM with no cards:\n%s", m.orderText())
	}
}

// The inventory report carries each VM's cards on vmtool networks, and none
// when it has none.
func TestReportVMCarriesNetworks(t *testing.T) {
	v := vmtool.VMInfo{Name: "vm1", UUID: repUUID1, Networks: []vmtool.NetAttachment{{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100"}}}
	rv := reportVM(v, nil)
	if rv.Networks == nil || len(*rv.Networks) != 1 || (*rv.Networks)[0].Network != "net1" || (*rv.Networks)[0].Mac != "52:54:00:00:00:01" || (*rv.Networks)[0].Ip != "10.77.1.100" {
		t.Fatalf("networks = %+v", rv.Networks)
	}
	if rv := reportVM(vmtool.VMInfo{Name: "vm2", UUID: repUUID2}, nil); rv.Networks != nil {
		t.Fatalf("a VM with none reports %+v", rv.Networks)
	}
}
