package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// Networks that span hosts (tranche 17, V5): join and leave, and the push of
// the peer list. A network's home does the NAT, DHCP and DNS; a member host
// defines the same network with only a bridge and the MTU and is linked to the
// home and to every other member by a VXLAN device. The registry (the store)
// holds who is a member and each member's LAN address; spanning networks need
// it, so join and leave refuse in local mode.
//
// Nothing requires every member to be awake. Only the host that joins or leaves
// must answer; every other member is told the new peer list if it answers and
// skipped, with one line, if not: it reads the registry when its daemon starts.
//
// The home is a row of the registry too (its LAN address and interface), made
// the first time a host joins, so every host's peers are the other rows.

// NetMember is what a host's Manager offers for networks that span hosts. It is
// optional: a daemon that predates V5 has none (the workflows then ask for a
// redeploy). The daemon's routes are these primitives; the order and the undo
// live here.
type NetMember interface {
	// CreateNetworkMember defines the member form of the network that has this
	// VNI: its name, bridge vmt-<VNI hex> and the MTU, nothing else.
	CreateNetworkMember(name string, vni uint32) (*vmtool.NetworkInfo, error)
	// SetNetworkLinks makes this host's VXLAN device of the network flood to
	// exactly peers, bound to localIP on dev.
	SetNetworkLinks(name, localIP, dev string, peers []string) error
	// RemoveNetworkLink removes this host's VXLAN device of the network.
	RemoveNetworkLink(name string) error
	// Underlay is the address and interface this host reaches the address to
	// with.
	Underlay(to string) (*vmtool.Underlay, error)
}

// Unwrapper is a Manager wrapper that can give up the Manager it wraps, so an
// optional capability (NetMember) of the inner one stays reachable.
type Unwrapper interface{ Unwrap() Manager }

// AsNetMember finds the NetMember capability of m or of a Manager it wraps.
func AsNetMember(m Manager) (NetMember, bool) {
	for m != nil {
		if nm, ok := m.(NetMember); ok {
			return nm, true
		}
		u, ok := m.(Unwrapper)
		if !ok {
			return nil, false
		}
		m = u.Unwrap()
	}
	return nil, false
}

// JoinResult is what a join did.
type JoinResult struct {
	Network string
	Host    string
	Home    string
	VNI     uint32
	LanIP   string // the joined host's address toward the home
	LanDev  string
	// Registered is true when this join registered the network (it was made
	// without a store, on a bridge number of 1 to 9999).
	Registered bool
	// Peers are the LAN addresses the joined host floods to.
	Peers []string
	// Skipped has one line for each member that did not answer the new peer
	// list; it takes the list from the registry when it wakes.
	Skipped []string
}

// LeaveResult is what a leave did.
type LeaveResult struct {
	Network string
	Host    string
	Skipped []string
}

func (s *Service) openHost(host string) (Manager, func(), error) {
	if s.OpenHost == nil {
		return nil, nil, fmt.Errorf("this command has no way to reach host %q", host)
	}
	m, release, err := s.OpenHost(host)
	if err != nil {
		return nil, nil, err
	}
	if release == nil {
		release = func() {}
	}
	return m, release, nil
}

// lookupHost resolves a configured host address; a variable so tests stay off
// the network.
var lookupHost = net.LookupHost

// hostIPv4 is the IPv4 address of a configured host: its address if that is
// one, else the first IPv4 its name resolves to.
func hostIPv4(addr string) (string, error) {
	if a, err := netip.ParseAddr(addr); err == nil {
		if a.Is4() {
			return a.String(), nil
		}
		return "", fmt.Errorf("address %s is not IPv4", addr)
	}
	ips, err := lookupHost(addr)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("%s has no IPv4 address", addr)
}

// peerIPs is the LAN addresses of every member but host, sorted.
func peerIPs(members []storeclient.NetworkMember, host string) []string {
	out := []string{}
	for _, m := range members {
		if m.Host != host {
			out = append(out, m.LanIp)
		}
	}
	sort.Strings(out)
	return out
}

func memberRow(members []storeclient.NetworkMember, host string) *storeclient.NetworkMember {
	for i := range members {
		if members[i].Host == host {
			return &members[i]
		}
	}
	return nil
}

// undoCtx is a context for an undo step: the command's may be what failed.
func undoCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), CredTimeout)
}

