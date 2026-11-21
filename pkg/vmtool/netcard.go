package vmtool

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

// A VM's cards on vmtool networks (tranche 17, V2). A card is a second
// <interface> with a MAC vmtool chose; its fixed address and its name are the
// network's DHCP reservation for that MAC (<host mac ip name>, a lease that
// never expires), which is libvirt's own configuration. Nothing goes into the
// VM's definition but the card itself. All of this is pure; the libvirt calls
// are in netcardlibvirt.go.

// ErrAddressTaken wraps a refusal of an address, a MAC or a name a network
// already has a reservation for, or whose address is the gateway or leased to
// another card (409).
var ErrAddressTaken = errors.New("address taken")

// Reservation is one DHCP host entry of a vmtool network.
type Reservation struct {
	Network string // the network's name; set by the parser and by PlanReservation
	MAC     string // lowercase, colon-separated
	IP      string
	Name    string
}

// NetAttachment is one card of a VM on a vmtool network: the network, the
// card's MAC, and its fixed address (the reservation for that MAC; empty when
// the network has none).
type NetAttachment struct {
	Network string
	MAC     string
	IP      string
}

// DomainCard is one <interface> of a domain as far as networks go. Network is
// the libvirt network of a type=network card; empty for any other type.
type DomainCard struct {
	Type    string
	MAC     string
	Network string
}

// maxMACDraws bounds the search for an unused MAC (there are 2^24 to pick
// from; running out of tries means a broken random source).
const maxMACDraws = 64

// NormMAC lowercases and validates a MAC written as six colon-separated hex
// pairs.
func NormMAC(s string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(s))
	parts := strings.Split(m, ":")
	ok := len(parts) == 6
	for _, p := range parts {
		if len(p) != 2 || strings.Trim(p, "0123456789abcdef") != "" {
			ok = false
		}
	}
	if !ok {
		return "", invalidNetwork("MAC %q: want six colon-separated hex pairs such as 52:54:00:00:00:01", s)
	}
	return m, nil
}

// NewMAC draws a MAC of 52:54:00 (QEMU's prefix) plus three bytes from r that
// is not in used. used holds lowercase MACs.
func NewMAC(used map[string]bool, r io.Reader) (string, error) {
	var b [3]byte
	for i := 0; i < maxMACDraws; i++ {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", fmt.Errorf("choosing a MAC: %w", err)
		}
		m := fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
		if !used[m] {
			return m, nil
		}
	}
	return "", fmt.Errorf("choosing a MAC: no unused MAC in %d draws", maxMACDraws)
}

// UsedMACs is the set of MACs a new card must not take: every card of the given
// domain XMLs and every reservation of the given networks (lowercase).
func UsedMACs(domainXMLs []string, nets []NetworkInfo) map[string]bool {
	used := map[string]bool{}
	for _, x := range domainXMLs {
		cards, err := ParseDomainCards(x)
		if err != nil {
			continue
		}
		for _, c := range cards {
			used[c.MAC] = true
		}
	}
	for _, n := range nets {
		for _, r := range n.Reservations {
			used[strings.ToLower(r.MAC)] = true
		}
	}
	return used
}

// ReservationXML is the DHCP host entry libvirt's virNetworkUpdate takes: the
// MAC's address and name, with a lease that never expires.
func ReservationXML(r Reservation) string {
	return fmt.Sprintf("<host mac='%s' name='%s' ip='%s'>\n  <lease expiry='0'/>\n</host>\n",
		xmlEscape(r.MAC), xmlEscape(r.Name), xmlEscape(r.IP))
}

// InterfaceXML is the card vmtool attaches: virtio on a libvirt network, with
// the MAC vmtool chose. It is all that goes into the VM's definition.
func InterfaceXML(network, mac string) string {
	return fmt.Sprintf("<interface type='network'>\n  <mac address='%s'/>\n  <source network='%s'/>\n  <model type='virtio'/>\n</interface>\n",
		xmlEscape(mac), xmlEscape(network))
}

// ParseDomainCards lists a domain's <interface> elements in definition order,
// so the first is the VM's own first card.
func ParseDomainCards(domXML string) ([]DomainCard, error) {
	var d struct {
		Interfaces []struct {
			Type string `xml:"type,attr"`
			MAC  struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Source struct {
				Network string `xml:"network,attr"`
			} `xml:"source"`
		} `xml:"devices>interface"`
	}
	if err := xml.Unmarshal([]byte(domXML), &d); err != nil {
		return nil, fmt.Errorf("parsing domain XML: %w", err)
	}
	out := make([]DomainCard, 0, len(d.Interfaces))
	for _, i := range d.Interfaces {
		c := DomainCard{Type: i.Type, MAC: normMAC(i.MAC.Address)}
		if i.Type == "network" {
			c.Network = i.Source.Network
		}
		out = append(out, c)
	}
	return out, nil
}

