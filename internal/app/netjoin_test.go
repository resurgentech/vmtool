package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// V5: networks that span hosts. The registry is the real store (httptest); the
// hosts are fakes that keep their libvirt networks, links and the calls made.
// Placeholders: hosts home1, hv1, hv2; LAN 192.0.2.13, .11, .12; eth-test.

// netHost is a host: a libvirt with networks (the card fake's), the member
// primitives and its own underlay.
type netHost struct {
	*cardMgr
	name     string
	ip, dev  string            // this host's own underlay
	asked    []string          // addresses Underlay was asked about
	links    map[string]string // network -> "ip dev peer,peer" last set
	linkLog  []string
	unlinked []string
	deleted  []string
	oldHost  bool  // makes a home-form network when asked for a member
	linkErr  error // SetNetworkLinks fails
	delErr   error // DeleteNetwork fails
	createVN []string
}

func newNetHost(name, ip string) *netHost {
	c := newCardMgr()
	c.nets = map[string]*vmtool.NetworkInfo{"default": {Name: "default", Bridge: "virbr0", Subnet: "192.168.122.0/24"}}
	return &netHost{cardMgr: c, name: name, ip: ip, dev: "eth-test", links: map[string]string{}}
}

func (h *netHost) Underlay(to string) (*vmtool.Underlay, error) {
	h.asked = append(h.asked, to)
	return &vmtool.Underlay{IP: h.ip, Dev: h.dev}, nil
}

func (h *netHost) CreateNetworkMember(name string, vni uint32) (*vmtool.NetworkInfo, error) {
	h.createVN = append(h.createVN, fmt.Sprintf("member %s %d", name, vni))
	n := &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni), VNI: vni, Vmtool: true, MTU: 1450, Active: true, Autostart: true, VMs: []string{}}
	if h.oldHost { // a daemon that ignores member: a home-form network
		n.Forward, n.Gateway, n.Subnet = "nat", "10.77.9.1", "10.77.9.0/24"
	}
	h.nets[name] = n
	c := *n
	return &c, nil
}

func (h *netHost) CreateNetworkVNI(name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	n := &vmtool.NetworkInfo{
		Name: name, Bridge: vmtool.BridgeName(vni), VNI: vni, Vmtool: true, Forward: "nat", Subnet: subnet, Domain: domain, MTU: 1450,
		Gateway: strings.TrimSuffix(subnet, "0/24") + "1", DHCPStart: strings.TrimSuffix(subnet, "0/24") + "100", DHCPEnd: strings.TrimSuffix(subnet, "0/24") + "200",
		Active: true, VMs: []string{},
	}
	h.nets[name] = n
	c := *n
	return &c, nil
}

func (h *netHost) SetNetworkLinks(name, ip, dev string, peers []string) error {
	h.linkLog = append(h.linkLog, name+" "+ip+" "+dev+" "+strings.Join(peers, ","))
	if h.linkErr != nil {
		return h.linkErr
	}
	if _, ok := h.nets[name]; !ok {
		return fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
	}
	h.links[name] = ip + " " + dev + " " + strings.Join(peers, ",")
	return nil
}

func (h *netHost) RemoveNetworkLink(name string) error {
	h.unlinked = append(h.unlinked, name)
	delete(h.links, name)
	return nil
}

func (h *netHost) DeleteNetwork(name string) error {
	h.deleted = append(h.deleted, name)
	if h.delErr != nil {
		return h.delErr
	}
	n, ok := h.nets[name]
	if !ok {
		return fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
	}
	if len(n.VMs) > 0 {
		return fmt.Errorf("%w: network %q has VMs attached", vmtool.ErrNetworkInUse, name)
	}
	delete(h.nets, name)
	return nil
}

// spanEnv is a registry and three hosts.
type spanEnv struct {
	s      *Service
	cl     *storeclient.Client
	hosts  map[string]*netHost
	down   map[string]bool
	opened []string
	mu     sync.Mutex // open is called by the concurrent peer pushes
}

