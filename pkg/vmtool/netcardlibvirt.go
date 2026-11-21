package vmtool

import (
	"crypto/rand"
	"fmt"

	"libvirt.org/go/libvirt"
)

// The libvirt side of a VM's cards on vmtool networks (V2): thin glue over the
// pure functions in netcard.go. A reservation is a DHCP host entry added to or
// removed from the network with virNetworkUpdate (live when the network is
// active, and in the config); a card is attached or detached with
// virDomainAttach/DetachDeviceFlags (live when the VM is running or paused,
// and in the config). Nothing is written into the VM's definition but the card.

func netUpdateFlags(n *libvirt.Network) libvirt.NetworkUpdateFlags {
	f := libvirt.NETWORK_UPDATE_AFFECT_CONFIG
	if active, _ := n.IsActive(); active {
		f |= libvirt.NETWORK_UPDATE_AFFECT_LIVE
	}
	return f
}

// vmtoolNetwork looks up a network and refuses one that is not vmtool's.
func (m *Manager) vmtoolNetwork(name string) (*libvirt.Network, *NetworkInfo, error) {
	n, err := m.conn.LookupNetworkByName(name)
	if err != nil {
		return nil, nil, fmt.Errorf("network %q: %w", name, ErrNotFound)
	}
	info, err := netInfo(n, nil)
	if err != nil {
		n.Free()
		return nil, nil, err
	}
	if !info.Vmtool {
		n.Free()
		return nil, nil, fmt.Errorf("%w: network %q has bridge %q, not %s plus six lowercase hex digits; vmtool leaves it alone", ErrNetworkNotVmtool, name, info.Bridge, BridgePrefix)
	}
	return n, info, nil
}

// domainXMLs reads every domain's XML (for the MACs in use on the host).
func (m *Manager) domainXMLs() ([]string, error) {
	domains, err := m.conn.ListAllDomains(libvirt.CONNECT_LIST_DOMAINS_ACTIVE | libvirt.CONNECT_LIST_DOMAINS_INACTIVE)
	if err != nil {
		return nil, fmt.Errorf("listing domains: %w", err)
	}
	var out []string
	for _, dom := range domains {
		x, err := dom.GetXMLDesc(0)
		dom.Free()
		if err == nil {
			out = append(out, x)
		}
	}
	return out, nil
}

// AddReservation adds a DHCP host entry (MAC, address, name, a lease that never
// expires) to a vmtool network, live and in the config. An empty mac is chosen
// here (52:54:00 plus random bytes unused by any VM on the host and any
// reservation); an empty ip is the lowest free address of the DHCP range. See
// PlanReservation for what is refused.
func (m *Manager) AddReservation(network, vm, mac, ip string) (*Reservation, error) {
	n, info, err := m.vmtoolNetwork(network)
	if err != nil {
		return nil, err
	}
	defer n.Free()
	if mac == "" {
		xmls, err := m.domainXMLs()
		if err != nil {
			return nil, err
		}
		nets, err := m.allNetworkInfos()
		if err != nil {
			return nil, err
		}
		if mac, err = NewMAC(UsedMACs(xmls, nets), rand.Reader); err != nil {
			return nil, err
		}
	}
	var leased []string
	if leases, err := n.GetDHCPLeases(); err == nil {
		for _, l := range leases {
			leased = append(leased, l.IPaddr)
		}
	}
	res, err := PlanReservation(*info, vm, mac, ip, leased)
	if err != nil {
		return nil, err
	}
	if err := n.Update(libvirt.NETWORK_UPDATE_COMMAND_ADD_LAST, libvirt.NETWORK_SECTION_IP_DHCP_HOST, -1, ReservationXML(res), netUpdateFlags(n)); err != nil {
		return nil, fmt.Errorf("reserving %s for %s on network %q: %w", res.IP, res.MAC, network, err)
	}
	return &res, nil
}

// RemoveReservation removes the reservation of mac from a vmtool network, live
// and from the config. A MAC with none is an error wrapping ErrNotFound.
func (m *Manager) RemoveReservation(network, mac string) error {
	n, info, err := m.vmtoolNetwork(network)
	if err != nil {
		return err
	}
	defer n.Free()
	mac, err = NormMAC(mac)
	if err != nil {
		return err
	}
	for _, r := range info.Reservations {
		if r.MAC != mac {
			continue
		}
		if err := n.Update(libvirt.NETWORK_UPDATE_COMMAND_DELETE, libvirt.NETWORK_SECTION_IP_DHCP_HOST, -1, ReservationXML(r), netUpdateFlags(n)); err != nil {
			return fmt.Errorf("removing the reservation of %s from network %q: %w", mac, network, err)
		}
		return nil
	}
	return fmt.Errorf("network %q has no reservation for %s: %w", network, mac, ErrNotFound)
}

