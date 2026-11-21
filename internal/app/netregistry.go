package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
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
// host that cannot is sent the V1 request and its network is not registered.
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
//  3. if step 2 fails, the registry entry is deleted again. If that delete
//     fails too, the error says the entry is left, by name.
//
// The registry goes first because the host's definition needs what the
// registry allocates; a registry entry with no libvirt network behind it
// (this process dying between the two steps) is the failure that remains, and
// the entry can be deleted through the store's DELETE /v1/networks/{name}.
// A host whose Manager is not a VNICreator is made as in local mode and the
// user is told once that the network is not registered.
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
		s.Store.Notify(fmt.Sprintf("network %q is not registered in the store: %s cannot take a bridge number from it yet; it was numbered on the host", name, host))
		return s.CreateNetwork(ctx, m, name, subnet, domain)
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
