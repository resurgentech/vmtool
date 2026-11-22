package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V2: the card primitives through the real handler chain, from the remote
// client, and VMInfo.networks both ways.

func TestCardRoutesRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)

	res, err := m.AddReservation("net1", "vm1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := (vmtool.Reservation{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100", Name: "vm1"}); *res != want {
		t.Errorf("AddReservation = %+v, want %+v", *res, want)
	}
	if res, err = m.AddReservation("net1", "vm2", "52:54:00:00:00:09", "10.77.1.50"); err != nil || res.IP != "10.77.1.50" || res.MAC != "52:54:00:00:00:09" {
		t.Errorf("AddReservation with a MAC and an address = %+v, %v", res, err)
	}
	att, err := m.AttachInterface("vm1", "net1", "52:54:00:00:00:01")
	if err != nil || *att != (vmtool.NetAttachment{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100"}) {
		t.Errorf("AttachInterface = %+v, %v", att, err)
	}
	if err := m.DetachInterface("vm1", "52:54:00:00:00:01"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveReservation("net1", "52:54:00:00:00:01"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"AddReservation net1 vm1  ",
		"AddReservation net1 vm2 52:54:00:00:00:09 10.77.1.50",
		"AttachInterface vm1 net1 52:54:00:00:00:01",
		"DetachInterface vm1 52:54:00:00:00:01",
		"RemoveReservation net1 52:54:00:00:00:01",
	}
	if g := fm.got(); !reflect.DeepEqual(g, want) {
		t.Errorf("calls %q, want %q", g, want)
	}
}

func TestCardRouteStatuses(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)
	kind := func(err error) app.Kind {
		t.Helper()
		var ae *app.Error
		if !app.AsError(err, &ae) {
			t.Fatalf("err = %v, not an *app.Error", err)
		}
		return ae.Kind
	}
	for _, c := range []struct {
		name    string
		network string
		kind    app.Kind
	}{{"unknown", "ghost", app.KindNotFound}, {"not vmtool's", "default", app.KindConflict},
		{"taken", "taken", app.KindConflict}, {"outside the subnet", "outside", app.KindInvalid}} {
		if _, err := m.AddReservation(c.network, "vm1", "", ""); err == nil || kind(err) != c.kind {
			t.Errorf("reserve, %s: %v", c.name, err)
		}
		if c.network != "taken" && c.network != "outside" {
			if _, err := m.AttachInterface("vm1", c.network, "52:54:00:00:00:01"); err == nil || kind(err) != c.kind {
				t.Errorf("attach, %s: %v", c.name, err)
			}
		}
	}
	if err := m.RemoveReservation("ghost", "52:54:00:00:00:01"); err == nil || kind(err) != app.KindNotFound {
		t.Errorf("unreserve a ghost: %v", err)
	}
	if err := m.DetachInterface("ghost", "52:54:00:00:00:01"); err == nil || kind(err) != app.KindNotFound {
		t.Errorf("detach from a ghost: %v", err)
	}
	if _, err := m.AttachInterface("ghost", "net1", "52:54:00:00:00:01"); err == nil || kind(err) != app.KindNotFound {
		t.Errorf("attach to a ghost: %v", err)
	}
	// a bad request is refused before the host is asked
	fm.reset()
	if _, err := m.AttachInterface("vm1", "", ""); err == nil || kind(err) != app.KindInvalid {
		t.Errorf("attach with no network: %v", err)
	}
	if g := fm.got(); len(g) != 0 {
		t.Errorf("a bad request reached the host: %q", g)
	}
}

func TestCardRoutesNeedBearer(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, pool, _ := hypervisor(t, newRTMgr(t), testToken)
	m := remoteFor(t, srv, pool, "not-the-token")
	if _, err := m.AddReservation("net1", "vm1", "", ""); err == nil {
		t.Error("a wrong token was accepted by createReservation")
	}
	if _, err := m.AttachInterface("vm1", "net1", "52:54:00:00:00:01"); err == nil {
		t.Error("a wrong token was accepted by attachInterface")
	}
}

// VMInfo.networks survives the daemon and the client; a VM with none has none.
func TestVMInfoNetworksRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	fm.info.IP = "192.0.2.12"
	fm.info.Networks = []vmtool.NetAttachment{
		{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100"},
		{Network: "net2", MAC: "52:54:00:00:00:02", IP: ""},
	}
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)
	got, err := m.Info("vm1")
	if err != nil {
		t.Fatal(err)
	}
	if got.IP != "192.0.2.12" || !reflect.DeepEqual(got.Networks, fm.info.Networks) {
		t.Fatalf("Info = ip %q networks %+v", got.IP, got.Networks)
	}
	fm.info.Networks = nil
	if got, err = m.Info("vm1"); err != nil || len(got.Networks) != 0 {
		t.Fatalf("Info with none = %+v, %v", got.Networks, err)
	}
}

// createVM's networks field reaches the workflow as requests, in order.
func TestCreateVMBodyCarriesNetworks(t *testing.T) {
	fm := &fakeMgr{}
	h := createServer(t, vmtoolNets{fm}, &app.Service{})
	body := `{"name":"web","image":"u.qcow2","no_wait":true,"networks":[{"network":"net1"},{"network":"net2","ip":"10.77.1.50"}]}`
	rec := do(h, "POST", "/v1/hosts/box/vms", body)
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	i1 := indexOf(fm.calls, "AddReservation net1 web  ")
	i2 := indexOf(fm.calls, "AddReservation net2 web  10.77.1.50")
	st := indexOf(fm.calls, "Start web")
	if i1 < 0 || i2 < i1 || st < i2 {
		t.Fatalf("want net1's reservation, net2's with its address, then Start:\n%s", strings.Join(fm.calls, "\n"))
	}
}

func TestCreateVMBodyRefusesAnUnknownNetworkBeforeCreating(t *testing.T) {
	fm := &fakeMgr{}
	h := createServer(t, vmtoolNets{fm}, &app.Service{})
	rec := do(h, "POST", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2","no_wait":true,"networks":[{"network":"ghost"}]}`)
	if rec.Code != 404 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if indexOf(fm.calls, "Define web") >= 0 || indexOf(fm.calls, "AddReservation ghost web  ") >= 0 {
		t.Fatalf("something was created before the refusal:\n%s", strings.Join(fm.calls, "\n"))
	}
}

// vmtoolNets answers Network with a vmtool network (and not found for "ghost").
type vmtoolNets struct{ *fakeMgr }

func (m vmtoolNets) Network(n string) (*vmtool.NetworkInfo, error) {
	m.rec("Network %s", n)
	if n == "ghost" {
		return nil, fmt.Errorf("network %q: %w", n, vmtool.ErrNotFound)
	}
	return &vmtool.NetworkInfo{Name: n, Bridge: "vmt-000001", Vmtool: true, VNI: 1, Subnet: "10.77.1.0/24", Gateway: "10.77.1.1",
		DHCPStart: "10.77.1.100", DHCPEnd: "10.77.1.200"}, nil
}

func indexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}