// registryNetwork reads one registered network; nil, nil when it is not.
func (s *Service) registryNetwork(ctx context.Context, op string, c *storeclient.Client, name string) (*storeclient.Network, error) {
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	n, err := c.GetNetwork(cctx, name)
	switch {
	case errors.Is(err, storeclient.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, unavailable(op, fmt.Errorf("the registry at %s could not be read: %v", s.Store.Host(), err))
	}
	return n, nil
}

func (s *Service) needRegistry(ctx context.Context, op, what string) (*storeclient.Client, error) {
	c := s.Store.registryClient(ctx)
	if c == nil {
		return nil, invalid(op, fmt.Errorf("%s needs the store: a network that spans hosts is registered there, and without it no host can learn the others' addresses; this command is in local mode (no store, or one that does not answer)", what))
	}
	return c, nil
}

// JoinNetwork makes host a member of the vmtool network name: the host defines
// the bridge-only network (the home's name, bridge vmt-<VNI hex>, the MTU) and
// links to the home and every other member; every awake member and the home get
// the new peer list.
//
// The registry must have the network. One made without a store (bridge number
// 1 to 9999) is registered by its first join, with its bridge number as its VNI
// and its subnet and domain as they are on home (so home must say which host
// that is); a number the registry already has is refused ("recreate it with a
// store"). A step that fails takes back the ones before it.
func (s *Service) JoinNetwork(ctx context.Context, host, name, home string) (*JoinResult, error) {
	const op = "net join"
	if !vmtool.ValidNetworkName(name) {
		return nil, invalid(op, fmt.Errorf("network name %q: letters, digits, '.', '_', '-', up to 63 characters, and not \"default\"", name))
	}
	if host == "" {
		return nil, invalid(op, errors.New("a host is required"))
	}
	c, err := s.needRegistry(ctx, op, "joining a network")
	if err != nil {
		return nil, err
	}
	rn, err := s.registryNetwork(ctx, op, c, name)
	if err != nil {
		return nil, err
	}
	switch {
	case rn != nil && host == rn.Home:
		return nil, conflict(op, fmt.Errorf("%s is the home of network %q: it has the network already", host, name))
	case rn != nil && home != "" && home != rn.Home:
		return nil, invalid(op, fmt.Errorf("network %q is registered with home %s, not %s", name, rn.Home, home))
	case rn == nil && home == "":
		return nil, invalid(op, fmt.Errorf("network %q is not in the registry (a network made without a store is registered by its first join); say which host is its home with --home HOST", name))
	case rn == nil && home == host:
		return nil, invalid(op, fmt.Errorf("%s is the home: it does not join its own network", host))
	}

	mh, release, err := s.openHost(host)
	if err != nil {
		return nil, unavailable(op, fmt.Errorf("host %s does not answer: %v", host, err))
	}
	defer release()
	nm, ok := AsNetMember(mh)
	if !ok {
		return nil, netErr(op, errOldHost(host))
	}
	local, err := mh.Network(name)
	switch {
	case err == nil && !local.Vmtool:
		return nil, conflict(op, fmt.Errorf("%w: host %s has a libvirt network %q with bridge %q, not %s plus six lowercase hex digits; vmtool leaves it alone", vmtool.ErrNetworkNotVmtool, host, name, local.Bridge, vmtool.BridgePrefix))
	case err == nil && !local.IsMemberForm():
		return nil, conflict(op, fmt.Errorf("host %s has a network %q with its own address (%s): it is a home or a one-host network, not a member of one", host, name, local.Subnet))
	case err != nil && !vmtool.IsNotFound(err):
		return nil, netErr(op, err)
	case err != nil:
		local = nil
	}

	var undoFns []func(context.Context)
	undoAdd := func(f func(context.Context)) { undoFns = append(undoFns, f) }
	fail := func(err error) (*JoinResult, error) {
		uctx, cancel := undoCtx(ctx)
		defer cancel()
		for i := len(undoFns) - 1; i >= 0; i-- {
			undoFns[i](uctx)
		}
		return nil, err
	}

	registered := false
	if rn == nil {
		if rn, err = s.registerLocalNetwork(ctx, op, c, host, name, home); err != nil {
			return nil, err
		}
		registered = true
		undoAdd(func(uctx context.Context) { _ = c.DeleteNetwork(uctx, name) })
	}
	vni := uint32(rn.Vni)
	home = rn.Home
	if local != nil && local.Bridge != vmtool.BridgeName(vni) {
		return fail(conflict(op, fmt.Errorf("host %s has network %q on bridge %s, but the registry's network %q is VNI %d (bridge %s)", host, name, local.Bridge, name, vni, vmtool.BridgeName(vni))))
	}

	// Where this host reaches the home from: the address the home was last
	// registered at, else the one config.yml gives its daemon.
	homeRow := memberRow(rn.Members, home)
	homeAddr := ""
	if homeRow != nil {
		homeAddr = homeRow.LanIp
	} else if a := s.hostAddr(home); a != "" {
		if homeAddr, err = hostIPv4(a); err != nil {
			return fail(invalid(op, fmt.Errorf("the address of host %s in config.yml: %v", home, err)))
		}
	} else {
		return fail(invalid(op, fmt.Errorf("host %s has no address in config.yml hosts:; add it so %s can find its route to the home", home, host)))
	}
	mine, err := nm.Underlay(homeAddr)
	if err != nil {
		return fail(netErr(op, fmt.Errorf("host %s: finding its address toward %s: %w", host, homeAddr, err)))
	}

	// The first join also registers the home's own address, found from the home
	// toward this host's: the home must answer now.
	if homeRow == nil {
		hm, hrelease, err := s.openHost(home)
		if err != nil {
			return fail(unavailable(op, fmt.Errorf("the home %s of network %q does not answer: %v; the first host to join needs it (it gives the home's LAN address)", home, name, err)))
		}
		defer hrelease()
		hnm, ok := AsNetMember(hm)
		if !ok {
			return fail(netErr(op, errOldHost(home)))
		}
		theirs, err := hnm.Underlay(mine.IP)
		if err != nil {
			return fail(netErr(op, fmt.Errorf("home %s: finding its address toward %s: %w", home, mine.IP, err)))
		}
		cctx, cancel := s.credCtx(ctx)
		_, err = c.PutNetworkMember(cctx, name, home, storeclient.NetworkMemberInput{LanIp: theirs.IP, LanDev: theirs.Dev})
		cancel()
		if err != nil {
			return fail(unavailable(op, fmt.Errorf("the registry at %s could not record home %s: %v", s.Store.Host(), home, err)))
		}
		undoAdd(func(uctx context.Context) { _ = c.DeleteNetworkMember(uctx, name, home) })
	}

	// The member's own libvirt network.
	if local == nil {
		n, err := nm.CreateNetworkMember(name, vni)
		if err != nil {
			return fail(netErr(op, err))
		}
		undoAdd(func(uctx context.Context) { _ = mh.DeleteNetwork(name) })
		if !n.IsMemberForm() || n.Bridge != vmtool.BridgeName(vni) {
			return fail(netErr(op, errOldHost(host)))
		}
	}
	// Then its row.
	hadRow := memberRow(rn.Members, host) != nil
	cctx, cancel := s.credCtx(ctx)
	_, err = c.PutNetworkMember(cctx, name, host, storeclient.NetworkMemberInput{LanIp: mine.IP, LanDev: mine.Dev})
	cancel()
	if err != nil {
		return fail(unavailable(op, fmt.Errorf("the registry at %s could not record %s: %v", s.Store.Host(), host, err)))
	}
	if !hadRow {
		undoAdd(func(uctx context.Context) { _ = c.DeleteNetworkMember(uctx, name, host) })
	}
	cur, err := s.registryNetwork(ctx, op, c, name)
	if err != nil {
		return fail(err)
	}
	if cur == nil {
		return fail(unavailable(op, fmt.Errorf("the registry lost network %q while it was being joined", name)))
	}
	peers := peerIPs(cur.Members, host)
	if err := nm.SetNetworkLinks(name, mine.IP, mine.Dev, peers); err != nil {
		undoAdd(func(context.Context) { _ = nm.RemoveNetworkLink(name) })
		return fail(netErr(op, fmt.Errorf("linking %s to network %q: %w", host, name, err)))
	}

	res := &JoinResult{Network: name, Host: host, Home: home, VNI: vni, LanIP: mine.IP, LanDev: mine.Dev, Registered: registered, Peers: peers}
	res.Skipped = s.pushPeers(ctx, name, cur.Members, host)
	return res, nil
}

func (s *Service) hostAddr(host string) string {
	if s.HostAddr == nil {
		return ""
	}
	return s.HostAddr(host)
}

// registerLocalNetwork registers a network made without a store: it reads the
// network on home (a vmtool home form with a bridge number) and registers it
// with that number as its VNI, its subnet and its domain. Any reservation the
// home already has is recorded with it (its VMs are the home's). A number, name
// or subnet the registry already has is refused.
func (s *Service) registerLocalNetwork(ctx context.Context, op string, c *storeclient.Client, host, name, home string) (*storeclient.Network, error) {
	hm, hrelease, err := s.openHost(home)
	if err != nil {
		return nil, unavailable(op, fmt.Errorf("the home %s of network %q does not answer: %v", home, name, err))
	}
	defer hrelease()
	hn, err := hm.Network(name)
	switch {
	case err != nil && vmtool.IsNotFound(err):
		return nil, &Error{Kind: KindNotFound, Op: op, Err: fmt.Errorf("home %s has no network %q: %w", home, name, vmtool.ErrNotFound)}
	case err != nil:
		return nil, netErr(op, err)
	case !hn.Vmtool:
		return nil, conflict(op, fmt.Errorf("%w: network %q on %s has bridge %q, not %s plus six lowercase hex digits", vmtool.ErrNetworkNotVmtool, name, home, hn.Bridge, vmtool.BridgePrefix))
	case hn.IsMemberForm():
		return nil, conflict(op, fmt.Errorf("network %q on %s has no address of its own: it is a member of another network, not a home", name, home))
	}
	vni := int(hn.VNI)
	in := storeclient.NetworkInput{Name: name, Home: home, Vni: &vni}
	if hn.Subnet != "" {
		sub := hn.Subnet
		in.Subnet = &sub
	}
	if hn.Domain != "" {
		d := hn.Domain
		in.Domain = &d
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	rn, err := c.CreateNetwork(cctx, in)
	switch {
	case errors.Is(err, storeclient.ErrRegistryConflict):
		return nil, conflict(op, fmt.Errorf("network %q on %s uses bridge number %d (%s) and subnet %s, and the registry at %s already has that number or subnet, or the name, for another network; recreate it with a store (vmtool net delete %s %s, then vmtool net create %s %s)",
			name, home, hn.VNI, hn.Bridge, hn.Subnet, s.Store.Host(), home, name, home, name))
	case err != nil:
		return nil, unavailable(op, fmt.Errorf("the registry at %s could not register network %q: %v", s.Store.Host(), name, err))
	}
	for _, r := range hn.Reservations {
		if _, err := c.PutNetworkReservation(cctx, name, r.MAC, storeclient.NetworkReservationInput{Ip: r.IP, Host: home, Vm: r.Name}); err != nil {
			s.noter(nil)(fmt.Sprintf("the reservation of %s (%s) on network %q could not be recorded in the registry: %v", r.IP, r.MAC, name, err))
		}
	}
	return rn, nil
}

// pushPeers tells every member of members but except its new peer list (the
// other members' LAN addresses). A member that does not answer in time, is not
// open to us or refuses is skipped with one line: its daemon reads the registry
// when it starts. The pushes run together, each under ListTimeout.
func (s *Service) pushPeers(ctx context.Context, name string, members []storeclient.NetworkMember, except string) []string {
	var targets []storeclient.NetworkMember
	for _, m := range members {
		if m.Host != except {
			targets = append(targets, m)
		}
	}
	lines := make([]string, len(targets))
	var wg sync.WaitGroup
	for i, m := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan string, 1)
			go func() { done <- s.pushOne(name, m, peerIPs(members, m.Host)) }()
			select {
			case l := <-done:
				lines[i] = l
			case <-time.After(ListTimeout):
				lines[i] = skipLine(m.Host, timeoutErr(ListTimeout))
			case <-ctx.Done():
				lines[i] = skipLine(m.Host, ctx.Err())
			}
		}()
	}
	wg.Wait()
	var out []string
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func skipLine(host string, err error) string {
	return fmt.Sprintf("%s was not told the new peer list (%v); it takes it from the registry when it wakes", host, err)
}