// Attachments lists the cards on vmtool networks, in card order, each with the
// address of its MAC's reservation. nets maps a libvirt network's name to its
// description (with Reservations); a card on a network that is not in nets or
// is not vmtool's is left out.
func Attachments(cards []DomainCard, nets map[string]NetworkInfo) []NetAttachment {
	var out []NetAttachment
	for _, c := range cards {
		if c.Type != "network" || c.Network == "" {
			continue
		}
		n, ok := nets[c.Network]
		if !ok || !n.Vmtool {
			continue
		}
		a := NetAttachment{Network: c.Network, MAC: c.MAC}
		for _, r := range n.Reservations {
			if normMAC(r.MAC) == c.MAC {
				a.IP = r.IP
				break
			}
		}
		out = append(out, a)
	}
	return out
}

// PlanReservation decides the reservation for a new card of vm with the MAC mac
// on network n: it must be a vmtool network. ip asked for must be a host
// address of the subnet (not the network address, the broadcast or the
// gateway) that no reservation and no lease in leased holds; ip omitted is the
// lowest address of the DHCP range (.100 to .200 as vmtool makes it) that none
// holds. A MAC or a name that another reservation of the network already has
// is refused, so a network never lists one VM name twice.
func PlanReservation(n NetworkInfo, vm, mac, ip string, leased []string) (Reservation, error) {
	if !n.Vmtool {
		return Reservation{}, fmt.Errorf("%w: network %q has bridge %q, not %s plus six lowercase hex digits; vmtool leaves it alone", ErrNetworkNotVmtool, n.Name, n.Bridge, BridgePrefix)
	}
	if vm == "" || !ValidMachineName(vm) {
		return Reservation{}, invalidNetwork("VM name %q cannot be a name on a network: letters, digits, '.', '_', '-'", vm)
	}
	// The reservation's name is the VM's hostname form ("_" becomes "-"), the
	// name dnsmasq answers.
	name := hostnameFromName(vm)
	m, err := NormMAC(mac)
	if err != nil {
		return Reservation{}, err
	}
	held := map[netip.Addr]bool{}
	for _, r := range n.Reservations {
		if normMAC(r.MAC) == m {
			return Reservation{}, fmt.Errorf("%w: network %q already has a reservation for %s (%s, %s)", ErrAddressTaken, n.Name, m, r.IP, r.Name)
		}
		if r.Name == name {
			return Reservation{}, fmt.Errorf("%w: network %q already has a card named %q (%s, %s)", ErrAddressTaken, n.Name, name, r.MAC, r.IP)
		}
		if a, err := netip.ParseAddr(r.IP); err == nil {
			held[a] = true
		}
	}
	for _, l := range leased {
		if a, err := netip.ParseAddr(l); err == nil {
			held[a] = true
		}
	}
	subnet, err := netip.ParsePrefix(n.Subnet)
	if err != nil || !subnet.Addr().Is4() {
		return Reservation{}, invalidNetwork("network %q has no IPv4 subnet to take an address from", n.Name)
	}
	gw, _ := netip.ParseAddr(n.Gateway)
	if ip != "" {
		a, err := netip.ParseAddr(ip)
		if err != nil || !a.Is4() {
			return Reservation{}, invalidNetwork("address %q is not an IPv4 address", ip)
		}
		if !subnet.Contains(a) {
			return Reservation{}, invalidNetwork("address %s is outside the subnet %s of network %q", a, subnet, n.Name)
		}
		b := a.As4()
		if a == subnet.Addr() || b[3] == 255 {
			return Reservation{}, invalidNetwork("address %s is the network or broadcast address of %s", a, subnet)
		}
		if a == gw {
			return Reservation{}, fmt.Errorf("%w: %s is the gateway of network %q", ErrAddressTaken, a, n.Name)
		}
		if held[a] {
			return Reservation{}, fmt.Errorf("%w: %s is already held on network %q", ErrAddressTaken, a, n.Name)
		}
		return Reservation{Network: n.Name, MAC: m, IP: a.String(), Name: name}, nil
	}
	start, err1 := netip.ParseAddr(n.DHCPStart)
	end, err2 := netip.ParseAddr(n.DHCPEnd)
	if err1 != nil || err2 != nil || !start.Is4() || !end.Is4() {
		return Reservation{}, invalidNetwork("network %q has no DHCP range to take an address from; give one with --ip", n.Name)
	}
	for a := start; a.Compare(end) <= 0; a = a.Next() {
		if !held[a] && a != gw && subnet.Contains(a) {
			return Reservation{Network: n.Name, MAC: m, IP: a.String(), Name: name}, nil
		}
	}
	return Reservation{}, fmt.Errorf("%w: every address of network %q's DHCP range (%s to %s) is reserved or leased", ErrNoFreeSubnet, n.Name, start, end)
}
