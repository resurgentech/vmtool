package vmtool

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Named networks on one host (tranche 17, V1). Everything here is pure: the
// network XML vmtool defines, the parser for any libvirt network's XML, the
// bridge-name rule, and the choices (subnet, bridge number) that read what a
// host already uses. The libvirt calls are in libvirt.go.
//
// vmtool writes no metadata into a network definition (locked decision "VM
// networks" 2). A network is vmtool's exactly when its bridge is BridgePrefix
// plus six lowercase hex digits; those digits are its VNI.

const (
	// NetworkMTU is every vmtool network's MTU: the VXLAN overhead is taken
	// off 1500 so a network can gain members on other hosts later.
	NetworkMTU = 1450
	// BridgePrefix starts every vmtool network's bridge name.
	BridgePrefix = "vmt-"
	// NetworkBaseCIDR is the range vmtool takes /24s from.
	NetworkBaseCIDR = "10.77.0.0/16"
	// maxLocalBridgeNumber: with no store a host numbers its own bridges
	// 1..9999, below the registry's range (10000 up), so a local number never
	// collides with one the registry hands out later.
	maxLocalBridgeNumber = 9999
	maxNetworkName       = 63
)

var (
	// ErrInvalidNetwork wraps a refused name, subnet, domain or bridge (400).
	ErrInvalidNetwork = errors.New("invalid network")
	// ErrSubnetOverlap wraps a given subnet that a libvirt network or a route
	// on the host already covers (400).
	ErrSubnetOverlap = errors.New("subnet overlap")
	// ErrNoFreeSubnet wraps a host with no free /24 (or bridge number) left (409).
	ErrNoFreeSubnet = errors.New("no free subnet")
	// ErrNetworkExists wraps a name a libvirt network already has (409).
	ErrNetworkExists = errors.New("network exists")
	// ErrNetworkNotVmtool wraps a refusal to touch a network whose bridge is
	// not vmtool's (409).
	ErrNetworkNotVmtool = errors.New("not made by vmtool")
	// ErrNetworkInUse wraps a refusal to delete a network VMs are attached to (409).
	ErrNetworkInUse = errors.New("network in use")
	// ErrUnderlay wraps a failure to find the address and interface that reach
	// another host: no route, a route to the host's own address, or an answer
	// that is no usable address and device (409).
	ErrUnderlay = errors.New("no underlay")
)

// NetworkSpec is what BuildNetworkXML writes: the home form of a network.
type NetworkSpec struct {
	Name   string
	Bridge string
	Subnet netip.Prefix // an IPv4 /24, aligned
	Domain string
}

// NetworkInfo describes one libvirt network. The XML parser fills the
// definition's fields; Active, Autostart and VMs come from libvirt.
type NetworkInfo struct {
	Name      string
	Bridge    string
	VNI       uint32 // the bridge's six hex digits; set only when Vmtool
	Vmtool    bool   // the bridge is vmt- plus six lowercase hex digits
	Forward   string // forward mode; empty for an isolated network
	Subnet    string // CIDR of the first IPv4 <ip>, masked
	Gateway   string
	Domain    string
	MTU       int
	DHCPStart string
	DHCPEnd   string
	Active    bool
	Autostart bool
	VMs       []string // VMs with a card on this network
	// Reservations are the DHCP host entries of the definition: libvirt's own
	// configuration (<host mac ip name>), read, never annotated.
	Reservations []Reservation
}

var (
	bridgeRE  = regexp.MustCompile(`^vmt-[0-9a-f]{6}$`)
	dnsNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
)