func (s *Service) pushOne(name string, m storeclient.NetworkMember, peers []string) string {
	hm, release, err := s.openHost(m.Host)
	if err != nil {
		return skipLine(m.Host, err)
	}
	defer release()
	nm, ok := AsNetMember(hm)
	if !ok {
		return skipLine(m.Host, errOldHost(m.Host))
	}
	if err := nm.SetNetworkLinks(name, m.LanIp, m.LanDev, peers); err != nil {
		return skipLine(m.Host, err)
	}
	return ""
}

// LeaveNetwork takes host out of the network name: it unlinks, undefines the
// member network and removes the host from the registry, then tells every awake
// member and the home the shorter peer list. It is refused while a VM of the
// host has a card on the network, and for the home (delete the network
// instead). A host that no longer has the libvirt network but is still in the
// registry is only removed from it.
func (s *Service) LeaveNetwork(ctx context.Context, host, name string) (*LeaveResult, error) {
	const op = "net leave"
	if !vmtool.ValidNetworkName(name) {
		return nil, invalid(op, fmt.Errorf("network name %q: letters, digits, '.', '_', '-', up to 63 characters, and not \"default\"", name))
	}
	c, err := s.needRegistry(ctx, op, "leaving a network")
	if err != nil {
		return nil, err
	}
	rn, err := s.registryNetwork(ctx, op, c, name)
	if err != nil {
		return nil, err
	}
	if rn == nil {
		return nil, &Error{Kind: KindNotFound, Op: op, Err: fmt.Errorf("network %q is not in the registry: %w", name, vmtool.ErrNotFound)}
	}
	if host == rn.Home {
		return nil, conflict(op, fmt.Errorf("%s is the home of network %q, not a member; delete the network instead (vmtool net delete %s %s)", host, name, host, name))
	}
	row := memberRow(rn.Members, host)
	mh, release, err := s.openHost(host)
	if err != nil {
		return nil, unavailable(op, fmt.Errorf("host %s does not answer: %v", host, err))
	}
	defer release()
	nm, ok := AsNetMember(mh)
	if !ok {
		return nil, netErr(op, errOldHost(host))
	}
	local, err := mh.Network(name)
	switch {
	case err == nil && !local.IsMemberForm():
		return nil, conflict(op, fmt.Errorf("host %s has a network %q that is not the member form (%s): vmtool leaves it alone", host, name, local.Bridge))
	case err == nil && len(local.VMs) > 0:
		return nil, conflict(op, fmt.Errorf("%w: host %s has VMs on network %q: %s; detach them first (vmtool net detach HOST/VM %s)", vmtool.ErrNetworkInUse, host, name, strings.Join(local.VMs, ", "), name))
	case err != nil && !vmtool.IsNotFound(err):
		return nil, netErr(op, err)
	case err != nil:
		local = nil
	}
	if local == nil && row == nil {
		return nil, &Error{Kind: KindNotFound, Op: op, Err: fmt.Errorf("%s is not a member of network %q: %w", host, name, vmtool.ErrNotFound)}
	}
	if local != nil {
		if err := nm.RemoveNetworkLink(name); err != nil {
			return nil, netErr(op, fmt.Errorf("unlinking %s from %q: %w", host, name, err))
		}
		if err := mh.DeleteNetwork(name); err != nil {
			// Put the link back so the host is as it was.
			if row != nil {
				_ = nm.SetNetworkLinks(name, row.LanIp, row.LanDev, peerIPs(rn.Members, host))
			}
			return nil, netErr(op, err)
		}
	}
	if row != nil {
		cctx, cancel := s.credCtx(ctx)
		err := c.DeleteNetworkMember(cctx, name, host)
		cancel()
		if err != nil && !errors.Is(err, storeclient.ErrNotFound) {
			return nil, unavailable(op, fmt.Errorf("%s left network %q, but the registry at %s could not remove it: %v; run vmtool net leave %s %s again", host, name, s.Store.Host(), err, host, name))
		}
	}
	res := &LeaveResult{Network: name, Host: host}
	cur, err := s.registryNetwork(ctx, op, c, name)
	if err != nil || cur == nil {
		res.Skipped = append(res.Skipped, fmt.Sprintf("the other members were not told: the registry could not be read again (%v)", err))
		return res, nil
	}
	res.Skipped = s.pushPeers(ctx, name, cur.Members, host)
	return res, nil
}