func newSpanEnv(t *testing.T) *spanEnv {
	t.Helper()
	sess, cl := realStoreSession(t)
	e := &spanEnv{cl: cl, down: map[string]bool{}, hosts: map[string]*netHost{
		"home1": newNetHost("home1", "192.0.2.13"),
		"hv1":   newNetHost("hv1", "192.0.2.11"),
		"hv2":   newNetHost("hv2", "192.0.2.12"),
	}}
	e.s = &Service{Store: sess, OpenHost: e.open, HostAddr: func(h string) string {
		if m, ok := e.hosts[h]; ok {
			return m.ip
		}
		return ""
	}}
	return e
}

func (e *spanEnv) open(host string) (Manager, func(), error) {
	e.mu.Lock()
	e.opened = append(e.opened, host)
	h, ok := e.hosts[host]
	gone := e.down[host]
	e.mu.Unlock()
	if !ok || gone {
		return nil, nil, fmt.Errorf("host %q does not answer", host)
	}
	return h, func() {}, nil
}

// makeNet registers and defines net1 on home1 (store mode).
func (e *spanEnv) makeNet(t *testing.T, name string) {
	t.Helper()
	if _, err := e.s.CreateNetworkOnHost(context.Background(), "home1", e.hosts["home1"], name, "", ""); err != nil {
		t.Fatal(err)
	}
}

