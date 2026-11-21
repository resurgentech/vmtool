package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// V1: `vmtool net create|delete|show <host> <name>` and the marks in
// `vmtool networks`. The hosts are fakes; no libvirt is opened.

func (f *fakeHosts) netRec(s string) {
	f.mu.Lock()
	f.netOps = append(f.netOps, s)
	f.mu.Unlock()
}

func (f *fakeHosts) netSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.netOps...)
}

func fakeNet(name string) *vmtool.NetworkInfo {
	if name == "default" {
		return &vmtool.NetworkInfo{Name: name, Bridge: "virbr0", Forward: "nat", Subnet: "192.0.2.0/24", Active: true, Autostart: true}
	}
	return &vmtool.NetworkInfo{Name: name, Bridge: "vmt-000001", VNI: 1, Vmtool: true, Forward: "nat", Subnet: "10.77.1.0/24",
		Gateway: "10.77.1.1", Domain: name + ".test", MTU: 1450, DHCPStart: "10.77.1.100", DHCPEnd: "10.77.1.200",
		Active: true, Autostart: true, VMs: []string{"vm1", "vm2"}}
}

func (f *fakeHosts) netCreate(host, name, subnet, domain string) (*vmtool.NetworkInfo, error) {
	f.netRec(fmt.Sprintf("%s create %s %s %s", host, name, subnet, domain))
	if name == "taken" {
		return nil, fmt.Errorf("a libvirt network named %q already exists: %w", name, vmtool.ErrNetworkExists)
	}
	return fakeNet(name), nil
}

func (f *fakeHosts) netShow(host, name string) (*vmtool.NetworkInfo, error) {
	if name == "ghost" {
		return nil, fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
	}
	return fakeNet(name), nil
}

func (f *fakeHosts) netDelete(host, name string) error {
	f.netRec(host + " delete " + name)
	if name == "default" {
		return fmt.Errorf("network %q has bridge %q: %w", name, "virbr0", vmtool.ErrNetworkNotVmtool)
	}
	return nil
}

var netKeep = map[string]bool{"vmtool net create": true, "vmtool net delete": true, "vmtool net show": true, "vmtool networks": true}

func TestNetCreate(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	r := runGate(t, dir, netKeep, "net", "create", "veritech01", "net1", "--subnet", "10.77.1.0/24", "--domain", "lab.example")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := f.netSeen(); len(got) != 1 || got[0] != "veritech01 create net1 10.77.1.0/24 lab.example" {
		t.Errorf("ops %q", got)
	}
	for _, want := range []string{`Network "net1" created on veritech01`, "vmtool (VNI 1)", "vmt-000001", "10.77.1.0/24", "10.77.1.100 - 10.77.1.200"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	// The defaults are the host's to pick: empty subnet and domain go on empty.
	f = installFakeHosts(t)
	if r := runGate(t, dir, netKeep, "net", "create", "veritech01", "net1"); r.err != nil {
		t.Fatal(r.err)
	}
	if got := f.netSeen(); len(got) != 1 || got[0] != "veritech01 create net1  " {
		t.Errorf("default ops %q", got)
	}
}

func TestNetCreateRefusals(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	r := runGate(t, dir, netKeep, "net", "create", "veritech01", "default")
	if r.err == nil || !strings.Contains(r.err.Error(), "default") {
		t.Errorf("default: %v", r.err)
	}
	if got := f.netSeen(); len(got) != 0 {
		t.Errorf("default reached the host: %q", got)
	}
	r = runGate(t, dir, netKeep, "net", "create", "veritech01", "taken")
	if r.err == nil || !strings.Contains(r.err.Error(), "already exists") {
		t.Errorf("taken: %v", r.err)
	}
	r = runGate(t, dir, netKeep, "net", "create", "nosuch", "net1")
	if !errors.Is(r.err, hosts.ErrUnknownHost) {
		t.Errorf("unknown host: %v", r.err)
	}
}

func TestNetDeleteAndShow(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	if r := runGate(t, dir, netKeep, "net", "delete", "veritech01", "net1"); r.err != nil || !strings.Contains(r.out, `Network "net1" deleted from veritech01`) {
		t.Errorf("delete: %v\n%s", r.err, r.out)
	}
	if got := f.netSeen(); len(got) != 1 || got[0] != "veritech01 delete net1" {
		t.Errorf("ops %q", got)
	}
	if r := runGate(t, dir, netKeep, "net", "delete", "veritech01", "default"); r.err == nil || !strings.Contains(r.err.Error(), "not made by vmtool") {
		t.Errorf("delete default: %v", r.err)
	}
	r := runGate(t, dir, netKeep, "net", "show", "veritech01", "net1")
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, want := range []string{"net1", "vmtool (VNI 1)", "active, autostart", "net1.test", "mtu:", "1450", "vm1", "vm2"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("show lacks %q:\n%s", want, r.out)
		}
	}
	if r := runGate(t, dir, netKeep, "net", "show", "veritech01", "default"); r.err != nil || !strings.Contains(r.out, "not made by vmtool") {
		t.Errorf("show default: %v\n%s", r.err, r.out)
	}
	if r := runGate(t, dir, netKeep, "net", "show", "veritech01", "ghost"); r.err == nil || !strings.Contains(r.err.Error(), "not found") {
		t.Errorf("show ghost: %v", r.err)
	}
}

// networks lists names as before, with vmtool's marked.
func TestNetworksMarksVmtoolNetworks(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	installFakeHosts(t)
	r := runGate(t, dir, netKeep, "networks", "--host", "veritech01")
	if r.err != nil {
		t.Fatal(r.err)
	}
	// cliFake lists one network, "<host>-net", which it describes as vmtool's.
	if strings.TrimSpace(r.out) != "veritech01-net (vmtool)" {
		t.Errorf("output %q", r.out)
	}
}

// V3: the help says where the number and the subnet come from in each mode.
func TestNetCreateHelpNamesTheRegistry(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	installFakeHosts(t)
	r := runGate(t, dir, netKeep, "net", "create", "--help")
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, want := range []string{"registry", "bridge number", "VNI", "10000"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("help lacks %q:\n%s", want, r.out)
		}
	}
}
