package vmtool

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// The cards of V2: a second <interface> with a MAC vmtool chose, and the DHCP
// reservation that gives it an address and a name. Everything here is pure.

const goldenReservationXML = `<host mac='52:54:00:00:00:01' name='vm1' ip='10.77.1.100'>
  <lease expiry='0'/>
</host>
`

func TestReservationXMLGolden(t *testing.T) {
	got := ReservationXML(Reservation{MAC: "52:54:00:00:00:01", IP: "10.77.1.100", Name: "vm1"})
	if got != goldenReservationXML {
		t.Fatalf("XML differs:\n--- got\n%s--- want\n%s", got, goldenReservationXML)
	}
}

const goldenInterfaceXML = `<interface type='network'>
  <mac address='52:54:00:00:00:01'/>
  <source network='net1'/>
  <model type='virtio'/>
</interface>
`

// The card is all vmtool writes into a VM: no <metadata>, no annotation.
func TestInterfaceXMLGolden(t *testing.T) {
	got := InterfaceXML("net1", "52:54:00:00:00:01")
	if got != goldenInterfaceXML {
		t.Fatalf("XML differs:\n--- got\n%s--- want\n%s", got, goldenInterfaceXML)
	}
	for _, bad := range []string{"<metadata", "urn:vmtool", "vmtool:"} {
		if strings.Contains(got, bad) {
			t.Errorf("interface XML carries %q", bad)
		}
	}
}

// A VM defined by vmtool still has exactly one <interface> and no metadata:
// the extra cards are attached afterwards, through the same path as `net
// attach`.
func TestDomainXMLUnchangedByV2(t *testing.T) {
	x := domainXML(DefaultConfig("vm1", "/var/lib/libvirt/images/vm1.qcow2"))
	if n := strings.Count(x, "<interface"); n != 1 {
		t.Errorf("domain XML has %d interfaces, want 1", n)
	}
	if strings.Contains(x, "<metadata") {
		t.Error("domain XML has metadata")
	}
}

func TestNewMAC(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 0x00, 0x01, 0x00, 0x00, 0x02, 0xab, 0xcd, 0xef})
	got, err := NewMAC(map[string]bool{"52:54:00:00:00:01": true}, r)
	if err != nil {
		t.Fatal(err)
	}
	// the first draw (00:00:01) is taken; the second (00:00:02) is free
	if got != "52:54:00:00:00:02" {
		t.Fatalf("NewMAC = %q, want 52:54:00:00:00:02 (the first draw was in use)", got)
	}
	got, err = NewMAC(nil, r)
	if err != nil || got != "52:54:00:ab:cd:ef" {
		t.Fatalf("NewMAC = %q, %v; want 52:54:00:ab:cd:ef", got, err)
	}
}

func TestNewMACGivesUpOnAnExhaustedSource(t *testing.T) {
	if _, err := NewMAC(nil, bytes.NewReader(nil)); err == nil {
		t.Fatal("an empty random source gave a MAC")
	}
	// a source that only ever draws a used value is given up on, not looped on
	used := map[string]bool{"52:54:00:00:00:00": true}
	if _, err := NewMAC(used, bytes.NewReader(make([]byte, 3*100))); err == nil {
		t.Fatal("a source of only used draws gave a MAC")
	}
}

func TestNormMAC(t *testing.T) {
	if got, err := NormMAC(" 52:54:00:AB:00:01 "); err != nil || got != "52:54:00:ab:00:01" {
		t.Errorf("NormMAC = %q, %v", got, err)
	}
	for _, bad := range []string{"", "52:54:00:00:00", "52-54-00-00-00-01", "zz:54:00:00:00:01", "52:54:00:00:00:01:02", "52:54:00:00:00:0"} {
		if _, err := NormMAC(bad); !errors.Is(err, ErrInvalidNetwork) {
			t.Errorf("NormMAC(%q) = %v, want ErrInvalidNetwork", bad, err)
		}
	}
}

func v1Net(res ...Reservation) NetworkInfo {
	n := NetworkInfo{
		Name: "net1", Bridge: "vmt-000001", Vmtool: true, VNI: 1, Subnet: "10.77.1.0/24", Gateway: "10.77.1.1",
		DHCPStart: "10.77.1.100", DHCPEnd: "10.77.1.200", Reservations: res,
	}
	return n
}