func (e *spanEnv) members(t *testing.T, name string) map[string]string {
	t.Helper()
	n, err := e.cl.GetNetwork(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range n.Members {
		out[m.Host] = m.LanIp + " " + m.LanDev
	}
	return out
}

func TestJoinNetworkLinksTheMemberToTheHome(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	ctx := context.Background()

	res, err := e.s.JoinNetwork(ctx, "hv1", "net1", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.VNI != 10000 || res.Home != "home1" || res.LanIP != "192.0.2.11" || res.LanDev != "eth-test" || res.Registered || len(res.Skipped) != 0 {
		t.Errorf("result = %+v", res)
	}
	hv1 := e.hosts["hv1"]
	// The member defined the bridge-only network of the registry's VNI.
	if got := hv1.nets["net1"]; got == nil || got.Bridge != "vmt-002710" || !got.IsMemberForm() {
		t.Fatalf("hv1's net1 = %+v", got)
	}
	// Its underlay is the address that reaches the home's configured address.
	if len(hv1.asked) != 1 || hv1.asked[0] != "192.0.2.13" {
		t.Errorf("hv1 asked for the route to %v, want the home's address 192.0.2.13", hv1.asked)
	}
	// The home had no row yet: it looked for its own underlay toward the member.
	if h := e.hosts["home1"]; len(h.asked) != 1 || h.asked[0] != "192.0.2.11" {
		t.Errorf("home1 asked for the route to %v, want hv1's LAN address 192.0.2.11", h.asked)
	}
	if got, want := e.members(t, "net1"), (map[string]string{"hv1": "192.0.2.11 eth-test", "home1": "192.0.2.13 eth-test"}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("registry members = %v, want %v", got, want)
	}
	// Both ends are linked to each other, and nothing else.
	if got := hv1.links["net1"]; got != "192.0.2.11 eth-test 192.0.2.13" {
		t.Errorf("hv1 link = %q", got)
	}
	if got := e.hosts["home1"].links["net1"]; got != "192.0.2.13 eth-test 192.0.2.11" {
		t.Errorf("home1 link = %q (the home gets the new peer list)", got)
	}
}

func TestJoinNetworkTellsEveryAwakeMemberAndSkipsASleepingOne(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	ctx := context.Background()
	if _, err := e.s.JoinNetwork(ctx, "hv1", "net1", ""); err != nil {
		t.Fatal(err)
	}
	e.hosts["hv1"].linkLog, e.hosts["home1"].linkLog = nil, nil
	e.hosts["hv1"].asked, e.hosts["home1"].asked = nil, nil

	res, err := e.s.JoinNetwork(ctx, "hv2", "net1", "")
	if err != nil {
		t.Fatal(err)
	}
	hv2 := e.hosts["hv2"]
	// With the home's row in the registry its LAN address is what hv2 asks the
	// route to (not the configured one), and the home is not asked again.
	if len(hv2.asked) != 1 || hv2.asked[0] != "192.0.2.13" || len(e.hosts["home1"].asked) != 0 {
		t.Errorf("hv2 asked %v, home1 asked %v", hv2.asked, e.hosts["home1"].asked)
	}
	if got := hv2.links["net1"]; got != "192.0.2.12 eth-test 192.0.2.11,192.0.2.13" {
		t.Errorf("hv2 link = %q, want both other members", got)
	}
	// Every awake member got the new list.
	if got := e.hosts["hv1"].links["net1"]; got != "192.0.2.11 eth-test 192.0.2.12,192.0.2.13" {
		t.Errorf("hv1 link = %q", got)
	}
	if got := e.hosts["home1"].links["net1"]; got != "192.0.2.13 eth-test 192.0.2.11,192.0.2.12" {
		t.Errorf("home1 link = %q", got)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %v", res.Skipped)
	}

	// A third member joins while hv1 sleeps: one line for it, the join stands.
	e.hosts["hv3"] = newNetHost("hv3", "192.0.2.14")
	e.down["hv1"] = true
	res, err = e.s.JoinNetwork(ctx, "hv3", "net1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "hv1") || !strings.Contains(res.Skipped[0], "wakes") {
		t.Errorf("skipped = %v, want one line naming hv1 and saying it refreshes when it wakes", res.Skipped)
	}
	if got := e.hosts["hv3"].links["net1"]; got != "192.0.2.14 eth-test 192.0.2.11,192.0.2.12,192.0.2.13" {
		t.Errorf("hv3 link = %q: a sleeping member is still a peer", got)
	}
	if got := e.hosts["hv2"].links["net1"]; got != "192.0.2.12 eth-test 192.0.2.11,192.0.2.13,192.0.2.14" {
		t.Errorf("hv2 link = %q", got)
	}
}

func TestJoinNetworkNeedsTheStore(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	s := &Service{OpenHost: e.open, HostAddr: e.s.HostAddr} // local mode
	e.opened = nil
	_, err := s.JoinNetwork(context.Background(), "hv1", "net1", "")
	if err == nil || kindOfErr(t, err) != KindInvalid || !strings.Contains(err.Error(), "store") {
		t.Fatalf("err = %v, want a refusal that names the store", err)
	}
	if len(e.opened) != 0 || len(e.hosts["hv1"].nets) != 1 { // only libvirt's default
		t.Errorf("a host was opened (%v) or changed", e.opened)
	}
}

func TestJoinNetworkRefusals(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		setup func(e *spanEnv)
		host  string
		name  string
		home  string
		kind  Kind
		want  string
	}{
		"the home": {host: "home1", name: "net1", kind: KindConflict, want: "home"},
		"bad name": {host: "hv1", name: "default", kind: KindInvalid, want: "name"},
		"unregistered, no home given": {setup: func(e *spanEnv) {
			e.hosts["home1"].nets["old1"] = &vmtool.NetworkInfo{Name: "old1", Bridge: "vmt-000005", VNI: 5, Vmtool: true, Forward: "nat", Subnet: "10.77.5.0/24", Gateway: "10.77.5.1"}
		}, host: "hv1", name: "old1", kind: KindInvalid, want: "--home"},
		"another home": {host: "hv1", name: "net1", home: "hv2", kind: KindInvalid, want: "home1"},
		"host has a network of that name that is not vmtool's": {setup: func(e *spanEnv) {
			e.hosts["hv1"].nets["net1"] = &vmtool.NetworkInfo{Name: "net1", Bridge: "virbr9", Subnet: "192.168.9.0/24", Gateway: "192.168.9.1", Forward: "nat"}
		}, host: "hv1", name: "net1", kind: KindConflict, want: "not made by vmtool"},
		"host has a home-form network of that name": {setup: func(e *spanEnv) {
			e.hosts["hv1"].nets["net1"] = &vmtool.NetworkInfo{Name: "net1", Bridge: "vmt-000007", Vmtool: true, VNI: 7, Subnet: "10.77.7.0/24", Gateway: "10.77.7.1", Forward: "nat"}
		}, host: "hv1", name: "net1", kind: KindConflict, want: "its own address"},
		"unknown host": {host: "hv9", name: "net1", kind: KindUnavailable, want: "hv9"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSpanEnv(t)
			e.makeNet(t, "net1")
			if c.setup != nil {
				c.setup(e)
			}
			_, err := e.s.JoinNetwork(ctx, c.host, c.name, c.home)
			if err == nil || kindOfErr(t, err) != c.kind || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v (kind %v), want kind %d naming %q", err, kindOfErr(t, err), c.kind, c.want)
			}
			if got := e.members(t, "net1"); len(got) != 0 {
				t.Errorf("a refused join left members %v", got)
			}
		})
	}
}