// deviceFlags: the config always, and the running domain when it is live.
func deviceFlags(dom *libvirt.Domain) (libvirt.DomainDeviceModifyFlags, error) {
	flags := libvirt.DOMAIN_DEVICE_MODIFY_CONFIG
	state, _, err := dom.GetState()
	if err != nil {
		return 0, fmt.Errorf("getting VM state: %w", err)
	}
	if stateIsLive(state) {
		flags |= libvirt.DOMAIN_DEVICE_MODIFY_LIVE
	}
	return flags, nil
}

// AttachInterface adds a virtio card with the given MAC on a vmtool network to
// the VM: live and in the config when it is running or paused, in the config
// only when it is shut off. It makes no reservation (the workflow does).
func (m *Manager) AttachInterface(vm, network, mac string) (*NetAttachment, error) {
	mac, err := NormMAC(mac)
	if err != nil {
		return nil, err
	}
	n, info, err := m.vmtoolNetwork(network)
	if err != nil {
		return nil, err
	}
	n.Free()
	dom, err := m.conn.LookupDomainByName(vm)
	if err != nil {
		return nil, fmt.Errorf("looking up domain %q: %w", vm, ErrNotFound)
	}
	defer dom.Free()
	x, err := dom.GetXMLDesc(0)
	if err != nil {
		return nil, fmt.Errorf("getting domain XML: %w", err)
	}
	cards, err := ParseDomainCards(x)
	if err != nil {
		return nil, err
	}
	for _, c := range cards {
		if c.MAC == mac {
			return nil, fmt.Errorf("%w: VM %q already has a card with MAC %s", ErrAddressTaken, vm, mac)
		}
	}
	flags, err := deviceFlags(dom)
	if err != nil {
		return nil, err
	}
	if err := dom.AttachDeviceFlags(InterfaceXML(network, mac), flags); err != nil {
		return nil, fmt.Errorf("attaching a card on network %q: %w", network, err)
	}
	return &Attachments([]DomainCard{{Type: "network", MAC: mac, Network: network}}, map[string]NetworkInfo{network: *info})[0], nil
}

// DetachInterface removes the VM's card with this MAC, live and from the config
// (the config only for a shut-off VM). Only a card on a vmtool network goes: the
// VM's own NAT card, and any other not on a vmtool network, is refused.
func (m *Manager) DetachInterface(vm, mac string) error {
	mac, err := NormMAC(mac)
	if err != nil {
		return err
	}
	dom, err := m.conn.LookupDomainByName(vm)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", vm, ErrNotFound)
	}
	defer dom.Free()
	x, err := dom.GetXMLDesc(0)
	if err != nil {
		return fmt.Errorf("getting domain XML: %w", err)
	}
	cards, err := ParseDomainCards(x)
	if err != nil {
		return err
	}
	var card *DomainCard
	for i := range cards {
		if cards[i].MAC == mac {
			card = &cards[i]
		}
	}
	if card == nil {
		return fmt.Errorf("VM %q has no card with MAC %s: %w", vm, mac, ErrNotFound)
	}
	if card.Network == "" {
		return fmt.Errorf("%w: the card %s of VM %q is not on a libvirt network; vmtool leaves it alone", ErrNetworkNotVmtool, mac, vm)
	}
	n, _, err := m.vmtoolNetwork(card.Network)
	if err != nil {
		return err
	}
	n.Free()
	flags, err := deviceFlags(dom)
	if err != nil {
		return err
	}
	if err := dom.DetachDeviceFlags(InterfaceXML(card.Network, mac), flags); err != nil {
		return fmt.Errorf("detaching the card %s: %w", mac, err)
	}
	return nil
}

// netDetails caches, for one Info or List call, the description (with its
// reservations) of each libvirt network a VM's card names.
type netDetails map[string]NetworkInfo

// vmNetworks lists the VM's cards on vmtool networks with their addresses. An
// error reading a network leaves its cards out: the listing is best effort.
func (m *Manager) vmNetworks(domXML string, cache netDetails) []NetAttachment {
	cards, err := ParseDomainCards(domXML)
	if err != nil {
		return nil
	}
	for _, c := range cards {
		if c.Network == "" {
			continue
		}
		if _, ok := cache[c.Network]; ok {
			continue
		}
		n, err := m.conn.LookupNetworkByName(c.Network)
		if err != nil {
			continue
		}
		if info, err := netInfo(n, nil); err == nil {
			cache[c.Network] = *info
		}
		n.Free()
	}
	return Attachments(cards, cache)
}
