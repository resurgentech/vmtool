package vmtool

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

const goldenHomeNetworkXML = `<network>
  <name>net1</name>
  <forward mode='nat'/>
  <bridge name='vmt-000001' stp='off'/>
  <mtu size='1450'/>
  <domain name='net1.test' localOnly='yes'/>
  <ip address='10.77.1.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='10.77.1.100' end='10.77.1.200'/>
    </dhcp>
  </ip>
</network>
`

func TestNetworkXMLGolden(t *testing.T) {
	x, err := BuildNetworkXML(NetworkSpec{Name: "net1", Bridge: "vmt-000001", Subnet: netip.MustParsePrefix("10.77.1.0/24"), Domain: "net1.test"})
	if err != nil {
		t.Fatal(err)
	}
	if x != goldenHomeNetworkXML {
		t.Fatalf("XML differs:\n--- got\n%s--- want\n%s", x, goldenHomeNetworkXML)
	}
}

// vmtool writes nothing of its own into a network definition (locked
// decision "VM networks" 2).
func TestNetworkXMLHasNoMetadata(t *testing.T) {
	x, err := BuildNetworkXML(NetworkSpec{Name: "net2", Bridge: "vmt-00002a", Subnet: netip.MustParsePrefix("10.77.2.0/24"), Domain: "lab.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<metadata", "urn:vmtool", "vmtool:"} {
		if strings.Contains(x, bad) {
			t.Errorf("network XML carries %q:\n%s", bad, x)
		}
	}
	if !strings.Contains(x, "10.77.2.100") || !strings.Contains(x, "10.77.2.200") || !strings.Contains(x, "lab.example") {
		t.Errorf("range or domain missing:\n%s", x)
	}
}

func TestBuildNetworkXMLRefusesBadSpec(t *testing.T) {
	good := NetworkSpec{Name: "net1", Bridge: "vmt-000001", Subnet: netip.MustParsePrefix("10.77.1.0/24"), Domain: "net1.test"}
	for name, mod := range map[string]func(*NetworkSpec){
		"bridge":  func(s *NetworkSpec) { s.Bridge = "virbr9" },
		"prefix":  func(s *NetworkSpec) { s.Subnet = netip.MustParsePrefix("10.77.0.0/16") },
		"v6":      func(s *NetworkSpec) { s.Subnet = netip.MustParsePrefix("fd00::/24") },
		"name":    func(s *NetworkSpec) { s.Name = "a<b" },
		"domain":  func(s *NetworkSpec) { s.Domain = "a'b" },
		"default": func(s *NetworkSpec) { s.Name = "default" },
	} {
		s := good
		mod(&s)
		if _, err := BuildNetworkXML(s); !errors.Is(err, ErrInvalidNetwork) {
			t.Errorf("%s: err = %v, want ErrInvalidNetwork", name, err)
		}
	}
}

func TestBridgeNameRule(t *testing.T) {
	ok := map[string]uint32{"vmt-000001": 1, "vmt-002710": 10000, "vmt-ffffff": 0xffffff, "vmt-00002a": 42}
	for b, want := range ok {
		got, isV := BridgeVNI(b)
		if !isV || got != want {
			t.Errorf("BridgeVNI(%q) = %d, %v; want %d, true", b, got, isV, want)
		}
		if BridgeName(want) != b {
			t.Errorf("BridgeName(%d) = %q, want %q", want, BridgeName(want), b)
		}
	}
	for _, b := range []string{"", "virbr0", "vmt-00000", "vmt-0000001", "vmt-00000G", "vmt-00002A", "VMT-000001", "vmt-0x0001", "xvmt-000001", "vmt-000001 "} {
		if _, isV := BridgeVNI(b); isV {
			t.Errorf("BridgeVNI(%q) accepted", b)
		}
	}
}

func TestValidNetworkName(t *testing.T) {
	for _, n := range []string{"net1", "a", "lab-net_2", "a.b"} {
		if !ValidNetworkName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "default", ".x", "a b", "a/b", "-x", strings.Repeat("a", 64)} {
		if ValidNetworkName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

const libvirtDefaultXML = `<network connections='1'>
  <name>default</name>
  <uuid>00000000-0000-4000-8000-000000000001</uuid>
  <forward mode='nat'><nat><port start='1024' end='65535'/></nat></forward>
  <bridge name='virbr0' stp='on' delay='0'/>
  <mac address='52:54:00:00:00:01'/>
  <ip address='192.0.2.1' netmask='255.255.255.0'>
    <dhcp><range start='192.0.2.2' end='192.0.2.254'/></dhcp>
  </ip>
  <ip family='ipv6' address='fd00::1' prefix='64'/>
</network>`

func TestParseNetworkXML(t *testing.T) {
	d, err := ParseNetworkXML(libvirtDefaultXML)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "default" || d.Bridge != "virbr0" || d.Vmtool || d.Forward != "nat" ||
		d.Subnet != "192.0.2.0/24" || d.Gateway != "192.0.2.1" || d.DHCPStart != "192.0.2.2" || d.DHCPEnd != "192.0.2.254" {
		t.Errorf("default = %+v", d)
	}
	built, _ := BuildNetworkXML(NetworkSpec{Name: "net1", Bridge: "vmt-000001", Subnet: netip.MustParsePrefix("10.77.1.0/24"), Domain: "net1.test"})
	n, err := ParseNetworkXML(built)
	if err != nil {
		t.Fatal(err)
	}
	want := NetworkInfo{Name: "net1", Bridge: "vmt-000001", VNI: 1, Vmtool: true, Forward: "nat", Subnet: "10.77.1.0/24",
		Gateway: "10.77.1.1", Domain: "net1.test", MTU: 1450, DHCPStart: "10.77.1.100", DHCPEnd: "10.77.1.200"}
	if n.Name != want.Name || n.Bridge != want.Bridge || n.VNI != want.VNI || n.Vmtool != want.Vmtool || n.Forward != want.Forward ||
		n.Subnet != want.Subnet || n.Gateway != want.Gateway || n.Domain != want.Domain || n.MTU != want.MTU ||
		n.DHCPStart != want.DHCPStart || n.DHCPEnd != want.DHCPEnd {
		t.Errorf("parsed = %+v, want %+v", n, want)
	}
	if _, err := ParseNetworkXML("<network"); err == nil {
		t.Error("broken XML accepted")
	}
}

const procRoute = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
	"eth-test\t00000000\t010200C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
	"eth-test\t000200C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n" +
	"virbr0\t00014D0A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"

func TestParseProcNetRoute(t *testing.T) {
	got, err := ParseProcNetRoute(procRoute)
	if err != nil {
		t.Fatal(err)
	}
	// The default route (a /0) is not a subnet in use.
	want := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("10.77.1.0/24")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	if _, err := ParseProcNetRoute("Iface\tDestination\nx\tzz\n"); err == nil {
		t.Error("garbage accepted")
	}
}

func mkNet(name, bridge, subnet string) NetworkInfo {
	n := NetworkInfo{Name: name, Bridge: bridge, Subnet: subnet}
	if v, ok := BridgeVNI(bridge); ok {
		n.Vmtool, n.VNI = true, v
	}
	return n
}

func TestPlanNetworkFirstFree(t *testing.T) {
	s, err := PlanNetwork("net1", "", "", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Subnet.String() != "10.77.0.0/24" || s.Bridge != "vmt-000001" || s.Domain != "net1.test" || s.Name != "net1" {
		t.Errorf("empty host: %+v", s)
	}
	existing := []NetworkInfo{
		mkNet("default", "virbr0", "192.0.2.0/24"),
		mkNet("a", "vmt-000001", "10.77.0.0/24"),
		mkNet("b", "vmt-000003", "10.77.2.0/24"),
	}
	routes := []netip.Prefix{netip.MustParsePrefix("10.77.1.0/24"), netip.MustParsePrefix("10.77.3.128/25")}
	s, err = PlanNetwork("net2", "", "lab.example", existing, routes, 0)
	if err != nil {
		t.Fatal(err)
	}
	// .0, .1 (route), .2 and .3 (route, a /25 inside it) are taken.
	if s.Subnet.String() != "10.77.4.0/24" {
		t.Errorf("subnet = %s, want 10.77.4.0/24", s.Subnet)
	}
	if s.Bridge != "vmt-000002" {
		t.Errorf("bridge = %s, want the lowest free number vmt-000002", s.Bridge)
	}
	if s.Domain != "lab.example" {
		t.Errorf("domain = %s", s.Domain)
	}
}

func TestPlanNetworkGivenSubnet(t *testing.T) {
	existing := []NetworkInfo{mkNet("a", "vmt-000001", "10.77.5.0/24")}
	s, err := PlanNetwork("net1", "10.77.9.0/24", "", existing, nil, 0)
	if err != nil || s.Subnet.String() != "10.77.9.0/24" {
		t.Fatalf("free subnet: %+v, %v", s, err)
	}
	_, err = PlanNetwork("net1", "10.77.5.0/24", "", existing, nil, 0)
	if !errors.Is(err, ErrSubnetOverlap) || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "10.77.5.0/24") {
		t.Errorf("overlap with a network: %v", err)
	}
	_, err = PlanNetwork("net1", "192.0.2.0/24", "", nil, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/23")}, 0)
	if !errors.Is(err, ErrSubnetOverlap) || !strings.Contains(err.Error(), "route 192.0.2.0/23") {
		t.Errorf("overlap with a route: %v", err)
	}
	for _, bad := range []string{"10.77.9.0/16", "10.77.9.0/25", "10.77.9.5/24", "x", "fd00::/24"} {
		if _, err := PlanNetwork("net1", bad, "", nil, nil, 0); !errors.Is(err, ErrInvalidNetwork) {
			t.Errorf("subnet %q: err = %v", bad, err)
		}
	}
}

func TestPlanNetworkRefusals(t *testing.T) {
	if _, err := PlanNetwork("default", "", "", nil, nil, 0); !errors.Is(err, ErrInvalidNetwork) {
		t.Errorf("default: %v", err)
	}
	if _, err := PlanNetwork("bad name", "", "", nil, nil, 0); !errors.Is(err, ErrInvalidNetwork) {
		t.Errorf("bad name: %v", err)
	}
	if _, err := PlanNetwork("net1", "", "bad domain", nil, nil, 0); !errors.Is(err, ErrInvalidNetwork) {
		t.Errorf("bad domain: %v", err)
	}
	if _, err := PlanNetwork("a_b", "", "", nil, nil, 0); !errors.Is(err, ErrInvalidNetwork) {
		t.Errorf("a_b makes no DNS domain: %v", err)
	}
	if _, err := PlanNetwork("a_b", "", "ok.test", nil, nil, 0); err != nil {
		t.Errorf("a_b with a domain: %v", err)
	}
	if _, err := PlanNetwork("net1", "", "", []NetworkInfo{mkNet("net1", "virbr1", "192.0.2.0/24")}, nil, 0); !errors.Is(err, ErrNetworkExists) {
		t.Errorf("existing name: %v", err)
	}
	all := []netip.Prefix{netip.MustParsePrefix("10.77.0.0/16")}
	if _, err := PlanNetwork("net1", "", "", nil, all, 0); !errors.Is(err, ErrNoFreeSubnet) {
		t.Errorf("no room: %v", err)
	}
}

func TestCheckNetworkDeletable(t *testing.T) {
	if err := CheckNetworkDeletable(mkNet("net1", "vmt-000001", "10.77.1.0/24")); err != nil {
		t.Errorf("plain vmtool network: %v", err)
	}
	err := CheckNetworkDeletable(mkNet("default", "virbr0", "192.0.2.0/24"))
	if !errors.Is(err, ErrNetworkNotVmtool) || !strings.Contains(err.Error(), "not made by vmtool") {
		t.Errorf("default: %v", err)
	}
	// A network with no bridge element at all is not vmtool's either.
	if err := CheckNetworkDeletable(NetworkInfo{Name: "x"}); !errors.Is(err, ErrNetworkNotVmtool) {
		t.Errorf("no bridge: %v", err)
	}
	n := mkNet("net1", "vmt-000001", "10.77.1.0/24")
	n.VMs = []string{"vm1", "vm2"}
	err = CheckNetworkDeletable(n)
	if !errors.Is(err, ErrNetworkInUse) || !strings.Contains(err.Error(), "vm1") || !strings.Contains(err.Error(), "vm2") {
		t.Errorf("attached: %v", err)
	}
}

func TestPlanNetworkGivenBridgeNumber(t *testing.T) {
	existing := []NetworkInfo{mkNet("a", "vmt-000001", "10.77.0.0/24"), mkNet("b", "vmt-002710", "10.77.1.0/24")}
	s, err := PlanNetwork("net1", "10.77.2.0/24", "", existing, nil, 10001)
	if err != nil {
		t.Fatal(err)
	}
	if s.Bridge != "vmt-002711" || s.Subnet.String() != "10.77.2.0/24" {
		t.Errorf("given number: %+v, want bridge vmt-002711", s)
	}
	// The given number is used as it is, whatever is free below it.
	if s, err = PlanNetwork("net1", "", "", nil, nil, 5); err != nil || s.Bridge != "vmt-000005" {
		t.Errorf("number 5: %+v, %v", s, err)
	}
	// A bridge of that number already on the host is a conflict (409), named.
	_, err = PlanNetwork("net1", "10.77.2.0/24", "", existing, nil, 10000)
	if !errors.Is(err, ErrNetworkExists) || !strings.Contains(err.Error(), "vmt-002710") {
		t.Errorf("bridge in use: %v, want ErrNetworkExists naming vmt-002710", err)
	}
	// Beyond 24 bits is not a bridge name.
	if _, err := PlanNetwork("net1", "", "", nil, nil, 1<<24); !errors.Is(err, ErrInvalidNetwork) {
		t.Errorf("number 1<<24: %v, want ErrInvalidNetwork", err)
	}
}