// A network made without a store (bridge number 1-9999) is registered on its
// first join with that number as its VNI.
func TestJoinNetworkRegistersALocalModeNetwork(t *testing.T) {
	e := newSpanEnv(t)
	home := e.hosts["home1"]
	home.nets["lab"] = &vmtool.NetworkInfo{Name: "lab", Bridge: "vmt-000005", VNI: 5, Vmtool: true, Forward: "nat", Subnet: "10.77.5.0/24",
		Gateway: "10.77.5.1", Domain: "lab.test", MTU: 1450, Active: true, VMs: []string{},
		Reservations: []vmtool.Reservation{{Network: "lab", MAC: "52:54:00:00:00:01", IP: "10.77.5.100", Name: "web1"}}}
	ctx := context.Background()

	res, err := e.s.JoinNetwork(ctx, "hv1", "lab", "home1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Registered || res.VNI != 5 || res.Home != "home1" {
		t.Errorf("result = %+v", res)
	}
	n, err := e.cl.GetNetwork(ctx, "lab")
	if err != nil || n.Vni != 5 || n.Subnet != "10.77.5.0/24" || n.Domain != "lab.test" || n.Home != "home1" {
		t.Fatalf("registry = %+v, %v", n, err)
	}
	if got := e.hosts["hv1"].nets["lab"]; got == nil || got.Bridge != "vmt-000005" || !got.IsMemberForm() {
		t.Errorf("hv1's lab = %+v", got)
	}
	// What the home already has is recorded with it (the VMs are the home's).
	rs, err := e.cl.NetworkReservations(ctx, "lab")
	if err != nil || len(rs) != 1 || rs[0].Mac != "52:54:00:00:00:01" || rs[0].Host != "home1" || rs[0].Vm != "web1" || rs[0].Ip != "10.77.5.100" {
		t.Errorf("recorded reservations = %+v, %v", rs, err)
	}
	// A registered network is not given again.
	e.hosts["hv2"].nets = map[string]*vmtool.NetworkInfo{}
	if res, err = e.s.JoinNetwork(ctx, "hv2", "lab", ""); err != nil || res.Registered {
		t.Errorf("second join = %+v, %v", res, err)
	}
}

func TestJoinNetworkRefusesALocalNumberTheRegistryHas(t *testing.T) {
	e := newSpanEnv(t)
	ctx := context.Background()
	vni := 5
	if _, err := e.cl.CreateNetwork(ctx, storeclient.NetworkInput{Name: "other", Home: "hv2", Vni: &vni}); err != nil {
		t.Fatal(err)
	}
	e.hosts["home1"].nets["lab"] = &vmtool.NetworkInfo{Name: "lab", Bridge: "vmt-000005", VNI: 5, Vmtool: true, Forward: "nat", Subnet: "10.77.5.0/24", Gateway: "10.77.5.1", Domain: "lab.test"}
	_, err := e.s.JoinNetwork(ctx, "hv1", "lab", "home1")
	if err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "recreate it with a store") {
		t.Fatalf("err = %v, want a conflict saying to recreate it with a store", err)
	}
	if _, err := e.cl.GetNetwork(ctx, "lab"); !errors.Is(err, storeclient.ErrNotFound) {
		t.Errorf("registry has lab after a refused join: %v", err)
	}
	if _, ok := e.hosts["hv1"].nets["lab"]; ok {
		t.Error("hv1 defined a network after a refused join")
	}
}