// The daemon's primitives for networks that span hosts, for its routes: each
// asks the host and sorts its refusal into the kind REST maps (see netErr). The
// workflows above compose them.

func noMember(op string) error {
	return invalid(op, errors.New("this host cannot do that: its vmtool predates networks that span hosts; redeploy it"))
}

// CreateNetworkMember makes the member form of the network with this VNI: its
// name, bridge vmt-<VNI hex> and the MTU, nothing else.
func (s *Service) CreateNetworkMember(_ context.Context, m Manager, name string, vni uint32) (*vmtool.NetworkInfo, error) {
	const op = "net create"
	if !vmtool.ValidNetworkName(name) {
		return nil, invalid(op, fmt.Errorf("network name %q: letters, digits, '.', '_', '-', up to 63 characters, and not \"default\"", name))
	}
	nm, ok := AsNetMember(m)
	if !ok {
		return nil, noMember(op)
	}
	n, err := nm.CreateNetworkMember(name, vni)
	if err != nil {
		return nil, netErr(op, err)
	}
	return n, nil
}

// SetNetworkPeers links the host's network to the hosts at peers: its VXLAN
// device bound to lanIP on dev floods to exactly those addresses.
func (s *Service) SetNetworkPeers(_ context.Context, m Manager, name, lanIP, dev string, peers []string) error {
	const op = "net peers"
	if name == "" || lanIP == "" || dev == "" {
		return invalid(op, errors.New("a network, a lan_ip and a lan_dev are required"))
	}
	nm, ok := AsNetMember(m)
	if !ok {
		return noMember(op)
	}
	return netErr(op, nm.SetNetworkLinks(name, lanIP, dev, peers))
}

