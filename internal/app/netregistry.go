package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// The network registry in store mode (tranche 17, V3). The store keeps the
// networks that span hosts and allocates their VNIs (from 10000) and subnets
// (the next free /24 of 10.77.0.0/16) so no two hosts collide. A command with
// no store, or one whose store does not answer, is in local mode and
// registers nothing: the host numbers its own bridge (1..9999) and picks its
// own subnet, as V1 does.

// VNICreator is a host that can define a vmtool network on a bridge number it
// is given (vmt- plus the VNI as six hex digits) and a subnet it is given.
// Service.CreateNetworkOnHost needs it to take the number from the registry; a
// host that cannot is refused before anything is registered.
type VNICreator interface {
	CreateNetworkVNI(name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error)
}

// registryClient is the store's client when this command is in store mode with
// a user token: the registry is managed by user tokens only. nil in local mode
// (no server, an unreachable store, an unusable token) and for a session whose
// token is a host's (the daemon's own), which has no write to the registry.
// The mode is decided here if it is not yet.
func (s *StoreSession) registryClient(ctx context.Context) *storeclient.Client {
	if s.Mode(ctx) != ModeStore || strings.HasPrefix(s.Caller(), "host/") {
		return nil
	}
	return s.client
}

// CreateNetworkOnHost makes a vmtool network on host, whose Manager is m.
// In local mode it is CreateNetwork: nothing is registered. In store mode the
// order is:
//
//  1. the registry creates the network (home = host) and answers its VNI and
//     subnet, so no other host can be handed them;
//  2. the host defines the libvirt network on bridge vmt-<VNI hex> with that
//     subnet (VNICreator);
//  3. if step 2 fails, or the host answers with another bridge (a daemon that
//     predates the vni field ignores it and numbers the bridge itself, in
//     which case the network it made is deleted again), the registry entry is
//     deleted. If a delete fails, the error says what is left, by name.
//
// The registry goes first because the host's definition needs what the
// registry allocates; a registry entry with no libvirt network behind it
// (this process dying between the two steps) is the failure that remains, and
// the entry can be deleted through the store's DELETE /v1/networks/{name}.
// A host whose Manager is not a VNICreator is refused before anything is
// registered: it cannot take the registry's number.
func (s *Service) CreateNetworkOnHost(ctx context.Context, host string, m Manager, name, subnet, domain string) (*vmtool.NetworkInfo, error) {
	const op = "net create"
	if !vmtool.ValidNetworkName(name) {
		return nil, invalid(op, fmt.Errorf("network name %q: letters, digits, '.', '_', '-', up to 63 characters, and not \"default\"", name))
	}
	c := s.Store.registryClient(ctx)
	if c == nil {
		return s.CreateNetwork(ctx, m, name, subnet, domain)
	}
	vc, ok := m.(VNICreator)
	if !ok {
		return nil, netErr(op, errOldHost(host))
	}
	in := storeclient.NetworkInput{Name: name, Home: host}
	if subnet != "" {
		in.Subnet = &subnet
	}
	if domain != "" {
		in.Domain = &domain
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	reg, err := c.CreateNetwork(cctx, in)
	switch {
	case errors.Is(err, storeclient.ErrRegistryConflict):
		return nil, conflict(op, fmt.Errorf("the registry at %s refused network %q: the name, the VNI or the subnet is taken, or none is free", s.Store.Host(), name))
	case err != nil:
		return nil, unavailable(op, fmt.Errorf("the registry at %s could not register network %q: %v", s.Store.Host(), name, err))
	}
	n, err := vc.CreateNetworkVNI(name, reg.Subnet, reg.Domain, uint32(reg.Vni))
	if err == nil && n.Bridge != vmtool.BridgeName(uint32(reg.Vni)) {
		err = errOldHost(host)
		// The host made a network on a number of its own: take it off again.
		if derr := m.DeleteNetwork(name); derr != nil {
			err = fmt.Errorf("%w (and network %q, bridge %s, is still on %s: %v; delete it with vmtool net delete %s %s)",
				err, name, n.Bridge, host, derr, host, name)
		}
	}
	if err != nil {
		// Undo with a context of its own: the command's may be what failed.
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), CredTimeout)
		defer ucancel()
		if derr := c.DeleteNetwork(uctx, name); derr != nil && !errors.Is(derr, storeclient.ErrNotFound) {
			return nil, netErr(op, fmt.Errorf("%w (and the registry entry for %q, VNI %d, is still in the store at %s: %v; delete it with DELETE /v1/networks/%s)",
				err, name, reg.Vni, s.Store.Host(), derr, name))
		}
		return nil, netErr(op, err)
	}
	return n, nil
}

func errOldHost(host string) error {
	return fmt.Errorf("host %s runs a vmtool that cannot take a network number; redeploy it (just deploy %s)", host, host)
}

// CreateNetworkWithVNI is the daemon's createNetwork: a primitive that never
// calls the store. With a vni (the registry's, given by the caller) the host
// defines its bridge as vmt-<vni hex>; without one it numbers the bridge
// itself, as CreateNetwork does. A Manager that cannot take a number is
// refused when one is given.
func (s *Service) CreateNetworkWithVNI(ctx context.Context, m Manager, name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	const op = "net create"
	if vni == 0 {
		return s.CreateNetwork(ctx, m, name, subnet, domain)
	}
	if !vmtool.ValidNetworkName(name) {
		return nil, invalid(op, fmt.Errorf("network name %q: letters, digits, '.', '_', '-', up to 63 characters, and not \"default\"", name))
	}
	vc, ok := m.(VNICreator)
	if !ok {
		return nil, invalid(op, errors.New("this host cannot take a network number"))
	}
	n, err := vc.CreateNetworkVNI(name, subnet, domain, vni)
	if err != nil {
		return nil, netErr(op, err)
	}
	return n, nil
}

// ForwardCreateNetworkVNI is what a Manager wrapper's CreateNetworkVNI calls
// on the Manager it wraps: the number goes through when that Manager takes
// one; with no number it is the plain CreateNetwork.
func ForwardCreateNetworkVNI(m Manager, name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	if vc, ok := m.(VNICreator); ok {
		return vc.CreateNetworkVNI(name, subnet, domain, vni)
	}
	if vni == 0 {
		return m.CreateNetwork(name, subnet, domain)
	}
	return nil, errors.New("this host cannot take a network number")
}
