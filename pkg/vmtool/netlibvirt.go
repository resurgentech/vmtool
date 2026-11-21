package vmtool

import (
	"fmt"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"libvirt.org/go/libvirt"
)

// The libvirt side of named networks (V1): thin glue over the pure functions
// in netdef.go. Nothing here runs ip, bridge, tc, virsh or sudo; the host's
// routes are read from /proc/net/route, the kernel's own table.

// routeTable returns the text of the host's IPv4 route table. A variable so
// tests of the choice logic never read the real one (they call PlanNetwork
// with routes they give).
var routeTable = func() (string, error) {
	b, err := os.ReadFile("/proc/net/route")
	return string(b), err
}

func hostRoutes() ([]netip.Prefix, error) {
	s, err := routeTable()
	if err != nil {
		return nil, fmt.Errorf("reading the host's routes: %w", err)
	}
	return ParseProcNetRoute(s)
}

// netAttachments maps a libvirt network's name to the VMs with a card on it.
func (m *Manager) netAttachments() (map[string][]string, error) {
	domains, err := m.conn.ListAllDomains(libvirt.CONNECT_LIST_DOMAINS_ACTIVE | libvirt.CONNECT_LIST_DOMAINS_INACTIVE)
	if err != nil {
		return nil, fmt.Errorf("listing domains: %w", err)
	}
	out := map[string][]string{}
	for _, dom := range domains {
		name, nerr := dom.GetName()
		x, xerr := dom.GetXMLDesc(0)
		dom.Free()
		if nerr != nil || xerr != nil {
			continue
		}
		_, nets, err := parseDomainNICs(x)
		if err != nil {
			continue
		}
		for _, n := range nets {
			out[n] = append(out[n], name)
		}
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out, nil
}

func netInfo(n *libvirt.Network, attached map[string][]string) (*NetworkInfo, error) {
	x, err := n.GetXMLDesc(0)
	if err != nil {
		return nil, fmt.Errorf("reading network definition: %w", err)
	}
	info, err := ParseNetworkXML(x)
	if err != nil {
		return nil, err
	}
	info.Active, _ = n.IsActive()
	info.Autostart, _ = n.GetAutostart()
	info.VMs = attached[info.Name]
	if info.VMs == nil {
		info.VMs = []string{}
	}
	return info, nil
}

// allNetworkInfos reads every libvirt network, active or not.
func (m *Manager) allNetworkInfos() ([]NetworkInfo, error) {
	nets, err := m.conn.ListAllNetworks(0)
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	attached, err := m.netAttachments()
	if err != nil {
		return nil, err
	}
	var out []NetworkInfo
	for i := range nets {
		info, err := netInfo(&nets[i], attached)
		nets[i].Free()
		if err != nil {
			return nil, err
		}
		out = append(out, *info)
	}
	return out, nil
}

// Network returns one libvirt network by name (any, vmtool's or not).
func (m *Manager) Network(name string) (*NetworkInfo, error) {
	n, err := m.conn.LookupNetworkByName(name)
	if err != nil {
		return nil, fmt.Errorf("network %q: %w", name, ErrNotFound)
	}
	defer n.Free()
	attached, err := m.netAttachments()
	if err != nil {
		return nil, err
	}
	return netInfo(n, attached)
}

// CreateNetwork defines, autostarts and starts a vmtool network on this host.
// An empty subnet is the first free /24 of NetworkBaseCIDR; an empty domain is
// <name>.test. See PlanNetwork for what is refused.
func (m *Manager) CreateNetwork(name, subnet, domain string) (*NetworkInfo, error) {
	return m.CreateNetworkVNI(name, subnet, domain, 0)
}

// CreateNetworkVNI is CreateNetwork on the bridge number the caller gives
// (vmt- plus vni as six hex digits), which the store's registry allocates; 0
// is the host's own lowest free number from 1. A bridge of that number that
// the host already has is refused (ErrNetworkExists).
func (m *Manager) CreateNetworkVNI(name, subnet, domain string, vni uint32) (*NetworkInfo, error) {
	existing, err := m.allNetworkInfos()
	if err != nil {
		return nil, err
	}
	routes, err := hostRoutes()
	if err != nil {
		return nil, err
	}
	spec, err := PlanNetwork(name, subnet, domain, existing, routes, vni)
	if err != nil {
		return nil, err
	}
	x, err := BuildNetworkXML(spec)
	if err != nil {
		return nil, err
	}
	n, err := m.conn.NetworkDefineXML(x)
	if err != nil {
		return nil, fmt.Errorf("defining network %q: %w", name, err)
	}
	defer n.Free()
	undo := func() { _ = n.Undefine() }
	if err := n.SetAutostart(true); err != nil {
		undo()
		return nil, fmt.Errorf("setting network %q to autostart: %w", name, err)
	}
	if err := n.Create(); err != nil {
		undo()
		return nil, fmt.Errorf("starting network %q: %w", name, err)
	}
	return netInfo(n, nil)
}

// DeleteNetwork destroys (when active) and undefines a vmtool network. It
// refuses a network whose bridge is not vmtool's and one with VMs attached
// (CheckNetworkDeletable).
func (m *Manager) DeleteNetwork(name string) error {
	info, err := m.Network(name)
	if err != nil {
		return err
	}
	if err := CheckNetworkDeletable(*info); err != nil {
		return err
	}
	n, err := m.conn.LookupNetworkByName(name)
	if err != nil {
		return fmt.Errorf("network %q: %w", name, ErrNotFound)
	}
	defer n.Free()
	if active, _ := n.IsActive(); active {
		if err := n.Destroy(); err != nil {
			return fmt.Errorf("stopping network %q: %w", name, err)
		}
	}
	if err := n.Undefine(); err != nil {
		return fmt.Errorf("undefining network %q: %w", name, err)
	}
	return nil
}

// The member form and the links of networks that span hosts (V5).

// CreateNetworkMember defines, autostarts and starts the member form of the
// network that has VNI vni: the home's name, bridge vmt-<vni hex> and the MTU,
// with no address of its own. A name or a bridge the host already has is
// refused (ErrNetworkExists).
func (m *Manager) CreateNetworkMember(name string, vni uint32) (*NetworkInfo, error) {
	existing, err := m.allNetworkInfos()
	if err != nil {
		return nil, err
	}
	spec, err := PlanMemberNetwork(name, vni, existing)
	if err != nil {
		return nil, err
	}
	x, err := BuildMemberNetworkXML(spec.Name, spec.Bridge)
	if err != nil {
		return nil, err
	}
	n, err := m.conn.NetworkDefineXML(x)
	if err != nil {
		return nil, fmt.Errorf("defining network %q: %w", name, err)
	}
	defer n.Free()
	undo := func() { _ = n.Undefine() }
	if err := n.SetAutostart(true); err != nil {
		undo()
		return nil, fmt.Errorf("setting network %q to autostart: %w", name, err)
	}
	if err := n.Create(); err != nil {
		undo()
		return nil, fmt.Errorf("starting network %q: %w", name, err)
	}
	return netInfo(n, nil)
}

// SetNetworkLinks makes this host's VXLAN device of the vmtool network name
// (vx<VNI>, on the network's bridge) bound to localIP on dev and flooding to
// exactly peers. A device already so is left alone. The network must be a
// vmtool network and active: its bridge is made when libvirt starts it.
func (m *Manager) SetNetworkLinks(name, localIP, dev string, peers []string) error {
	info, err := m.Network(name)
	if err != nil {
		return err
	}
	if !info.Vmtool {
		return fmt.Errorf("%w: network %q has bridge %q, not %s plus six lowercase hex digits; vmtool leaves it alone", ErrNetworkNotVmtool, name, info.Bridge, BridgePrefix)
	}
	if !info.Active {
		return fmt.Errorf("network %q is not active, so its bridge %s does not exist yet; libvirt starts it (autostart) and vmtool links it then", name, info.Bridge)
	}
	_, err = (&NetHelper{}).EnsureLink(info.VNI, info.Bridge, localIP, dev, peers)
	return err
}

// RemoveNetworkLink removes this host's VXLAN device of the vmtool network
// name; none there is success.
func (m *Manager) RemoveNetworkLink(name string) error {
	info, err := m.Network(name)
	if err != nil {
		return err
	}
	if !info.Vmtool {
		return fmt.Errorf("%w: network %q has bridge %q, not %s plus six lowercase hex digits; vmtool leaves it alone", ErrNetworkNotVmtool, name, info.Bridge, BridgePrefix)
	}
	return (&NetHelper{}).Unlink(info.VNI)
}

// Underlay is the address and interface this host reaches the IPv4 address to
// with (ip route get).
func (m *Manager) Underlay(to string) (*Underlay, error) {
	u, err := FindUnderlay(to)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

var (
	eventLoopOnce sync.Once
	eventLoopErr  error
)

// StartEventLoop registers libvirt's default event loop and runs it in the
// background, once. libvirt wants the registration before the process opens its
// first connection, so the daemon calls it before anything else touches libvirt;
// WatchNetworkStarted calls it too (a second call does nothing).
func StartEventLoop() error { return startEventLoop() }

func startEventLoop() error {
	eventLoopOnce.Do(func() {
		if eventLoopErr = libvirt.EventRegisterDefaultImpl(); eventLoopErr != nil {
			return
		}
		go func() {
			for {
				if err := libvirt.EventRunDefaultImpl(); err != nil {
					time.Sleep(time.Second)
				}
			}
		}()
	})
	return eventLoopErr
}

// WatchNetworkStarted calls fn with the name of each libvirt network that
// starts (the bridge is made again, so a VXLAN device on it must be linked
// again). It opens a connection of its own; stop deregisters and closes it. fn
// runs on libvirt's event goroutine and must not block.
func WatchNetworkStarted(fn func(name string)) (stop func(), err error) {
	if err := startEventLoop(); err != nil {
		return nil, fmt.Errorf("libvirt event loop: %w", err)
	}
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		return nil, fmt.Errorf("connecting to libvirt: %w", err)
	}
	id, err := conn.NetworkEventLifecycleRegister(nil, func(_ *libvirt.Connect, n *libvirt.Network, ev *libvirt.NetworkEventLifecycle) {
		if ev.Event != libvirt.NETWORK_EVENT_STARTED {
			return
		}
		if name, err := n.GetName(); err == nil {
			fn(name)
		}
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("watching libvirt networks: %w", err)
	}
	return func() {
		_ = conn.NetworkEventDeregister(id)
		_, _ = conn.Close()
	}, nil
}
