package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V2: `vmtool net attach|detach <host/vm> ...`, `create --network`, and the
// cards in `vmtool info`. The hosts are fakes; no libvirt is opened.

func (c cliFake) AddReservation(network, vm, mac, ip string) (*vmtool.Reservation, error) {
	c.f.netRec(fmt.Sprintf("%s reserve %s %s %s", c.host, network, vm, ip))
	if ip == "" {
		ip = "10.77.1.100"
	}
	return &vmtool.Reservation{Network: network, MAC: "52:54:00:00:00:01", IP: ip, Name: vm}, nil
}

func (c cliFake) RemoveReservation(network, mac string) error {
	c.f.netRec(fmt.Sprintf("%s unreserve %s %s", c.host, network, mac))
	return nil
}

func (c cliFake) AttachInterface(vm, network, mac string) (*vmtool.NetAttachment, error) {
	c.f.netRec(fmt.Sprintf("%s attach %s %s %s", c.host, vm, network, mac))
	return &vmtool.NetAttachment{Network: network, MAC: mac, IP: "10.77.1.100"}, nil
}

func (c cliFake) DetachInterface(vm, mac string) error {
	c.f.netRec(fmt.Sprintf("%s detach %s %s", c.host, vm, mac))
	return nil
}

var cardKeep = map[string]bool{"vmtool net attach": true, "vmtool net detach": true, "vmtool info": true, "vmtool create": true}

func withCards(f *fakeHosts, cards ...vmtool.NetAttachment) {
	f.cardInfo = func(host, vm string) (*vmtool.VMInfo, error) {
		return &vmtool.VMInfo{Name: vm, IP: "192.0.2.12", State: vmtool.StateRunning, Networks: cards}, nil
	}
}

func TestNetAttach(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	withCards(f)
	r := runGate(t, dir, cardKeep, "net", "attach", "veritech01/vm1", "net1", "--ip", "10.77.1.50")
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := []string{"veritech01 reserve net1 vm1 10.77.1.50", "veritech01 attach vm1 net1 52:54:00:00:00:01"}
	if got := f.netSeen(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ops %q, want %q", got, want)
	}
	for _, w := range []string{`VM "veritech01/vm1" attached to network "net1"`, "net1 52:54:00:00:00:01 10.77.1.50"} {
		if !strings.Contains(r.out, w) {
			t.Errorf("output lacks %q:\n%s", w, r.out)
		}
	}
	// no --ip: the host picks the address
	f = installFakeHosts(t)
	withCards(f)
	if r := runGate(t, dir, cardKeep, "net", "attach", "veritech01/vm1", "net1"); r.err != nil || !strings.Contains(r.out, "10.77.1.100") {
		t.Errorf("no --ip: %v\n%s", r.err, r.out)
	}
	if got := f.netSeen(); len(got) < 1 || got[0] != "veritech01 reserve net1 vm1 " {
		t.Errorf("ops %q: an empty address goes on empty", got)
	}
}

func TestNetAttachRefusals(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	withCards(f)
	// not a vmtool network: refused before anything is reserved
	r := runGate(t, dir, cardKeep, "net", "attach", "veritech01/vm1", "default")
	if r.err == nil || !strings.Contains(r.err.Error(), "not one vmtool made") {
		t.Errorf("default: %v", r.err)
	}
	if r := runGate(t, dir, cardKeep, "net", "attach", "veritech01/vm1", "net1", "--ip", "10.99.0.5"); r.err == nil || !strings.Contains(r.err.Error(), "outside the subnet") {
		t.Errorf("outside: %v", r.err)
	}
	if got := f.netSeen(); len(got) != 0 {
		t.Errorf("a refused attach reached the host's reservations: %q", got)
	}
	if r := runGate(t, dir, cardKeep, "net", "attach", "veritech01/vm1", "ghost"); r.err == nil || !strings.Contains(r.err.Error(), "not found") {
		t.Errorf("ghost: %v", r.err)
	}
}

func TestNetDetach(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	withCards(f, vmtool.NetAttachment{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100"})
	r := runGate(t, dir, cardKeep, "net", "detach", "veritech01/vm1", "net1")
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := []string{"veritech01 detach vm1 52:54:00:00:00:01", "veritech01 unreserve net1 52:54:00:00:00:01"}
	if got := f.netSeen(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ops %q, want %q", got, want)
	}
	if !strings.Contains(r.out, `VM "veritech01/vm1" detached from "net1"`) {
		t.Errorf("output %q", r.out)
	}
	if r := runGate(t, dir, cardKeep, "net", "detach", "veritech01/vm1", "net2"); r.err == nil || !strings.Contains(r.err.Error(), "no card on network") {
		t.Errorf("no card: %v", r.err)
	}
}

func TestInfoShowsCards(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	withCards(f, vmtool.NetAttachment{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100"})
	r := runGate(t, dir, cardKeep, "info", "veritech01/vm1")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(r.out, "IP:        192.0.2.12") || !strings.Contains(r.out, "Networks:  net1 52:54:00:00:00:01 10.77.1.100") {
		t.Errorf("info output:\n%s", r.out)
	}
	withCards(f)
	if r := runGate(t, dir, cardKeep, "info", "veritech01/vm1"); r.err != nil || strings.Contains(r.out, "Networks:") {
		t.Errorf("info of a VM with no cards: %v\n%s", r.err, r.out)
	}
}

// --network is read before anything is created; a bad value never reaches the host.
func TestCreateNetworkFlagIsCheckedFirst(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	for _, bad := range []string{"=10.77.1.5", "net1="} {
		r := runGate(t, dir, cardKeep, "create", "veritech01/web", "img.qcow2", "--network", bad)
		if r.err == nil || !strings.Contains(r.err.Error(), "--network") {
			t.Errorf("--network %q: %v", bad, r.err)
		}
	}
	if got := f.netSeen(); len(got) != 0 {
		t.Errorf("a bad --network reached the host: %q", got)
	}
}
