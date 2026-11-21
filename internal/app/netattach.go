package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// checkNetwork asks the host about a requested network and refuses, before
// anything is created or reserved: a network that does not exist (not found), one
// that is not vmtool's, an address that is taken, a name that already has a card
// there or a full range (conflict), and an address outside the subnet or not an
// address (invalid). The reservation itself is chosen by the host later.
func checkNetwork(op string, m Manager, vm string, r NetworkRequest) error {
	if r.Network == "" {
		return invalid(op, fmt.Errorf("a network name is required"))
	}
	n, err := m.Network(r.Network)
	if err != nil {
		return netErr(op, err)
	}
	if n == nil || !n.Vmtool {
		return conflict(op, fmt.Errorf("%w: network %q is not one vmtool made (its bridge is not %s plus six lowercase hex digits); vmtool leaves it alone", vmtool.ErrNetworkNotVmtool, r.Network, vmtool.BridgePrefix))
	}
	if _, err := vmtool.PlanReservation(*n, vm, placeholderMAC, r.IP, nil); err != nil {
		return netErr(op, err)
	}
	return nil
}

// attachCard reserves an address and a name for a new card of vm on network and
// attaches the card, in that order. If the card fails the reservation is
// removed; if that fails too the error says which reservation is left.
func attachCard(m Manager, vm, network, ip string) (*vmtool.NetAttachment, error) {
	res, err := m.AddReservation(network, vm, "", ip)
	if err != nil {
		return nil, err
	}
	att, err := m.AttachInterface(vm, network, res.MAC)
	if err != nil {
		if uerr := m.RemoveReservation(network, res.MAC); uerr != nil && !vmtool.IsNotFound(uerr) {
			err = fmt.Errorf("%w; and the reservation of %s for %s on network %q could not be removed: %v", err, res.IP, res.MAC, network, uerr)
		}
		return nil, err
	}
	if att == nil {
		att = &vmtool.NetAttachment{Network: network, MAC: res.MAC}
	}
	att.IP = res.IP // the reservation just made is the card's address
	return att, nil
}

// detachCard removes vm's card with mac from network and then its reservation.
// A reservation that is already gone is fine. If removing the reservation fails
// the card is put back (so the reservation does not outlive its card's absence
// unseen); if that fails too the error says so.
func detachCard(m Manager, vm, network, mac string) error {
	if err := m.DetachInterface(vm, mac); err != nil {
		return err
	}
	if err := m.RemoveReservation(network, mac); err != nil && !vmtool.IsNotFound(err) {
		if _, rerr := m.AttachInterface(vm, network, mac); rerr != nil {
			return fmt.Errorf("%w; and the card %s could not be put back: %v", err, mac, rerr)
		}
		return err
	}
	return nil
}

// AttachNetwork gives VM ref an extra card on a vmtool network of its host:
// vmtool picks the MAC, reserves ip (or the next free address of the network's
// DHCP range) under the VM's name, then attaches the card, live and in the
// config when the VM is running, in the config only when it is shut off. A
// failed step undoes the one before it.
func (s *Service) AttachNetwork(_ context.Context, m Manager, ref VMRef, network, ip string) (*vmtool.NetAttachment, error) {
	const op = "net attach"
	if err := validRef(op, ref); err != nil {
		return nil, err
	}
	if network == "" {
		return nil, invalid(op, fmt.Errorf("a network name is required"))
	}
	if _, err := m.Info(ref.Name); err != nil {
		return nil, wrap(op, err)
	}
	if err := checkNetwork(op, m, ref.Name, NetworkRequest{Network: network, IP: ip}); err != nil {
		return nil, err
	}
	att, err := attachCard(m, ref.Name, network, ip)
	if err != nil {
		return nil, netErr(op, err)
	}
	return att, nil
}

// DetachNetwork removes VM ref's card on a vmtool network, named by the network
// or by the card's MAC (needed when the VM has more than one card on the
// network), then the card's reservation. A failed reservation removal puts the
// card back.
func (s *Service) DetachNetwork(_ context.Context, m Manager, ref VMRef, network string) error {
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
	return netErr(op, detachCard(m, ref.Name, hit[0].Network, hit[0].MAC))
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
// after the VM is gone. A reservation or a network that is already gone is
// fine; any other failure is collected, naming the reservation left.
func removeReservations(m Manager, cards []vmtool.NetAttachment) error {
	var errs []error
	for _, c := range cards {
		if err := m.RemoveReservation(c.Network, c.MAC); err != nil && !vmtool.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("the reservation of %s (%s) on network %q could not be removed: %w", c.IP, c.MAC, c.Network, err))
		}
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