// DeleteNetworkPeers removes the host's link of the network.
func (s *Service) DeleteNetworkPeers(_ context.Context, m Manager, name string) error {
	const op = "net unlink"
	if name == "" {
		return invalid(op, errors.New("a network name is required"))
	}
	nm, ok := AsNetMember(m)
	if !ok {
		return noMember(op)
	}
	return netErr(op, nm.RemoveNetworkLink(name))
}

// Underlay is the address and interface the host reaches the address to with.
func (s *Service) Underlay(_ context.Context, m Manager, to string) (*vmtool.Underlay, error) {
	const op = "underlay"
	nm, ok := AsNetMember(m)
	if !ok {
		return nil, noMember(op)
	}
	u, err := nm.Underlay(to)
	if err != nil {
		return nil, netErr(op, err)
	}
	return u, nil
}

// NetworkShow is a network as `net show` shows it, with its orphaned
// reservations that were not removed.
type NetworkShow struct {
	Info    *vmtool.NetworkInfo
	Orphans []storeclient.NetworkReservation
}

// ShowNetwork describes the network as the host has it (Network) and, in
// store mode, first removes the network's orphaned reservations when the home
// answers (removeOrphans); the orphans it could not remove are listed. A home
// that does not answer changes nothing and is not an error of a show.
func (s *Service) ShowNetwork(ctx context.Context, host string, m Manager, name string) (*NetworkShow, error) {
	n, err := s.Network(ctx, m, name)
	if err != nil {
		return nil, err
	}
	out := &NetworkShow{Info: n}
	if !n.Vmtool {
		return out, nil
	}
	if cs, _, serr := s.openSite(ctx, "net show", m, host, name); serr == nil {
		out.Orphans, _ = s.removeOrphans(ctx, cs, name)
		cs.close()
	} else if reg := s.Store.registryClient(ctx); reg != nil {
		// The home does not answer (or the network has none to ask): nothing
		// is removed, and the store's marked reservations are listed.
		cctx, cancel := s.credCtx(ctx)
		defer cancel()
		if rs, lerr := reg.OrphanedNetworkReservations(cctx, name); lerr == nil {
			out.Orphans = rs
		}
	}
	return out, nil
}