// BridgeVNI reports whether bridge is a vmtool network's bridge and, if so,
// its VNI.
func BridgeVNI(bridge string) (uint32, bool) {
	if !bridgeRE.MatchString(bridge) {
		return 0, false
	}
	v, err := strconv.ParseUint(bridge[len(BridgePrefix):], 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// BridgeName is the bridge of the network with that VNI (24 bits).
func BridgeName(vni uint32) string { return fmt.Sprintf("%s%06x", BridgePrefix, vni&0xffffff) }

// ValidNetworkName is the VM name rule, at most 63 characters, and not
// "default" (libvirt's own network).
func ValidNetworkName(name string) bool {
	return name != "default" && len(name) <= maxNetworkName && ValidMachineName(name)
}

func validDomain(d string) bool { return len(d) <= 253 && dnsNameRE.MatchString(d) }

func invalidNetwork(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidNetwork, fmt.Sprintf(format, a...))
}

func checkSubnet(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is4() || p.Bits() != 24 || p.Masked() != p {
		return invalidNetwork("subnet %s must be an IPv4 /24 written as its network address, such as 10.77.1.0/24", p)
	}
	return nil
}

// BuildNetworkXML is the libvirt XML of the home form of a vmtool network:
// NAT, the bridge, MTU 1450, a localOnly DNS domain, and DHCP over .100 to .200
// of the /24. There is no <metadata> and no annotation of any kind.
func BuildNetworkXML(s NetworkSpec) (string, error) {
	if !ValidNetworkName(s.Name) {
		return "", invalidNetwork("name %q: letters, digits, '.', '_', '-', up to %d characters, and not \"default\"", s.Name, maxNetworkName)
	}
	if _, ok := BridgeVNI(s.Bridge); !ok {
		return "", invalidNetwork("bridge %q: want %s plus six lowercase hex digits", s.Bridge, BridgePrefix)
	}
	if !validDomain(s.Domain) {
		return "", invalidNetwork("domain %q is not a DNS name", s.Domain)
	}
	if err := checkSubnet(s.Subnet); err != nil {
		return "", err
	}
	b := s.Subnet.Addr().As4()
	host := func(last byte) string { return netip.AddrFrom4([4]byte{b[0], b[1], b[2], last}).String() }
	return fmt.Sprintf(`<network>
  <name>%s</name>
  <forward mode='nat'/>
  <bridge name='%s' stp='off'/>
  <mtu size='%d'/>
  <domain name='%s' localOnly='yes'/>
  <ip address='%s' netmask='255.255.255.0'>
    <dhcp>
      <range start='%s' end='%s'/>
    </dhcp>
  </ip>
</network>
`, s.Name, s.Bridge, NetworkMTU, s.Domain, host(1), host(100), host(200)), nil
}

// ParseNetworkXML reads any libvirt network's XML: a vmtool one or not.
// Active, Autostart and VMs are left for the caller.
func ParseNetworkXML(x string) (*NetworkInfo, error) {
	var d struct {
		Name    string `xml:"name"`
		Forward struct {
			Mode string `xml:"mode,attr"`
		} `xml:"forward"`
		Bridge struct {
			Name string `xml:"name,attr"`
		} `xml:"bridge"`
		MTU struct {
			Size int `xml:"size,attr"`
		} `xml:"mtu"`
		Domain struct {
			Name string `xml:"name,attr"`
		} `xml:"domain"`
		IPs []struct {
			Family  string `xml:"family,attr"`
			Address string `xml:"address,attr"`
			Netmask string `xml:"netmask,attr"`
			Prefix  int    `xml:"prefix,attr"`
			Range   struct {
				Start string `xml:"start,attr"`
				End   string `xml:"end,attr"`
			} `xml:"dhcp>range"`
			Hosts []struct {
				MAC  string `xml:"mac,attr"`
				IP   string `xml:"ip,attr"`
				Name string `xml:"name,attr"`
			} `xml:"dhcp>host"`
		} `xml:"ip"`
	}
	if err := xml.Unmarshal([]byte(x), &d); err != nil {
		return nil, fmt.Errorf("parsing network XML: %w", err)
	}
	n := &NetworkInfo{Name: d.Name, Bridge: d.Bridge.Name, Forward: d.Forward.Mode, Domain: d.Domain.Name, MTU: d.MTU.Size}
	if v, ok := BridgeVNI(n.Bridge); ok {
		n.Vmtool, n.VNI = true, v
	}
	for _, ip := range d.IPs {
		addr, err := netip.ParseAddr(ip.Address)
		if err != nil || !addr.Is4() || (ip.Family != "" && ip.Family != "ipv4") {
			continue
		}
		ones := ip.Prefix
		if ip.Netmask != "" {
			if m := net.ParseIP(ip.Netmask).To4(); m != nil {
				ones = bits.OnesCount32(uint32(m[0])<<24 | uint32(m[1])<<16 | uint32(m[2])<<8 | uint32(m[3]))
			}
		}
		if ones <= 0 || ones > 32 {
			continue
		}
		n.Gateway = addr.String()
		n.Subnet = netip.PrefixFrom(addr, ones).Masked().String()
		n.DHCPStart, n.DHCPEnd = ip.Range.Start, ip.Range.End
		for _, h := range ip.Hosts {
			if h.MAC == "" {
				continue // an entry by name or address only: not a card's
			}
			n.Reservations = append(n.Reservations, Reservation{Network: n.Name, MAC: normMAC(h.MAC), IP: h.IP, Name: h.Name})
		}
		break
	}
	return n, nil
}

// ParseProcNetRoute reads the text of /proc/net/route into the prefixes the
// host routes. The default route (a /0) is left out: it is no subnet in use.
func ParseProcNetRoute(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) == 0 {
			continue
		}
		if len(f) < 8 {
			return nil, fmt.Errorf("route table line %d: want at least 8 fields, got %d", i+1, len(f))
		}
		dst, err1 := strconv.ParseUint(f[1], 16, 32)
		mask, err2 := strconv.ParseUint(f[7], 16, 32)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("route table line %d: destination or mask is not hex", i+1)
		}
		// The kernel prints the words in host order on a little-endian CPU:
		// the first address byte is the low byte.
		ones := bits.OnesCount32(uint32(mask))
		if ones == 0 {
			continue
		}
		a := netip.AddrFrom4([4]byte{byte(dst), byte(dst >> 8), byte(dst >> 16), byte(dst >> 24)})
		out = append(out, netip.PrefixFrom(a, ones).Masked())
	}
	return out, nil
}

