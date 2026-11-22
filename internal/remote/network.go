package remote

import (
	"context"
	"net/http"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

func netFromAPI(n api.NetworkInfo) *vmtool.NetworkInfo {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	out := &vmtool.NetworkInfo{
		Name:      n.Name,
		Bridge:    str(n.Bridge),
		Vmtool:    n.Vmtool,
		Forward:   str(n.Forward),
		Subnet:    str(n.Subnet),
		Gateway:   str(n.Gateway),
		Domain:    str(n.Domain),
		DHCPStart: str(n.DhcpStart),
		DHCPEnd:   str(n.DhcpEnd),
		Active:    n.Active,
		Autostart: n.Autostart,
		VMs:       n.Vms,
	}
	if out.VMs == nil {
		out.VMs = []string{}
	}
	if n.Mtu != nil {
		out.MTU = *n.Mtu
	}
	if n.Vni != nil && *n.Vni >= 0 {
		out.VNI = uint32(*n.Vni)
	}
	if n.Reservations != nil {
		for _, r := range *n.Reservations {
			out.Reservations = append(out.Reservations, vmtool.Reservation{Network: r.Network, MAC: r.Mac, IP: r.Ip, Name: r.Name})
		}
	}
	return out
}

// AddReservation reserves an address and a name for a card on a vmtool network
// (createReservation); an empty mac or ip is chosen by the host.
func (m *Manager) AddReservation(network, vm, mac, ip string) (*vmtool.Reservation, error) {
	var v api.Reservation
	err := m.call("createReservation", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreateReservation(ctx, m.name, network, api.CreateReservationRequest{Vm: vm, Mac: nonEmpty(mac), Ip: nonEmpty(ip)})
	})
	if err != nil {
		return nil, err
	}
	return &vmtool.Reservation{Network: v.Network, MAC: v.Mac, IP: v.Ip, Name: v.Name}, nil
}

// RemoveReservation removes a card's reservation (deleteReservation).
func (m *Manager) RemoveReservation(network, mac string) error {
	return m.call("deleteReservation", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.DeleteReservation(ctx, m.name, network, mac)
	})
}

// AttachInterface adds a card with this MAC on a vmtool network to a VM
// (attachInterface).
func (m *Manager) AttachInterface(vm, network, mac string) (*vmtool.NetAttachment, error) {
	var v api.NetworkAttachment
	err := m.call("attachInterface", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.AttachInterface(ctx, m.name, vm, api.AttachInterfaceRequest{Network: network, Mac: mac})
	})
	if err != nil {
		return nil, err
	}
	return &vmtool.NetAttachment{Network: v.Network, MAC: v.Mac, IP: v.Ip}, nil
}

// DetachInterface removes the card with this MAC from a VM (detachInterface).
func (m *Manager) DetachInterface(vm, mac string) error {
	return m.call("detachInterface", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.DetachInterface(ctx, m.name, vm, mac)
	})
}

// CreateNetwork makes a vmtool network on the host (createNetwork).
func (m *Manager) CreateNetwork(name, subnet, domain string) (*vmtool.NetworkInfo, error) {
	return m.CreateNetworkVNI(name, subnet, domain, 0)
}

// CreateNetworkVNI is CreateNetwork on the bridge number the caller gives (the
// store registry's); 0 sends none and the host numbers the bridge itself. A
// daemon that predates the field ignores it: the caller compares the bridge
// of the network that comes back.
func (m *Manager) CreateNetworkVNI(name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	var v api.NetworkInfo
	req := api.CreateNetworkRequest{Name: name, Subnet: nonEmpty(subnet), Domain: nonEmpty(domain)}
	if vni != 0 {
		n := int(vni)
		req.Vni = &n
	}
	err := m.call("createNetwork", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreateNetwork(ctx, m.name, req)
	})
	if err != nil {
		return nil, err
	}
	return netFromAPI(v), nil
}

// Network describes one libvirt network on the host (getNetwork).
func (m *Manager) Network(name string) (*vmtool.NetworkInfo, error) {
	var v api.NetworkInfo
	err := m.call("getNetwork", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.GetNetwork(ctx, m.name, name)
	})
	if err != nil {
		return nil, err
	}
	return netFromAPI(v), nil
}

// DeleteNetwork removes a vmtool network from the host (deleteNetwork).
func (m *Manager) DeleteNetwork(name string) error {
	return m.call("deleteNetwork", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.DeleteNetwork(ctx, m.name, name)
	})
}

// CreateNetworkMember makes the member form of a network that spans hosts: the
// network's name, bridge vmt-<vni hex> and the MTU (createNetwork with member).
// A daemon that predates the field would make a home-form network: the caller
// compares what comes back.
func (m *Manager) CreateNetworkMember(name string, vni uint32) (*vmtool.NetworkInfo, error) {
	var v api.NetworkInfo
	n := int(vni)
	member := true
	req := api.CreateNetworkRequest{Name: name, Vni: &n, Member: &member}
	err := m.call("createNetwork", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreateNetwork(ctx, m.name, req)
	})
	if err != nil {
		return nil, err
	}
	return netFromAPI(v), nil
}

// SetNetworkLinks links the host's network to the hosts at peers
// (setNetworkPeers).
func (m *Manager) SetNetworkLinks(name, localIP, dev string, peers []string) error {
	if peers == nil {
		peers = []string{}
	}
	return m.call("setNetworkPeers", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.SetNetworkPeers(ctx, m.name, name, api.NetworkPeers{LanIp: localIP, LanDev: dev, Peers: peers})
	})
}

// RemoveNetworkLink removes the host's link of the network (deleteNetworkPeers).
func (m *Manager) RemoveNetworkLink(name string) error {
	return m.call("deleteNetworkPeers", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.DeleteNetworkPeers(ctx, m.name, name)
	})
}

// Underlay asks the host which of its addresses and interfaces reach the
// address to (getUnderlay).
func (m *Manager) Underlay(to string) (*vmtool.Underlay, error) {
	var v api.Underlay
	err := m.call("getUnderlay", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.GetUnderlay(ctx, m.name, &api.GetUnderlayParams{To: to})
	})
	if err != nil {
		return nil, err
	}
	return &vmtool.Underlay{IP: v.Ip, Dev: v.Dev}, nil
}