// removeOrphans removes the orphaned reservations of network (the ones the
// store has marked) on the reservation host cs.resv, which is the home, and
// then their records in the store, saying one line for each. It is the whole
// cleanup: there is no timer, so each command that makes, lists, removes or
// shows a reservation on the network calls it first. Without a store, or a
// store that does not list the network, it does nothing; a home that fails to
// remove one stops it with that reservation and the rest left as they are.
// It returns the orphans not removed and how many it removed on the home.
func (s *Service) removeOrphans(ctx context.Context, cs *cardSite, network string) (left []storeclient.NetworkReservation, removed int) {
	if cs == nil || cs.reg == nil {
		return nil, 0
	}
	cctx, cancel := s.credCtx(ctx)
	orphans, err := cs.reg.OrphanedNetworkReservations(cctx, network)
	cancel()
	if err != nil {
		return nil, 0
	}
	say := s.noter(nil)
	for i, o := range orphans {
		if err := cs.resv.RemoveReservation(network, o.Mac); err != nil && !vmtool.IsNotFound(err) {
			return orphans[i:], removed
		}
		removed++
		fctx, cancel := s.credCtx(ctx)
		err := cs.reg.DeleteNetworkReservation(fctx, network, o.Mac)
		cancel()
		if err != nil && !errors.Is(err, storeclient.ErrNotFound) {
			say(fmt.Sprintf("the orphaned reservation of %s (%s) for %s on network %q was removed on its home but the registry at %s could not forget it: %v", o.Ip, o.Mac, o.Vm, network, s.Store.Host(), err))
			continue
		}
		say(fmt.Sprintf("removed the orphaned reservation of %s (%s) for %s of %s on network %q", o.Ip, o.Mac, o.Vm, o.Host, network))
	}
	return nil, removed
}

// DeleteNetworkResult is what a delete did.
type DeleteNetworkResult struct {
	Network string
	Host    string
	// Registered is false when the network was not in the registry (or there is
	// no store): only the host's libvirt network was removed, as in V1.
	Registered bool
	// Members are the member hosts that unlinked and removed their member network.
	Members []string
	// Left has one line for each member that did not answer: its row is removed
	// and its network cleans itself up when its daemon starts.
	Left []string
}

// hostSilent reports whether err is a host that did not answer (the transport
// failed or timed out), as opposed to one that answered with a refusal.
func hostSilent(err error) bool {
	var ae *Error
	return AsError(err, &ae) && (ae.Kind == KindHostUnreachable || ae.Kind == KindHostTimeout)
}

// deleteStop is a refusal that stops the delete part way: it keeps the kind of
// err, says which hosts were already done, and that the command can be run
// again (every step can be repeated).
func deleteStop(host, name string, done []string, err error) error {
	rerun := fmt.Sprintf("fix that and run vmtool net delete %s %s again", host, name)
	if len(done) > 0 {
		rerun = fmt.Sprintf("done before it stopped: %s; %s", strings.Join(done, ", "), rerun)
	}
	var ae *Error
	if AsError(err, &ae) {
		c := *ae
		c.Err = fmt.Errorf("%v (%s)", ae.Err, rerun)
		return &c
	}
	return internal("net delete", fmt.Errorf("%v (%s)", err, rerun))
}

