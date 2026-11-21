package vmtool

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Networks that span hosts (tranche 17, V5), the host side. A member host's
// libvirt network has the home's name and bridge and nothing else: no address,
// no NAT, no DHCP. Its VMs get theirs from the home's dnsmasq over the VXLAN
// link. Everything here is pure or runs one tool through command(), so tests
// stub it; the libvirt calls are in netlibvirt.go.

// Underlay is a host's LAN address and the interface that address is on, toward
// some other host: what a VXLAN device is bound to.
type Underlay struct {
	IP  string
	Dev string
}

// BuildMemberNetworkXML is the libvirt XML of the member form of a vmtool
// network: the home's name, its bridge and the MTU. There is no <forward>, no
// <ip>, no <domain> and no <metadata>.
func BuildMemberNetworkXML(name, bridge string) (string, error) {
	if !ValidNetworkName(name) {
		return "", invalidNetwork("name %q: letters, digits, '.', '_', '-', up to %d characters, and not \"default\"", name, maxNetworkName)
	}
	if _, ok := BridgeVNI(bridge); !ok {
		return "", invalidNetwork("bridge %q: want %s plus six lowercase hex digits", bridge, BridgePrefix)
	}
	return fmt.Sprintf(`<network>
  <name>%s</name>
  <bridge name='%s' stp='off'/>
  <mtu size='%d'/>
</network>
`, name, bridge, NetworkMTU), nil
}

// MemberSpec is what BuildMemberNetworkXML writes.
type MemberSpec struct {
	Name   string
	Bridge string
}

// PlanMemberNetwork validates a member's network: the name, the registry's VNI
// (1 to 24 bits), a name no libvirt network on the host has, and a bridge
// number the host does not already use (ErrNetworkExists for both).
func PlanMemberNetwork(name string, vni uint32, existing []NetworkInfo) (MemberSpec, error) {
	if !ValidNetworkName(name) {
		return MemberSpec{}, invalidNetwork("name %q: letters, digits, '.', '_', '-', up to %d characters, and not \"default\"", name, maxNetworkName)
	}
	if vni < 1 || vni > maxVNI {
		return MemberSpec{}, invalidNetwork("network number %d: want 1 to %d", vni, maxVNI)
	}
	bridge := BridgeName(vni)
	for _, n := range existing {
		if n.Name == name {
			return MemberSpec{}, fmt.Errorf("%w: a libvirt network named %q already exists", ErrNetworkExists, name)
		}
		if n.Bridge == bridge {
			return MemberSpec{}, fmt.Errorf("%w: a bridge %s already exists on this host (network %q)", ErrNetworkExists, bridge, n.Name)
		}
	}
	return MemberSpec{Name: name, Bridge: bridge}, nil
}

// IsMemberForm reports whether n is a vmtool network in the member form: a
// bridge of the VNI and no address of its own, so no forwarding, DHCP or DNS.
func (n NetworkInfo) IsMemberForm() bool {
	return n.Vmtool && n.Forward == "" && n.Gateway == ""
}

// ParseRouteGet reads the output of `ip route get ADDR`: the source address and
// the device the host would use to reach ADDR. A route to the host's own address
// (device lo) is refused: that is no underlay.
func ParseRouteGet(s string) (Underlay, error) {
	f := strings.Fields(s)
	var u Underlay
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case "dev":
			if u.Dev == "" {
				u.Dev = f[i+1]
			}
		case "src":
			if u.IP == "" {
				u.IP = f[i+1]
			}
		}
	}
	if u.Dev == "" || u.IP == "" {
		return Underlay{}, fmt.Errorf("%w: ip route get: no source address and device in %q", ErrUnderlay, strings.TrimSpace(s))
	}
	if err := netCheckIPv4("source address", u.IP); err != nil {
		return Underlay{}, fmt.Errorf("%w: %v", ErrUnderlay, err)
	}
	if u.Dev == "lo" {
		return Underlay{}, fmt.Errorf("%w: ip route get: the address is this host's own (device lo)", ErrUnderlay)
	}
	if err := netCheckDev(u.Dev); err != nil {
		return Underlay{}, fmt.Errorf("%w: %v", ErrUnderlay, err)
	}
	return u, nil
}

// FindUnderlay asks the kernel which of this host's addresses, and which
// interface, reach the IPv4 address `to` (`ip route get TO`): no root needed.
func FindUnderlay(to string) (Underlay, error) {
	if err := netCheckIPv4("address", to); err != nil {
		return Underlay{}, err
	}
	cmd, err := command("ip", "route", "get", to)
	if err != nil {
		return Underlay{}, fmt.Errorf("%w: ip route get %s: %v", ErrUnderlay, to, err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.Join(strings.Fields(stderr.String()), " ")
		if msg == "" {
			msg = err.Error()
		}
		return Underlay{}, fmt.Errorf("%w: ip route get %s: %s", ErrUnderlay, to, msg)
	}
	u, err := ParseRouteGet(stdout.String())
	if err != nil {
		return Underlay{}, fmt.Errorf("toward %s: %w", to, err)
	}
	return u, nil
}

// EnsureLink makes vx<vni> as Link does, but leaves a device that is already as
// wanted alone (the helper re-makes the device, which drops the network's
// cross-host traffic for a moment): it reads the report and calls Link only
// when the device is missing, on another bridge, address or interface, or has
// other flood entries. changed says whether Link ran.
func (n *NetHelper) EnsureLink(vni uint32, bridge, localIP, dev string, peers []string) (changed bool, err error) {
	want := dedupSorted(peers)
	links, err := n.Report()
	if err != nil {
		return false, err
	}
	for _, l := range links {
		if l.VNI == vni && l.Bridge == bridge && l.Local == localIP && l.Dev == dev && reflect.DeepEqual(dedupSorted(l.Peers), want) {
			return false, nil
		}
	}
	if err := n.Link(vni, bridge, localIP, dev, peers); err != nil {
		return false, err
	}
	return true, nil
}

func dedupSorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	j := 0
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			out[j] = s
			j++
		}
	}
	return out[:j]
}
