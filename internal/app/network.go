package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Named networks on one host (tranche 17, V1). They need no store: a network
// is a libvirt network on the host, found by its bridge name (vmt- plus six
// lowercase hex digits), and vmtool keeps nothing about it anywhere else.

// netErr sorts a host's refusal into the kind REST maps: a bad name, subnet or
// domain, and an overlap, are the caller's (400); a name taken, no room, a
// network that is not vmtool's, and one with VMs on it, are conflicts (409); a
// missing network is 404.
func netErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var ae *Error
	if AsError(err, &ae) {
		if ae.Op == "" {
			ae.Op = op
		}
		return ae
	}
	switch {
	case errors.Is(err, vmtool.ErrInvalidNetwork), errors.Is(err, vmtool.ErrSubnetOverlap):
		return invalid(op, err)
	case errors.Is(err, vmtool.ErrNetworkExists), errors.Is(err, vmtool.ErrNoFreeSubnet),
		errors.Is(err, vmtool.ErrNetworkNotVmtool), errors.Is(err, vmtool.ErrNetworkInUse),
		errors.Is(err, vmtool.ErrAddressTaken):
		return conflict(op, err)
	}
	return wrap(op, err)
}

// CreateNetwork makes a vmtool network on the host m serves: NAT, bridge
// vmt-<6 hex>, DHCP, autostart, started. An empty subnet is the first free /24
// of 10.77.0.0/16; an empty domain is <name>.test.
func (s *Service) CreateNetwork(_ context.Context, m Manager, name, subnet, domain string) (*vmtool.NetworkInfo, error) {
	const op = "net create"
	if !vmtool.ValidNetworkName(name) {
		return nil, invalid(op, fmt.Errorf("network name %q: letters, digits, '.', '_', '-', up to 63 characters, and not \"default\"", name))
	}
	n, err := m.CreateNetwork(name, subnet, domain)
	if err != nil {
		return nil, netErr(op, err)
	}
	return n, nil
}

// NetworkMark is one entry of the networks listing: a name, and whether the
// network is vmtool's.
type NetworkMark struct {
	Name   string
	Vmtool bool
}

// ListNetworkMarks lists the host's active libvirt networks and marks the
// ones vmtool made. listNetworks answers names only, so each name is asked of
// the host; a name it cannot describe is listed unmarked.
func (s *Service) ListNetworkMarks(ctx context.Context, m Manager) ([]NetworkMark, error) {
	names, err := s.ListNetworks(ctx, m)
	if err != nil {
		return nil, err
	}
	out := make([]NetworkMark, 0, len(names))
	for _, n := range names {
		mk := NetworkMark{Name: n}
		if info, err := m.Network(n); err == nil && info != nil {
			mk.Vmtool = info.Vmtool
		}
		out = append(out, mk)
	}
	return out, nil
}

// Network describes one libvirt network on the host, vmtool's or not.
func (s *Service) Network(_ context.Context, m Manager, name string) (*vmtool.NetworkInfo, error) {
	const op = "net show"
	if name == "" {
		return nil, invalid(op, fmt.Errorf("a network name is required"))
	}
	n, err := m.Network(name)
	if err != nil {
		return nil, netErr(op, err)
	}
	return n, nil
}

// DeleteNetwork destroys and undefines a vmtool network. The host refuses a
// network that is not vmtool's and one with VMs attached (409).
func (s *Service) DeleteNetwork(_ context.Context, m Manager, name string) error {
	const op = "net delete"
	if name == "" {
		return invalid(op, fmt.Errorf("a network name is required"))
	}
	return netErr(op, m.DeleteNetwork(name))
}