// DeleteNetworkOnHost deletes the vmtool network name from host, whose Manager
// is m. With no store, or a network the registry does not have, it is
// DeleteNetwork (V1). With a store that does not answer it is refused: a
// registered network must not be removed from its home while its entry stays.
//
// A registered network is deleted only on its home, and only while no VM
// anywhere has a card on it (the home's own libvirt, and every live inventory
// row of the store that reports the network). Then, each step repeatable:
//
//  1. every member other than the home: if its daemon answers it unlinks and
//     undefines its member network (refused there, and the delete stops, when a
//     card is attached), then its row is removed; if it does not answer only
//     its row is removed, and its keeper removes the network when it starts;
//  2. the home unlinks and undefines its network;
//  3. the network's reservation records, the home's row and the entry go.
func (s *Service) DeleteNetworkOnHost(ctx context.Context, host string, m Manager, name string) (*DeleteNetworkResult, error) {
	const op = "net delete"
	if name == "" {
		return nil, invalid(op, errors.New("a network name is required"))
	}
	res := &DeleteNetworkResult{Network: name, Host: host}
	if s.Store == nil {
		if err := s.DeleteNetwork(ctx, m, name); err != nil {
			return nil, err
		}
		return res, nil
	}
	c := s.Store.registryClient(ctx)
	if c == nil {
		return nil, unavailable(op, fmt.Errorf("the store is needed to delete a network that may be registered; try again (the store at %s does not answer this command)", s.Store.Host()))
	}
	rn, err := s.registryNetwork(ctx, op, c, name)
	if err != nil {
		return nil, err
	}
	if rn == nil {
		if err := s.DeleteNetwork(ctx, m, name); err != nil {
			return nil, err
		}
		return res, nil
	}
	res.Registered = true
	if host != rn.Home {
		if memberRow(rn.Members, host) != nil {
			return nil, conflict(op, fmt.Errorf("%s is a member of network %q; run vmtool net leave %s %s, or delete on the home %s", host, name, host, name, rn.Home))
		}
		return nil, conflict(op, fmt.Errorf("network %q is registered with home %s, not %s; delete it on %s", name, rn.Home, host, rn.Home))
	}

	local, err := m.Network(name)
	switch {
	case err != nil && !vmtool.IsNotFound(err):
		return nil, netErr(op, err)
	case err != nil:
		local = nil
	case !local.Vmtool:
		return nil, conflict(op, fmt.Errorf("%w: network %q on %s has bridge %q, not %s plus six lowercase hex digits; vmtool leaves it alone", vmtool.ErrNetworkNotVmtool, name, host, local.Bridge, vmtool.BridgePrefix))
	}

	// No card anywhere: the home's libvirt, and every live row of the store.
	cards := map[string]bool{}
	if local != nil {
		for _, vm := range local.VMs {
			cards[host+"/"+vm] = true
		}
	}
	cctx, cancel := s.credCtx(ctx)
	inv, err := c.Inventory(cctx, "", false)
	cancel()
	if err != nil {
		return nil, unavailable(op, fmt.Errorf("the VMs on network %q could not be read from the store at %s: %v", name, s.Store.Host(), err))
	}
	for _, vm := range inv.Vms {
		if vm.Networks == nil {
			continue
		}
		for _, n := range *vm.Networks {
			if n.Network == name {
				cards[vm.Host+"/"+vm.Name] = true
				break
			}
		}
	}
	if len(cards) > 0 {
		refs := make([]string, 0, len(cards))
		for r := range cards {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		cmds := make([]string, len(refs))
		for i, r := range refs {
			cmds[i] = fmt.Sprintf("vmtool net detach %s %s", r, name)
		}
		return nil, conflict(op, fmt.Errorf("%w: network %q has VMs on it: %s; detach them first (%s)", vmtool.ErrNetworkInUse, name, strings.Join(refs, ", "), strings.Join(cmds, "; ")))
	}

	// The members, one after the other.
	rows := append([]storeclient.NetworkMember(nil), rn.Members...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host < rows[j].Host })
	for _, row := range rows {
		if row.Host == rn.Home {
			continue
		}
		line, err := s.deleteMemberNetwork(ctx, c, rn, row, name)
		if err != nil {
			return nil, deleteStop(host, name, res.Members, err)
		}
		if line != "" {
			res.Left = append(res.Left, line)
		} else {
			res.Members = append(res.Members, row.Host)
		}
	}

	// The home's own network.
	if local != nil {
		if nm, ok := AsNetMember(m); ok {
			if err := nm.RemoveNetworkLink(name); err != nil {
				return nil, deleteStop(host, name, res.Members, netErr(op, fmt.Errorf("unlinking %s from %q: %w", host, name, err)))
			}
		}
		if err := m.DeleteNetwork(name); err != nil && !vmtool.IsNotFound(err) {
			return nil, deleteStop(host, name, res.Members, netErr(op, err))
		}
	}
	// Its records, its row, then the entry.
	cctx, cancel = s.credCtx(ctx)
	defer cancel()
	rsv, err := c.NetworkReservations(cctx, name)
	if err != nil && !errors.Is(err, storeclient.ErrNotFound) {
		return nil, deleteStop(host, name, res.Members, unavailable(op, fmt.Errorf("the registry at %s could not list the reservations of %q: %v", s.Store.Host(), name, err)))
	}
	for _, r := range rsv {
		if err := c.DeleteNetworkReservation(cctx, name, r.Mac); err != nil && !errors.Is(err, storeclient.ErrNotFound) {
			return nil, deleteStop(host, name, res.Members, unavailable(op, fmt.Errorf("the registry at %s could not remove the reservation %s of %q: %v", s.Store.Host(), r.Mac, name, err)))
		}
	}
	if err := c.DeleteNetworkMember(cctx, name, host); err != nil && !errors.Is(err, storeclient.ErrNotFound) {
		return nil, deleteStop(host, name, res.Members, unavailable(op, fmt.Errorf("the registry at %s could not remove the row of %s on %q: %v", s.Store.Host(), host, name, err)))
	}
	switch err := c.DeleteNetwork(cctx, name); {
	case err == nil, errors.Is(err, storeclient.ErrNotFound):
	case errors.Is(err, storeclient.ErrRegistryConflict):
		return nil, deleteStop(host, name, res.Members, conflict(op, fmt.Errorf("the registry at %s still lists members of %q (a host joined meanwhile?); run vmtool net delete again", s.Store.Host(), name)))
	default:
		return nil, deleteStop(host, name, res.Members, unavailable(op, fmt.Errorf("the registry at %s could not remove the entry of %q: %v", s.Store.Host(), name, err)))
	}
	return res, nil
}

