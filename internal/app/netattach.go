package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// A VM's cards on vmtool networks (tranche 17, V2). A card is an extra
// interface after the VM's own first one; its fixed address and its name are the
// network's DHCP reservation for the card's MAC. The steps of a card are
// ordered, and a failed step undoes the ones before it, so a network never has
// a reservation without a card nor a card without a reservation:
//
//	attach: reservation, then card (the card failing removes the reservation)
//	detach: card, then reservation (the reservation failing puts the card back)
//
// The primitives (Manager.AddReservation and the rest) are the host's; the
// order and the undo are here, once, for `net attach`, `net detach`, create's
// networks and delete.

// NetworkRequest asks for a card on a vmtool network of the VM's host: IP is
// the fixed address wanted, or empty for the next free one of the network's DHCP
// range.
type NetworkRequest struct {
	Network string
	IP      string
}

// ParseNetworkRequests reads `--network NAME[=IP]` values. A network named
// twice, an empty name and an empty address after `=` are refused (invalid).
func ParseNetworkRequests(specs []string) ([]NetworkRequest, error) {
	const op = "create"
	var out []NetworkRequest
	seen := map[string]bool{}
	for _, s := range specs {
		name, ip, hasIP := strings.Cut(s, "=")
		name, ip = strings.TrimSpace(name), strings.TrimSpace(ip)
		switch {
		case name == "":
			return nil, invalid(op, fmt.Errorf("--network %q: a network name is required (NAME or NAME=IP)", s))
		case hasIP && ip == "":
			return nil, invalid(op, fmt.Errorf("--network %q: an address is required after '='", s))
		case seen[name]:
			return nil, invalid(op, fmt.Errorf("--network %q: network %q is named twice; one card per network", s, name))
		}
		seen[name] = true
		out = append(out, NetworkRequest{Network: name, IP: ip})
	}
	return out, nil
}

// placeholderMAC stands in for the card's MAC when a request is checked before
// any MAC exists: the check reads only the network's reservations.
const placeholderMAC = "52:54:00:00:00:00"

// cardSite is where a card's steps happen. A VM's card is attached on the VM's
// own host; its reservation is made in the network's libvirt definition on the
// network's home. On a network made on one host those are the same host. On a
// network that spans hosts the VM's host holds only the member form (no
// addresses), so the reservation is made on the home, which the registry names,
// and the card is attached on the VM's host with the MAC the home picked.
type cardSite struct {
	vm      Manager // the VM's host: the card
	resv    Manager // where the network's reservations are: vm, or the network's home
	release func()  // releases resv when it is another host's
	reg     *storeclient.Client
	vmHost  string
}

func (cs *cardSite) close() {
	if cs != nil && cs.release != nil {
		cs.release()
	}
}

// openSite asks the host about a network and finds where its reservations are:
// a network that does not exist (not found), or is not vmtool's (conflict), is
// refused. The member form of a spanning network needs the store (to find the
// home) and the home awake (unavailable otherwise). The returned NetworkInfo
// is the network as the reservation host has it, with the subnet and the DHCP
// range. reg, when the command is in store mode, records each reservation.
func (s *Service) openSite(ctx context.Context, op string, m Manager, host, network string) (*cardSite, *vmtool.NetworkInfo, error) {
	n, err := m.Network(network)
	if err != nil {
		return nil, nil, netErr(op, err)
	}
	if n == nil || !n.Vmtool {
		return nil, nil, conflict(op, fmt.Errorf("%w: network %q is not one vmtool made (its bridge is not %s plus six lowercase hex digits); vmtool leaves it alone", vmtool.ErrNetworkNotVmtool, network, vmtool.BridgePrefix))
	}
	cs := &cardSite{vm: m, resv: m, vmHost: host, reg: s.Store.registryClient(ctx)}
	if !n.IsMemberForm() {
		return cs, n, nil
	}
	if cs.reg == nil {
		return nil, nil, unavailable(op, fmt.Errorf("network %q on %s is the member form of a network that spans hosts: its reservations are on the home, which is found through the store, and this command has none (local mode)", network, host))
	}
	rn, err := s.registryNetwork(ctx, op, cs.reg, network)
	if err != nil {
		return nil, nil, err
	}
	if rn == nil {
		return nil, nil, conflict(op, fmt.Errorf("network %q on %s is a member network but the registry has no such network; vmtool net leave %s %s removes it", network, host, host, network))
	}
	hm, release, err := s.openHost(rn.Home)
	if err != nil {
		return nil, nil, unavailable(op, fmt.Errorf("the home %s of network %q does not answer: %v; a card on it needs the home for its address and name", rn.Home, network, err))
	}
	hn, err := hm.Network(network)
	if err != nil {
		release()
		return nil, nil, netErr(op, fmt.Errorf("home %s: %w", rn.Home, err))
	}
	cs.resv, cs.release = hm, release
	return cs, hn, nil
}