// PlanNetwork decides what a new network is: it validates name, subnet and
// domain, refuses a name a libvirt network already has, takes the given
// subnet (refusing one that overlaps a libvirt network or a route on the host,
// naming the overlap) or the first /24 of NetworkBaseCIDR that none uses, and
// the lowest free bridge number from 1 (below the registry's 10000 up). A
// non-zero vni is the bridge number to use instead (the registry's, 24 bits):
// a bridge of that number already on the host is refused (ErrNetworkExists).
func PlanNetwork(name, subnet, domain string, existing []NetworkInfo, routes []netip.Prefix, vni uint32) (NetworkSpec, error) {
	if !ValidNetworkName(name) {
		return NetworkSpec{}, invalidNetwork("name %q: letters, digits, '.', '_', '-', up to %d characters, and not \"default\"", name, maxNetworkName)
	}
	if domain == "" {
		domain = name + ".test"
		if !validDomain(domain) {
			return NetworkSpec{}, invalidNetwork("%q makes no DNS domain (%q); give one with --domain", name, domain)
		}
	} else if !validDomain(domain) {
		return NetworkSpec{}, invalidNetwork("domain %q is not a DNS name", domain)
	}
	for _, n := range existing {
		if n.Name == name {
			return NetworkSpec{}, fmt.Errorf("%w: a libvirt network named %q already exists", ErrNetworkExists, name)
		}
	}
	overlap := func(p netip.Prefix) string {
		for _, n := range existing {
			if q, err := netip.ParsePrefix(n.Subnet); err == nil && q.Overlaps(p) {
				return fmt.Sprintf("libvirt network %q (%s)", n.Name, q)
			}
		}
		for _, r := range routes {
			if r.Overlaps(p) {
				return fmt.Sprintf("route %s", r)
			}
		}
		return ""
	}
	var pick netip.Prefix
	if subnet != "" {
		p, err := netip.ParsePrefix(subnet)
		if err != nil {
			return NetworkSpec{}, invalidNetwork("subnet %q is not a CIDR: %v", subnet, err)
		}
		if err := checkSubnet(p); err != nil {
			return NetworkSpec{}, err
		}
		if o := overlap(p); o != "" {
			return NetworkSpec{}, fmt.Errorf("%w: subnet %s overlaps %s", ErrSubnetOverlap, p, o)
		}
		pick = p
	} else {
		base := netip.MustParsePrefix(NetworkBaseCIDR).Addr().As4()
		for i := 0; i < 256; i++ {
			p := netip.PrefixFrom(netip.AddrFrom4([4]byte{base[0], base[1], byte(i), 0}), 24)
			if overlap(p) == "" {
				pick = p
				break
			}
		}
		if !pick.IsValid() {
			return NetworkSpec{}, fmt.Errorf("%w: every /24 of %s is used by a libvirt network or a route on this host", ErrNoFreeSubnet, NetworkBaseCIDR)
		}
	}
	used := map[uint32]bool{}
	for _, n := range existing {
		if v, ok := BridgeVNI(n.Bridge); ok {
			used[v] = true
		}
	}
	if vni != 0 {
		if vni > maxVNI {
			return NetworkSpec{}, invalidNetwork("bridge number %d is beyond %d (24 bits)", vni, maxVNI)
		}
		if used[vni] {
			return NetworkSpec{}, fmt.Errorf("%w: a bridge %s already exists on this host", ErrNetworkExists, BridgeName(vni))
		}
		return NetworkSpec{Name: name, Bridge: BridgeName(vni), Subnet: pick, Domain: domain}, nil
	}
	for v := uint32(1); v <= maxLocalBridgeNumber; v++ {
		if !used[v] {
			return NetworkSpec{Name: name, Bridge: BridgeName(v), Subnet: pick, Domain: domain}, nil
		}
	}
	return NetworkSpec{}, fmt.Errorf("%w: no bridge number 1..%d is free on this host", ErrNoFreeSubnet, maxLocalBridgeNumber)
}

// CheckNetworkDeletable is nil for a vmtool network no VM is attached to. A
// network whose bridge is not vmtool's is refused ("not made by vmtool"), and
// so is one with VMs on it (naming them).
func CheckNetworkDeletable(n NetworkInfo) error {
	if _, ok := BridgeVNI(n.Bridge); !ok {
		return fmt.Errorf("%w: network %q has bridge %q, not %s plus six lowercase hex digits; vmtool leaves it alone", ErrNetworkNotVmtool, n.Name, n.Bridge, BridgePrefix)
	}
	if len(n.VMs) > 0 {
		return fmt.Errorf("%w: network %q has VMs attached: %s", ErrNetworkInUse, n.Name, strings.Join(n.VMs, ", "))
	}
	return nil
}
