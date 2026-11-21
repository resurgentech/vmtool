package server

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// V5: the daemon's primitives for networks that span hosts, through the real
// handler chain from the remote client: the member form of createNetwork, the
// peers route (set and delete) and the underlay lookup. The daemon never calls
// the store for any of them.

func (f *rtMgr) CreateNetworkMember(name string, vni uint32) (*vmtool.NetworkInfo, error) {
	f.rec("CreateNetworkMember %s %d", name, vni)
	if name == "taken" {
		return nil, fmt.Errorf("a libvirt network named %q already exists: %w", name, vmtool.ErrNetworkExists)
	}
	return &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni), VNI: vni, Vmtool: true, MTU: 1450, Active: true, Autostart: true, VMs: []string{}}, nil
}

func (f *rtMgr) SetNetworkLinks(name, ip, dev string, peers []string) error {
	f.rec("SetNetworkLinks %s %s %s %s", name, ip, dev, strings.Join(peers, ","))
	switch {
	case name == "ghost":
		return fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
	case name == "default":
		return fmt.Errorf("network %q: %w", name, vmtool.ErrNetworkNotVmtool)
	case ip == "192.0.2.99":
		return fmt.Errorf("%w: invalid local address %q", vmtool.ErrInvalidNetwork, ip)
	}
	return nil
}

func (f *rtMgr) RemoveNetworkLink(name string) error {
	f.rec("RemoveNetworkLink %s", name)
	switch name {
	case "ghost":
		return fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
	case "default":
		return fmt.Errorf("network %q: %w", name, vmtool.ErrNetworkNotVmtool)
	}
	return nil
}

func (f *rtMgr) Underlay(to string) (*vmtool.Underlay, error) {
	f.rec("Underlay %s", to)
	switch to {
	case "192.0.2.99":
		return nil, fmt.Errorf("%w: the address is this host's own (device lo)", vmtool.ErrUnderlay)
	case "192.0.2.98":
		return nil, fmt.Errorf("%w: invalid address %q", vmtool.ErrInvalidNetwork, to)
	}
	return &vmtool.Underlay{IP: "192.0.2.11", Dev: "eth-test"}, nil
}

func TestMemberNetworkRoutesRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)

	n, err := m.CreateNetworkMember("net1", 10001)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "net1" || n.Bridge != "vmt-002711" || n.VNI != 10001 || !n.IsMemberForm() || n.MTU != 1450 {
		t.Errorf("member = %+v", n)
	}
	if err := m.SetNetworkLinks("net1", "192.0.2.11", "eth-test", []string{"192.0.2.12", "192.0.2.13"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetNetworkLinks("net1", "192.0.2.11", "eth-test", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveNetworkLink("net1"); err != nil {
		t.Fatal(err)
	}
	u, err := m.Underlay("192.0.2.13")
	if err != nil || *u != (vmtool.Underlay{IP: "192.0.2.11", Dev: "eth-test"}) {
		t.Errorf("Underlay = %+v, %v", u, err)
	}
	want := []string{
		"CreateNetworkMember net1 10001",
		"SetNetworkLinks net1 192.0.2.11 eth-test 192.0.2.12,192.0.2.13",
		"SetNetworkLinks net1 192.0.2.11 eth-test ",
		"RemoveNetworkLink net1",
		"Underlay 192.0.2.13",
	}
	if g := fm.got(); !reflect.DeepEqual(g, want) {
		t.Errorf("calls %q, want %q", g, want)
	}
}

func TestMemberNetworkRouteStatuses(t *testing.T) {
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
	if _, err := m.CreateNetworkMember("taken", 10001); err == nil || kind(err) != app.KindConflict {
		t.Errorf("member of a taken name: %v", err)
	}
	fm.reset()
	for _, bad := range []string{"default", "a b"} {
		if _, err := m.CreateNetworkMember(bad, 10001); err == nil || kind(err) != app.KindInvalid {
			t.Errorf("member %q: %v", bad, err)
		}
	}
	if _, err := m.CreateNetworkMember("net1", 1<<24); err == nil || kind(err) != app.KindInvalid {
		t.Errorf("vni 1<<24: %v", err)
	}
	if g := fm.got(); len(g) != 0 {
		t.Errorf("a refused member reached the host: %q", g)
	}
	if err := m.SetNetworkLinks("ghost", "192.0.2.11", "eth-test", nil); err == nil || kind(err) != app.KindNotFound {
		t.Errorf("peers of a ghost: %v", err)
	}
	if err := m.SetNetworkLinks("default", "192.0.2.11", "eth-test", nil); err == nil || kind(err) != app.KindConflict {
		t.Errorf("peers of the default network: %v", err)
	}
	if err := m.SetNetworkLinks("net1", "192.0.2.99", "eth-test", nil); err == nil || kind(err) != app.KindInvalid {
		t.Errorf("a bad local address: %v", err)
	}
	if err := m.SetNetworkLinks("net1", "", "eth-test", nil); err == nil || kind(err) != app.KindInvalid {
		t.Errorf("an empty local address: %v", err)
	}
	if err := m.RemoveNetworkLink("ghost"); err == nil || kind(err) != app.KindNotFound {
		t.Errorf("unlink of a ghost: %v", err)
	}
	if err := m.RemoveNetworkLink("default"); err == nil || kind(err) != app.KindConflict {
		t.Errorf("unlink of the default network: %v", err)
	}
	if _, err := m.Underlay("192.0.2.99"); err == nil || kind(err) != app.KindConflict {
		t.Errorf("underlay of the host's own address: %v", err)
	}
	if _, err := m.Underlay("192.0.2.98"); err == nil || kind(err) != app.KindInvalid {
		t.Errorf("underlay of a bad address: %v", err)
	}
}

// createNetwork's member form: member needs vni and refuses subnet and domain
// (400, never reaching the host); without member nothing changes.
func TestCreateNetworkMemberFormIsChecked(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, _, _ := hypervisor(t, fm, testToken)
	post := func(body string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/hosts/veritech01/networks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(`{"name":"net1","member":true}`); c != 400 {
		t.Errorf("member without vni = %d, want 400", c)
	}
	if c := post(`{"name":"net1","member":true,"vni":10001,"subnet":"10.77.1.0/24"}`); c != 400 {
		t.Errorf("member with subnet = %d, want 400", c)
	}
	if c := post(`{"name":"net1","member":true,"vni":10001,"domain":"lab.example"}`); c != 400 {
		t.Errorf("member with domain = %d, want 400", c)
	}
	if g := fm.got(); len(g) != 0 {
		t.Errorf("a refused member reached the host: %q", g)
	}
	if c := post(`{"name":"net1","member":true,"vni":10001}`); c != 201 {
		t.Errorf("member = %d, want 201", c)
	}
	if c := post(`{"name":"net2","member":false,"vni":10002}`); c != 201 {
		t.Errorf("member false = %d, want 201", c)
	}
	if g, want := fm.got(), []string{"CreateNetworkMember net1 10001", "CreateNetworkVNI net2   10002"}; !reflect.DeepEqual(g, want) {
		t.Errorf("calls %q, want %q", g, want)
	}
}

func TestMemberNetworkRoutesNeedBearer(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, pool, _ := hypervisor(t, newRTMgr(t), testToken)
	m := remoteFor(t, srv, pool, "not-the-token")
	if _, err := m.CreateNetworkMember("net1", 10001); err == nil {
		t.Error("createNetwork member accepted a wrong token")
	}
	if err := m.SetNetworkLinks("net1", "192.0.2.11", "eth-test", nil); err == nil {
		t.Error("setNetworkPeers accepted a wrong token")
	}
	if err := m.RemoveNetworkLink("net1"); err == nil {
		t.Error("deleteNetworkPeers accepted a wrong token")
	}
	if _, err := m.Underlay("192.0.2.13"); err == nil {
		t.Error("getUnderlay accepted a wrong token")
	}
}