// A step that fails takes back the ones before it.
func TestJoinNetworkUndoesWhatItDidWhenLinkingFails(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	e.hosts["hv1"].linkErr = errors.New("vmtool-net link: bridge does not exist")
	ctx := context.Background()
	_, err := e.s.JoinNetwork(ctx, "hv1", "net1", "")
	if err == nil || !strings.Contains(err.Error(), "bridge does not exist") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.hosts["hv1"].nets["net1"]; ok && e.hosts["hv1"].nets["net1"].IsMemberForm() {
		t.Error("hv1 keeps the member network")
	}
	if got := e.members(t, "net1"); len(got) != 0 {
		t.Errorf("registry members after the undo = %v, want none (the home's row too)", got)
	}
	// The network itself is still registered: only this join was undone.
	if _, err := e.cl.GetNetwork(ctx, "net1"); err != nil {
		t.Errorf("registry lost the network: %v", err)
	}
	// And a retry works.
	e.hosts["hv1"].linkErr = nil
	if _, err := e.s.JoinNetwork(ctx, "hv1", "net1", ""); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestJoinNetworkUndoesAnOlderDaemonThatIgnoresMember(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	e.hosts["hv1"].oldHost = true
	_, err := e.s.JoinNetwork(context.Background(), "hv1", "net1", "")
	if err == nil || !strings.Contains(err.Error(), "just deploy hv1") {
		t.Fatalf("err = %v, want the redeploy message", err)
	}
	if _, ok := e.hosts["hv1"].nets["net1"]; ok && !e.hosts["hv1"].nets["net1"].IsMemberForm() && len(e.hosts["hv1"].deleted) == 0 {
		t.Error("the home-form network the old daemon made was not deleted")
	}
	if got := e.members(t, "net1"); len(got) != 0 {
		t.Errorf("members after the undo = %v", got)
	}
}

func TestJoinNetworkAgainRelinksWithoutDefiningAgain(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	ctx := context.Background()
	if _, err := e.s.JoinNetwork(ctx, "hv1", "net1", ""); err != nil {
		t.Fatal(err)
	}
	hv1 := e.hosts["hv1"]
	hv1.createVN, hv1.linkLog = nil, nil
	if _, err := e.s.JoinNetwork(ctx, "hv1", "net1", ""); err != nil {
		t.Fatal(err)
	}
	if len(hv1.createVN) != 0 || len(hv1.linkLog) != 1 {
		t.Errorf("second join: created %v, linked %v, want no define and one link", hv1.createVN, hv1.linkLog)
	}
}

func TestJoinNeedsTheHomeAwakeTheFirstTime(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	e.down["home1"] = true
	_, err := e.s.JoinNetwork(context.Background(), "hv1", "net1", "")
	if err == nil || kindOfErr(t, err) != KindUnavailable || !strings.Contains(err.Error(), "home1") {
		t.Fatalf("err = %v, want unavailable, naming home1", err)
	}
	if _, ok := e.hosts["hv1"].nets["net1"]; ok && e.hosts["hv1"].nets["net1"].IsMemberForm() {
		t.Error("the member network was defined before the home was known to answer")
	}
	if got := e.members(t, "net1"); len(got) != 0 {
		t.Errorf("members = %v", got)
	}
}

func TestLeaveNetwork(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	ctx := context.Background()
	for _, h := range []string{"hv1", "hv2"} {
		if _, err := e.s.JoinNetwork(ctx, h, "net1", ""); err != nil {
			t.Fatal(err)
		}
	}
	hv1, hv2, home := e.hosts["hv1"], e.hosts["hv2"], e.hosts["home1"]

	// Refused while a VM of the host is on the network, naming it; nothing changes.
	hv1.nets["net1"].VMs = []string{"web1"}
	_, err := e.s.LeaveNetwork(ctx, "hv1", "net1")
	if err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "web1") {
		t.Fatalf("leave with a VM: %v", err)
	}
	if len(hv1.unlinked) != 0 || len(hv1.deleted) != 0 || len(e.members(t, "net1")) != 3 {
		t.Errorf("a refused leave changed things: unlinked %v deleted %v members %v", hv1.unlinked, hv1.deleted, e.members(t, "net1"))
	}

	hv1.nets["net1"].VMs = nil
	hv2.linkLog, home.linkLog = nil, nil
	res, err := e.s.LeaveNetwork(ctx, "hv1", "net1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %v", res.Skipped)
	}
	if len(hv1.unlinked) != 1 || len(hv1.deleted) != 1 {
		t.Errorf("unlinked %v, deleted %v, want one each", hv1.unlinked, hv1.deleted)
	}
	if _, ok := hv1.nets["net1"]; ok && hv1.nets["net1"].IsMemberForm() {
		t.Error("hv1 keeps the member network")
	}
	if got := e.members(t, "net1"); len(got) != 2 || got["hv1"] != "" {
		t.Errorf("members = %v, want hv2 and home1 only", got)
	}
	// The shorter list went to the others.
	if got := hv2.links["net1"]; got != "192.0.2.12 eth-test 192.0.2.13" {
		t.Errorf("hv2 link = %q", got)
	}
	if got := home.links["net1"]; got != "192.0.2.13 eth-test 192.0.2.12" {
		t.Errorf("home1 link = %q", got)
	}
	// Leaving again (the host no longer has the network, the registry no row) is
	// told, not silently fine? The row is gone, so there is nothing to leave.
	if _, err := e.s.LeaveNetwork(ctx, "hv1", "net1"); err == nil {
		t.Error("a second leave succeeded")
	}
}