// deleteMemberNetwork takes one member out of a network that is being deleted:
// if its daemon answers, it unlinks and undefines its member network, and its
// row is removed either way. It answers a line, not an error, for a member that
// was left to clean itself up (it did not answer) or whose network is not
// vmtool's member form (left alone); "" when it did the work.
func (s *Service) deleteMemberNetwork(ctx context.Context, c *storeclient.Client, rn *storeclient.Network, row storeclient.NetworkMember, name string) (string, error) {
	const op = "net delete"
	line := ""
	asleep := func(err error) {
		line = fmt.Sprintf("%s did not answer (%v): its row is removed and it removes its network %q itself when it wakes", row.Host, err, name)
	}
	mh, release, err := s.openHost(row.Host)
	if err != nil {
		asleep(err)
	} else {
		defer release()
		local, err := mh.Network(name)
		switch {
		case err != nil && hostSilent(err):
			asleep(err)
		case err != nil && !vmtool.IsNotFound(err):
			return "", netErr(op, fmt.Errorf("host %s: %w", row.Host, err))
		case err == nil && !local.IsMemberForm():
			line = fmt.Sprintf("%s has a network %q that is not the member form (%s): left alone; its row is removed", row.Host, name, local.Bridge)
		case err == nil && len(local.VMs) > 0:
			return "", conflict(op, fmt.Errorf("%w: host %s has VMs on network %q: %s; detach them first (vmtool net detach %s/%s %s)", vmtool.ErrNetworkInUse, row.Host, name, strings.Join(local.VMs, ", "), row.Host, local.VMs[0], name))
		case err == nil:
			nm, ok := AsNetMember(mh)
			if !ok {
				return "", netErr(op, errOldHost(row.Host))
			}
			if err := nm.RemoveNetworkLink(name); err != nil {
				return "", netErr(op, fmt.Errorf("unlinking %s from %q: %w", row.Host, name, err))
			}
			if err := mh.DeleteNetwork(name); err != nil {
				// Put the link back so the host is as it was.
				_ = nm.SetNetworkLinks(name, row.LanIp, row.LanDev, peerIPs(rn.Members, row.Host))
				return "", netErr(op, fmt.Errorf("host %s: %w", row.Host, err))
			}
		}
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	if err := c.DeleteNetworkMember(cctx, name, row.Host); err != nil && !errors.Is(err, storeclient.ErrNotFound) {
		return "", unavailable(op, fmt.Errorf("the registry at %s could not remove the row of %s on %q: %v", s.Store.Host(), row.Host, name, err))
	}
	return line, nil
}
