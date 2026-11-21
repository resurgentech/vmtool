package remote

import (
	"context"
	"net/http"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/pkg/vmtool"
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
	return out
}

// CreateNetwork makes a vmtool network on the host (createNetwork).
func (m *Manager) CreateNetwork(name, subnet, domain string) (*vmtool.NetworkInfo, error) {
	var v api.NetworkInfo
	err := m.call("createNetwork", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreateNetwork(ctx, m.name, api.CreateNetworkRequest{
			Name: name, Subnet: nonEmpty(subnet), Domain: nonEmpty(domain),
		})
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