func TestParseNetworkXMLReadsReservations(t *testing.T) {
	x := `<network>
  <name>net1</name>
  <bridge name='vmt-000001' stp='off'/>
  <ip address='10.77.1.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='10.77.1.100' end='10.77.1.200'/>
      <host mac='52:54:00:00:00:01' name='vm1' ip='10.77.1.100'>
        <lease expiry='0'/>
      </host>
      <host mac='52:54:00:AA:00:02' name='vm2' ip='10.77.1.101'/>
    </dhcp>
  </ip>
</network>`
	n, err := ParseNetworkXML(x)
	if err != nil {
		t.Fatal(err)
	}
	want := []Reservation{
		{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100", Name: "vm1"},
		{Network: "net1", MAC: "52:54:00:aa:00:02", IP: "10.77.1.101", Name: "vm2"},
	}
	if len(n.Reservations) != 2 || n.Reservations[0] != want[0] || n.Reservations[1] != want[1] {
		t.Fatalf("reservations = %+v, want %+v", n.Reservations, want)
	}
}

// What BuildNetworkXML writes round-trips with the reservation element added.
func TestReservationXMLInsideANetworkParses(t *testing.T) {
	x, err := BuildNetworkXML(NetworkSpec{Name: "net1", Bridge: "vmt-000001", Subnet: netip.MustParsePrefix("10.77.1.0/24"), Domain: "net1.test"})
	if err != nil {
		t.Fatal(err)
	}
	x = strings.Replace(x, "</dhcp>", ReservationXML(Reservation{MAC: "52:54:00:00:00:01", IP: "10.77.1.150", Name: "vm1"})+"</dhcp>", 1)
	n, err := ParseNetworkXML(x)
	if err != nil || len(n.Reservations) != 1 || n.Reservations[0].IP != "10.77.1.150" {
		t.Fatalf("parse = %+v, %v", n, err)
	}
}

func TestPlanReservation(t *testing.T) {
	r1 := Reservation{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100", Name: "vm1"}
	r2 := Reservation{Network: "net1", MAC: "52:54:00:00:00:02", IP: "10.77.1.101", Name: "vm2"}
	mac := "52:54:00:00:00:09"

	t.Run("the first free address of the DHCP range", func(t *testing.T) {
		got, err := PlanReservation(v1Net(), "vm3", mac, "", nil)
		if err != nil || got.IP != "10.77.1.100" || got.Name != "vm3" || got.MAC != mac || got.Network != "net1" {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("skips reserved addresses", func(t *testing.T) {
		got, err := PlanReservation(v1Net(r1, r2), "vm3", mac, "", nil)
		if err != nil || got.IP != "10.77.1.102" {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("takes the lowest gap, not the one after the last", func(t *testing.T) {
		got, err := PlanReservation(v1Net(r2), "vm3", mac, "", nil)
		if err != nil || got.IP != "10.77.1.100" {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("skips addresses leased to another card", func(t *testing.T) {
		got, err := PlanReservation(v1Net(r1), "vm3", mac, "", []string{"10.77.1.101"})
		if err != nil || got.IP != "10.77.1.102" {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("a full range is refused", func(t *testing.T) {
		n := v1Net()
		n.DHCPStart, n.DHCPEnd = "10.77.1.100", "10.77.1.101"
		n.Reservations = []Reservation{r1, r2}
		if _, err := PlanReservation(n, "vm3", mac, "", nil); !errors.Is(err, ErrNoFreeSubnet) {
			t.Fatalf("err = %v, want ErrNoFreeSubnet", err)
		}
	})
	t.Run("the address asked for", func(t *testing.T) {
		got, err := PlanReservation(v1Net(r1), "vm3", mac, "10.77.1.50", nil)
		if err != nil || got.IP != "10.77.1.50" {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("an address already reserved is refused", func(t *testing.T) {
		if _, err := PlanReservation(v1Net(r1), "vm3", mac, "10.77.1.100", nil); !errors.Is(err, ErrAddressTaken) {
			t.Fatalf("err = %v, want ErrAddressTaken", err)
		}
	})
	t.Run("an address leased to another card is refused", func(t *testing.T) {
		if _, err := PlanReservation(v1Net(), "vm3", mac, "10.77.1.120", []string{"10.77.1.120"}); !errors.Is(err, ErrAddressTaken) {
			t.Fatalf("err = %v, want ErrAddressTaken", err)
		}
	})
	t.Run("outside the subnet, the network address, the broadcast and the gateway", func(t *testing.T) {
		for _, ip := range []string{"10.77.2.5", "192.0.2.12", "10.77.1.0", "10.77.1.255", "10.77.1.1", "nonsense", "fd00::1"} {
			if _, err := PlanReservation(v1Net(), "vm3", mac, ip, nil); err == nil {
				t.Errorf("address %q accepted", ip)
			} else if ip == "10.77.1.1" {
				if !errors.Is(err, ErrAddressTaken) {
					t.Errorf("gateway: err = %v, want ErrAddressTaken", err)
				}
			} else if !errors.Is(err, ErrInvalidNetwork) {
				t.Errorf("%q: err = %v, want ErrInvalidNetwork", ip, err)
			}
		}
	})
	t.Run("a MAC that already has a reservation is refused", func(t *testing.T) {
		if _, err := PlanReservation(v1Net(r1), "vm3", r1.MAC, "", nil); !errors.Is(err, ErrAddressTaken) {
			t.Fatalf("err = %v, want ErrAddressTaken", err)
		}
	})
	t.Run("a name another card already has is refused", func(t *testing.T) {
		if _, err := PlanReservation(v1Net(r1), "vm1", mac, "", nil); !errors.Is(err, ErrAddressTaken) {
			t.Fatalf("err = %v, want ErrAddressTaken", err)
		}
	})
	t.Run("a network that is not vmtool's is refused", func(t *testing.T) {
		n := v1Net()
		n.Vmtool, n.Bridge = false, "virbr0"
		if _, err := PlanReservation(n, "vm3", mac, "", nil); !errors.Is(err, ErrNetworkNotVmtool) {
			t.Fatalf("err = %v, want ErrNetworkNotVmtool", err)
		}
	})
	t.Run("bad names and MACs", func(t *testing.T) {
		for _, vm := range []string{"", "a b", "a'b", "a<b"} {
			if _, err := PlanReservation(v1Net(), vm, mac, "", nil); !errors.Is(err, ErrInvalidNetwork) {
				t.Errorf("vm %q: err = %v, want ErrInvalidNetwork", vm, err)
			}
		}
		if _, err := PlanReservation(v1Net(), "vm3", "nonsense", "", nil); !errors.Is(err, ErrInvalidNetwork) {
			t.Errorf("mac: err = %v", err)
		}
	})
}

const domainWithTwoCards = `<domain type='kvm'>
  <name>vm1</name>
  <devices>
    <interface type='network'>
      <mac address='52:54:00:11:22:33'/>
      <source network='default'/>
      <model type='virtio'/>
    </interface>
    <interface type='network'>
      <mac address='52:54:00:00:00:01'/>
      <source network='net1' bridge='vmt-000001'/>
      <model type='virtio'/>
    </interface>
    <interface type='bridge'>
      <mac address='52:54:00:00:00:77'/>
      <source bridge='br0'/>
    </interface>
  </devices>
</domain>`

func TestParseDomainCards(t *testing.T) {
	got, err := ParseDomainCards(domainWithTwoCards)
	if err != nil {
		t.Fatal(err)
	}
	want := []DomainCard{
		{Type: "network", MAC: "52:54:00:11:22:33", Network: "default"},
		{Type: "network", MAC: "52:54:00:00:00:01", Network: "net1"},
		{Type: "bridge", MAC: "52:54:00:00:00:77"},
	}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("cards = %+v, want %+v", got, want)
	}
}

func TestAttachmentsListOnlyVmtoolNetworks(t *testing.T) {
	cards, _ := ParseDomainCards(domainWithTwoCards)
	def := NetworkInfo{Name: "default", Bridge: "virbr0"}
	net1 := v1Net(Reservation{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100", Name: "vm1"})
	got := Attachments(cards, map[string]NetworkInfo{"default": def, "net1": net1})
	if len(got) != 1 || got[0] != (NetAttachment{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100"}) {
		t.Fatalf("attachments = %+v", got)
	}
	// a card with no reservation is listed with no address
	got = Attachments(cards, map[string]NetworkInfo{"net1": v1Net()})
	if len(got) != 1 || got[0].IP != "" {
		t.Fatalf("attachments without a reservation = %+v", got)
	}
	// a network libvirt does not know is not listed
	if got := Attachments(cards, nil); len(got) != 0 {
		t.Fatalf("attachments with no networks = %+v", got)
	}
}

// Every VM card MAC on the host and every reservation MAC is taken.
func TestUsedMACs(t *testing.T) {
	used := UsedMACs([]string{domainWithTwoCards}, []NetworkInfo{v1Net(Reservation{MAC: "52:54:00:AA:00:09"})})
	for _, m := range []string{"52:54:00:11:22:33", "52:54:00:00:00:01", "52:54:00:00:00:77", "52:54:00:aa:00:09"} {
		if !used[m] {
			t.Errorf("%s not in the used set %v", m, used)
		}
	}
}