// checkNetwork asks about a requested network and refuses, before anything is
// created or reserved: what openSite refuses, an address that is taken, a name
// that already has a card there or a full range (conflict), and an address
// outside the subnet or not an address (invalid). The reservation itself is
// chosen by the reservation host later. The caller closes the site.
func (s *Service) checkNetwork(ctx context.Context, op string, m Manager, host, vm string, r NetworkRequest) (*cardSite, error) {
	if r.Network == "" {
		return nil, invalid(op, fmt.Errorf("a network name is required"))
	}
	cs, n, err := s.openSite(ctx, op, m, host, r.Network)
	if err != nil {
		return nil, err
	}
	// Orphaned reservations go first (the home answered, or openSite refused),
	// so their addresses are free for the check below.
	if _, removed := s.removeOrphans(ctx, cs, r.Network); removed > 0 {
		if hn, herr := cs.resv.Network(r.Network); herr == nil && hn != nil {
			n = hn
		}
	}
	if _, err := vmtool.PlanReservation(*n, vm, placeholderMAC, r.IP, nil); err != nil {
		cs.close()
		return nil, netErr(op, err)
	}
	return cs, nil
}

// recordReservation keeps the registry's copy of a reservation (network, MAC,
// address, host, VM). A network the registry does not have (made without a
// store) records nothing; a registry that fails to answer is one line, never an
// error: the reservation itself is made.
func (s *Service) recordReservation(ctx context.Context, cs *cardSite, network, vm, uuid string, res vmtool.Reservation) bool {
	if cs.reg == nil {
		return false
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	in := storeclient.NetworkReservationInput{Ip: res.IP, Host: cs.vmHost, Vm: vm}
	if uuid != "" {
		in.Uuid = &uuid
	}
	_, err := cs.reg.PutNetworkReservation(cctx, network, res.MAC, in)
	if err != nil && !errors.Is(err, storeclient.ErrNotFound) {
		s.noter(nil)(fmt.Sprintf("the reservation of %s (%s) on network %q was made but the registry at %s could not record it: %v", res.IP, res.MAC, network, s.Store.Host(), err))
		return false
	}
	return err == nil
}

// markOrphan tells the registry that the reservation of mac on network is left
// on the home: the caller tried to remove it there and could not. This is a
// positive fact the caller holds; a record the registry does not have, or a
// registry that does not answer, is not an error (the reservation stays, and
// the error the caller returns already names it).
func (s *Service) markOrphan(ctx context.Context, reg *storeclient.Client, network, mac string) {
	if reg == nil {
		return
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	err := reg.MarkNetworkReservationOrphaned(cctx, network, mac)
	if err != nil && !errors.Is(err, storeclient.ErrNotFound) {
		s.noter(nil)(fmt.Sprintf("the reservation of %s on network %q is left on its home but the registry at %s could not be told: %v", mac, network, s.Store.Host(), err))
	}
}

// forgetReservation removes the registry's copy of a reservation, as above.
func (s *Service) forgetReservation(ctx context.Context, cs *cardSite, network, mac string) {
	if cs.reg == nil {
		return
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	err := cs.reg.DeleteNetworkReservation(cctx, network, mac)
	if err != nil && !errors.Is(err, storeclient.ErrNotFound) {
		s.noter(nil)(fmt.Sprintf("the reservation of %s on network %q was removed but the registry at %s could not forget it: %v", mac, network, s.Store.Host(), err))
	}
}

// attachCard reserves an address and a name for a new card of vm on network and
// attaches the card, in that order: the reservation on the reservation host, the
// card on the VM's. If the card fails the reservation is removed; if that fails
// too the error says which reservation is left.
//
// uuid is the VM's libvirt UUID when the caller has it (store mode), recorded
// with the reservation so the store can tell when the VM's row is gone for good.
// A failed undo leaves a reservation with no card: it is recorded with the UUID
// and marked orphaned, for the next command on the network to remove.
func (s *Service) attachCard(ctx context.Context, cs *cardSite, vm, uuid, network, ip string) (*vmtool.NetAttachment, error) {
	res, err := cs.resv.AddReservation(network, vm, "", ip)
	if err != nil {
		return nil, err
	}
	att, err := cs.vm.AttachInterface(vm, network, res.MAC)
	if err != nil {
		if uerr := cs.resv.RemoveReservation(network, res.MAC); uerr != nil && !vmtool.IsNotFound(uerr) {
			err = fmt.Errorf("%w; and the reservation of %s for %s on network %q could not be removed: %v", err, res.IP, res.MAC, network, uerr)
			if s.recordReservation(ctx, cs, network, vm, uuid, *res) {
				s.markOrphan(ctx, cs.reg, network, res.MAC)
			}
		}
		return nil, err
	}
	if att == nil {
		att = &vmtool.NetAttachment{Network: network, MAC: res.MAC}
	}
	att.IP = res.IP // the reservation just made is the card's address
	s.recordReservation(ctx, cs, network, vm, uuid, *res)
	return att, nil
}

// detachCard removes vm's card with mac from network and then its reservation.
// A reservation that is already gone is fine. If removing the reservation fails
// the card is put back (so the reservation does not outlive its card's absence
// unseen); if that fails too the error says so.
func (s *Service) detachCard(ctx context.Context, cs *cardSite, vm, network, mac string) error {
	if err := cs.vm.DetachInterface(vm, mac); err != nil {
		return err
	}
	if err := cs.resv.RemoveReservation(network, mac); err != nil && !vmtool.IsNotFound(err) {
		if _, rerr := cs.vm.AttachInterface(vm, network, mac); rerr != nil {
			// The card is gone and its reservation could not be removed: the
			// reservation is left on the home with no card.
			s.markOrphan(ctx, cs.reg, network, mac)
			return fmt.Errorf("%w; and the card %s could not be put back: %v", err, mac, rerr)
		}
		return err
	}
	s.forgetReservation(ctx, cs, network, mac)
	return nil
}

// AttachNetwork gives VM ref an extra card on a vmtool network of its host:
// vmtool picks the MAC, reserves ip (or the next free address of the network's
// DHCP range) under the VM's name, then attaches the card, live and in the
// config when the VM is running, in the config only when it is shut off. A
// failed step undoes the one before it.
func (s *Service) AttachNetwork(ctx context.Context, m Manager, ref VMRef, network, ip string) (*vmtool.NetAttachment, error) {
	const op = "net attach"
	if err := validRef(op, ref); err != nil {
		return nil, err
	}
	if network == "" {
		return nil, invalid(op, fmt.Errorf("a network name is required"))
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return nil, wrap(op, err)
	}
	cs, err := s.checkNetwork(ctx, op, m, ref.Host, ref.Name, NetworkRequest{Network: network, IP: ip})
	if err != nil {
		return nil, err
	}
	defer cs.close()
	att, err := s.attachCard(ctx, cs, ref.Name, info.UUID, network, ip)
	if err != nil {
		return nil, netErr(op, err)
	}
	return att, nil
}

// DetachNetwork removes VM ref's card on a vmtool network, named by the network
// or by the card's MAC (needed when the VM has more than one card on the
// network), then the card's reservation. A failed reservation removal puts the
// card back.
func (s *Service) DetachNetwork(ctx context.Context, m Manager, ref VMRef, network string) error {
	const op = "net detach"
	if err := validRef(op, ref); err != nil {
		return err
	}
	if network == "" {
		return invalid(op, fmt.Errorf("a network name or a MAC is required"))
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return wrap(op, err)
	}
	want := strings.ToLower(strings.TrimSpace(network))
	var hit []vmtool.NetAttachment
	for _, a := range info.Networks {
		if a.Network == network || strings.ToLower(a.MAC) == want {
			hit = append(hit, a)
		}
	}
	switch len(hit) {
	case 0:
		return &Error{Kind: KindNotFound, Op: op, Err: fmt.Errorf("%s has no card on network %q (cards on vmtool networks: %s): %w", ref, network, cardList(info.Networks), vmtool.ErrNotFound)}
	case 1:
	default:
		var macs []string
		for _, a := range hit {
			macs = append(macs, a.MAC)
		}
		return invalid(op, fmt.Errorf("%s has %d cards on network %q (%s); name one by its MAC", ref, len(hit), network, strings.Join(macs, ", ")))
	}
	cs, _, err := s.openSite(ctx, op, m, ref.Host, hit[0].Network)
	if err != nil {
		return err
	}
	defer cs.close()
	s.removeOrphans(ctx, cs, hit[0].Network)
	return netErr(op, s.detachCard(ctx, cs, ref.Name, hit[0].Network, hit[0].MAC))
}

func cardList(cards []vmtool.NetAttachment) string {
	if len(cards) == 0 {
		return "none"
	}
	var out []string
	for _, c := range cards {
		out = append(out, c.Network+" "+c.MAC)
	}
	return strings.Join(out, ", ")
}

// removeReservations removes the reservation of each card from its network,
// after the VM is gone: on the network's home for a network that spans hosts.
// A reservation or a network that is already gone is fine; any other failure
// (a home that does not answer, say) is collected, naming the reservation left.
func (s *Service) removeReservations(ctx context.Context, m Manager, host string, cards []vmtool.NetAttachment) error {
	var errs []error
	sites := map[string]*cardSite{}
	defer func() {
		for _, cs := range sites {
			cs.close()
		}
	}()
	for _, c := range cards {
		cs, ok := sites[c.Network]
		if !ok {
			var err error
			cs, _, err = s.openSite(ctx, "delete", m, host, c.Network)
			if err != nil {
				if !vmtool.IsNotFound(err) {
					// %v: the site's error is an *Error, and the caller's wrap would
					// otherwise swap this line for it.
					errs = append(errs, fmt.Errorf("the reservation of %s (%s) on network %q could not be removed: %v", c.IP, c.MAC, c.Network, err))
					// The VM is gone and its reservation is left on the home: a
					// positive fact; the next command on the network removes it.
					s.markOrphan(ctx, s.Store.registryClient(ctx), c.Network, c.MAC)
				}
				continue
			}
			sites[c.Network] = cs
			s.removeOrphans(ctx, cs, c.Network)
		}
		if err := cs.resv.RemoveReservation(c.Network, c.MAC); err != nil && !vmtool.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("the reservation of %s (%s) on network %q could not be removed: %w", c.IP, c.MAC, c.Network, err))
			s.markOrphan(ctx, cs.reg, c.Network, c.MAC)
			continue
		}
		s.forgetReservation(ctx, cs, c.Network, c.MAC)
	}
	return errors.Join(errs...)
}

// The four primitives, for the daemon's routes: each asks the host and sorts its
// refusal into the kind REST maps (see netErr). The workflows above compose them;
// a client that calls them directly composes the steps itself.

// AddReservation reserves an address and a name for mac on a vmtool network.
func (s *Service) AddReservation(_ context.Context, m Manager, network, vm, mac, ip string) (*vmtool.Reservation, error) {
	const op = "reserve"
	if network == "" || vm == "" {
		return nil, invalid(op, fmt.Errorf("a network and a vm name are required"))
	}
	r, err := m.AddReservation(network, vm, mac, ip)
	if err != nil {
		return nil, netErr(op, err)
	}
	return r, nil
}

// RemoveReservation removes the reservation of mac from a vmtool network.
func (s *Service) RemoveReservation(_ context.Context, m Manager, network, mac string) error {
	return netErr("unreserve", m.RemoveReservation(network, mac))
}

// AttachInterface adds the card with mac on a vmtool network to a VM.
func (s *Service) AttachInterface(_ context.Context, m Manager, vm, network, mac string) (*vmtool.NetAttachment, error) {
	const op = "attach interface"
	if network == "" || mac == "" {
		return nil, invalid(op, fmt.Errorf("a network and a mac are required"))
	}
	a, err := m.AttachInterface(vm, network, mac)
	if err != nil {
		return nil, netErr(op, err)
	}
	return a, nil
}

// DetachInterface removes the card with mac from a VM.
func (s *Service) DetachInterface(_ context.Context, m Manager, vm, mac string) error {
	return netErr("detach interface", m.DetachInterface(vm, mac))
}