func TestLeaveNetworkRefusals(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	ctx := context.Background()
	if _, err := e.s.JoinNetwork(ctx, "hv1", "net1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.LeaveNetwork(ctx, "home1", "net1"); err == nil || kindOfErr(t, err) != KindConflict || !strings.Contains(err.Error(), "home") {
		t.Errorf("the home leaves: %v", err)
	}
	if _, err := e.s.LeaveNetwork(ctx, "hv1", "nonet"); err == nil || kindOfErr(t, err) != KindNotFound {
		t.Errorf("unregistered: %v, want not found", err)
	}
	e.hosts["hv2"].nets["net1"] = &vmtool.NetworkInfo{Name: "net1", Bridge: "vmt-000007", Vmtool: true, VNI: 7, Subnet: "10.77.7.0/24", Gateway: "10.77.7.1", Forward: "nat"}
	if _, err := e.s.LeaveNetwork(ctx, "hv2", "net1"); err == nil || kindOfErr(t, err) != KindConflict {
		t.Errorf("a home-form network leaves: %v", err)
	}
}

// A host that has no libvirt network of it any more but still has its row
// (it was rebuilt) is removed from the registry.
func TestLeaveNetworkCleansARowWithNoNetwork(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	ctx := context.Background()
	if _, err := e.s.JoinNetwork(ctx, "hv1", "net1", ""); err != nil {
		t.Fatal(err)
	}
	delete(e.hosts["hv1"].nets, "net1")
	if _, err := e.s.LeaveNetwork(ctx, "hv1", "net1"); err != nil {
		t.Fatal(err)
	}
	if got := e.members(t, "net1"); got["hv1"] != "" {
		t.Errorf("members = %v", got)
	}
}

func TestLeaveNetworkSkipsASleepingMember(t *testing.T) {
	e := newSpanEnv(t)
	e.makeNet(t, "net1")
	ctx := context.Background()
	for _, h := range []string{"hv1", "hv2"} {
		if _, err := e.s.JoinNetwork(ctx, h, "net1", ""); err != nil {
			t.Fatal(err)
		}
	}
	e.down["hv2"] = true
	res, err := e.s.LeaveNetwork(ctx, "hv1", "net1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "hv2") {
		t.Errorf("skipped = %v", res.Skipped)
	}
	m := e.members(t, "net1")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "home1,hv2" {
		t.Errorf("members = %v", keys)
	}
}
