package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeapi"
)

// The network registry (tranche 17, V3): the networks that span hosts, their
// members, and the allocation of their VNIs and subnets, so two hosts never
// hand out the same ones. User tokens manage it; a host token has exactly one
// read, GET /v1/hosts/{host}/networks, for its own host. The store only
// records: it never reaches a host and keeps nothing about whether one is up.

const (
	maxNetworkName = 63
	maxDomainLen   = 253
)

var (
	// Mirrors pkg/vmtool's dnsNameRE and the interface-name rule of the
	// kernel (at most 15 bytes, no '/' or space); pkg/vmtool cannot be imported
	// here (it pulls in libvirt).
	dnsNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
	lanDevRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,14}$`)
)

// validNetworkName is the VM name rule, at most 63 characters, and not
// "default" (libvirt's own network).
func validNetworkName(n string) bool {
	return n != "default" && len(n) <= maxNetworkName && validMachineName(n)
}

func validDomain(d string) bool { return len(d) <= maxDomainLen && dnsNameRE.MatchString(d) }

// validSubnet reports whether s is an IPv4 /24 written as its network address.
func validSubnet(s string) bool {
	p, err := netip.ParsePrefix(s)
	return err == nil && p.Addr().Is4() && p.Bits() == 24 && p.Masked() == p && p.String() == s
}

// lanAddr returns the canonical form of a member's LAN address: a plain IPv4
// unicast address.
func lanAddr(s string) (string, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.String() != s {
		return "", false
	}
	return a.String(), true
}

func bridgeOf(vni uint32) string { return fmt.Sprintf("vmt-%06x", vni) }

func netToAPI(n db.Network) storeapi.Network {
	out := storeapi.Network{
		Name: n.Name, Vni: int(n.VNI), Bridge: bridgeOf(n.VNI), Subnet: n.Subnet, Domain: n.Domain,
		Home: n.Home, Created: n.Created, Members: make([]storeapi.NetworkMember, 0, len(n.Members)),
	}
	for _, m := range n.Members {
		out.Members = append(out.Members, memberToAPI(m))
	}
	return out
}

func memberToAPI(m db.NetworkMember) storeapi.NetworkMember {
	return storeapi.NetworkMember{Host: m.Host, LanIp: m.LANIP, LanDev: m.LANDev, Joined: m.Joined}
}

// netConflict is the 409 text of a registry refusal; the db's errors are fixed
// strings with no caller value in them.
func netConflict(err error) (string, bool) {
	switch {
	case errors.Is(err, db.ErrNetworkExists), errors.Is(err, db.ErrVNITaken), errors.Is(err, db.ErrSubnetTaken),
		errors.Is(err, db.ErrNoFreeVNI), errors.Is(err, db.ErrNoFreeSubnet), errors.Is(err, db.ErrNetworkHasMembers):
		return err.Error(), true
	}
	return "", false
}

func (h *handlers) ListNetworks(ctx context.Context, _ storeapi.ListNetworksRequestObject) (storeapi.ListNetworksResponseObject, error) {
	if !h.img.dbReady() {
		return storeapi.ListNetworks500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	nets, err := h.img.db.ListNetworks(ctx)
	if err != nil {
		h.img.fail("list networks", err)
		return storeapi.ListNetworks500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	out := storeapi.NetworkList{Networks: make([]storeapi.Network, 0, len(nets))}
	for _, n := range nets {
		out.Networks = append(out.Networks, netToAPI(n))
	}
	return storeapi.ListNetworks200JSONResponse(out), nil
}

func (h *handlers) CreateNetwork(ctx context.Context, req storeapi.CreateNetworkRequestObject) (storeapi.CreateNetworkResponseObject, error) {
	bad := func(f string) (storeapi.CreateNetworkResponseObject, error) {
		return storeapi.CreateNetwork400JSONResponse{BadRequestJSONResponse: fieldErr(f)}, nil
	}
	b := req.Body
	if b == nil {
		return bad("body")
	}
	domain := deref(b.Domain)
	if domain == "" {
		domain = b.Name + ".test"
	}
	switch {
	case !validNetworkName(b.Name):
		return bad("name")
	case !validHostName(b.Home):
		return bad("home")
	case !validDomain(domain):
		return bad("domain")
	case b.Subnet != nil && !validSubnet(*b.Subnet):
		return bad("subnet")
	case b.Vni != nil && (*b.Vni < 1 || *b.Vni > db.MaxVNI):
		return bad("vni")
	}
	if !h.img.dbReady() {
		return storeapi.CreateNetwork500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	in := db.NetworkNew{Name: b.Name, Home: b.Home, Domain: domain, Subnet: deref(b.Subnet)}
	if b.Vni != nil {
		in.VNI = uint32(*b.Vni)
	}
	n, err := h.img.db.CreateNetwork(ctx, in)
	if msg, ok := netConflict(err); ok {
		return storeapi.CreateNetwork409JSONResponse{Error: msg}, nil
	}
	if err != nil {
		h.img.fail("create network", err)
		return storeapi.CreateNetwork500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.CreateNetwork201JSONResponse(netToAPI(n)), nil
}

func (h *handlers) GetNetwork(ctx context.Context, req storeapi.GetNetworkRequestObject) (storeapi.GetNetworkResponseObject, error) {
	if !validMachineName(req.Name) {
		return storeapi.GetNetwork400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.GetNetwork500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	n, err := h.img.db.GetNetwork(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.GetNetwork404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("get network", err)
		return storeapi.GetNetwork500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.GetNetwork200JSONResponse(netToAPI(n)), nil
}

func (h *handlers) DeleteNetwork(ctx context.Context, req storeapi.DeleteNetworkRequestObject) (storeapi.DeleteNetworkResponseObject, error) {
	if !validMachineName(req.Name) {
		return storeapi.DeleteNetwork400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.DeleteNetwork500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	err := h.img.db.DeleteNetwork(ctx, req.Name)
	if msg, ok := netConflict(err); ok {
		return storeapi.DeleteNetwork409JSONResponse{Error: msg}, nil
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DeleteNetwork404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("delete network", err)
		return storeapi.DeleteNetwork500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.DeleteNetwork204Response{}, nil
}

func (h *handlers) PutNetworkMember(ctx context.Context, req storeapi.PutNetworkMemberRequestObject) (storeapi.PutNetworkMemberResponseObject, error) {
	bad := func(f string) (storeapi.PutNetworkMemberResponseObject, error) {
		return storeapi.PutNetworkMember400JSONResponse{BadRequestJSONResponse: fieldErr(f)}, nil
	}
	b := req.Body
	switch {
	case !validMachineName(req.Name):
		return bad("name")
	case !validHostName(req.Host):
		return bad("host")
	case b == nil:
		return bad("body")
	}
	ip, ok := lanAddr(b.LanIp)
	switch {
	case !ok:
		return bad("lan_ip")
	case !lanDevRE.MatchString(b.LanDev):
		return bad("lan_dev")
	case !h.img.dbReady():
		return storeapi.PutNetworkMember500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	m, err := h.img.db.PutNetworkMember(ctx, req.Name, db.NetworkMember{Host: req.Host, LANIP: ip, LANDev: b.LanDev})
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.PutNetworkMember404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("put network member", err)
		return storeapi.PutNetworkMember500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.PutNetworkMember200JSONResponse(memberToAPI(m)), nil
}

func (h *handlers) DeleteNetworkMember(ctx context.Context, req storeapi.DeleteNetworkMemberRequestObject) (storeapi.DeleteNetworkMemberResponseObject, error) {
	switch {
	case !validMachineName(req.Name):
		return storeapi.DeleteNetworkMember400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	case !validHostName(req.Host):
		return storeapi.DeleteNetworkMember400JSONResponse{BadRequestJSONResponse: fieldErr("host")}, nil
	case !h.img.dbReady():
		return storeapi.DeleteNetworkMember500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	err := h.img.db.DeleteNetworkMember(ctx, req.Name, req.Host)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DeleteNetworkMember404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("delete network member", err)
		return storeapi.DeleteNetworkMember500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.DeleteNetworkMember204Response{}, nil
}

// GetHostNetworks is the one read a host token has beyond its report. A host
// token is answered only for its own host (another host's name is 403); a user
// token may name any host. Only the networks the host is a member of or the
// home of come back, with the other members' LAN addresses and nothing else.
func (h *handlers) GetHostNetworks(ctx context.Context, req storeapi.GetHostNetworksRequestObject) (storeapi.GetHostNetworksResponseObject, error) {
	if c := callerFrom(ctx); strings.HasPrefix(c, hostCallerPrefix) && c != hostCallerPrefix+req.Host {
		return storeapi.GetHostNetworks403JSONResponse{ForbiddenJSONResponse: forbidden}, nil
	}
	if !validHostName(req.Host) {
		return storeapi.GetHostNetworks400JSONResponse{BadRequestJSONResponse: fieldErr("host")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.GetHostNetworks500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	nets, err := h.img.db.HostNetworks(ctx, req.Host)
	if err != nil {
		h.img.fail("host networks", err)
		return storeapi.GetHostNetworks500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	out := storeapi.HostNetworks{Host: req.Host, Networks: make([]storeapi.HostNetwork, 0, len(nets))}
	for _, n := range nets {
		hn := storeapi.HostNetwork{
			Name: n.Name, Vni: int(n.VNI), Bridge: bridgeOf(n.VNI), Subnet: n.Subnet, Domain: n.Domain,
			Home: n.Home, Peers: []storeapi.NetworkPeer{},
		}
		for _, m := range n.Members {
			if m.Host == req.Host {
				hn.Member = &storeapi.HostNetworkSelf{LanIp: m.LANIP, LanDev: m.LANDev}
				continue
			}
			hn.Peers = append(hn.Peers, storeapi.NetworkPeer{Host: m.Host, LanIp: m.LANIP})
		}
		out.Networks = append(out.Networks, hn)
	}
	return storeapi.GetHostNetworks200JSONResponse(out), nil
}
