package server

import (
	"context"
	"fmt"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// Named networks on one host (V1): createNetwork, getNetwork, deleteNetwork.
// The workflows are internal/app's; these only map kinds to statuses.

func netToAPI(n *vmtool.NetworkInfo) api.NetworkInfo {
	out := api.NetworkInfo{
		Name:      n.Name,
		Vmtool:    n.Vmtool,
		Active:    n.Active,
		Autostart: n.Autostart,
		Vms:       n.VMs,
	}
	if out.Vms == nil {
		out.Vms = []string{}
	}
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out.Bridge = str(n.Bridge)
	out.Forward = str(n.Forward)
	out.Subnet = str(n.Subnet)
	out.Gateway = str(n.Gateway)
	out.Domain = str(n.Domain)
	out.DhcpStart = str(n.DHCPStart)
	out.DhcpEnd = str(n.DHCPEnd)
	if n.MTU > 0 {
		m := n.MTU
		out.Mtu = &m
	}
	if n.Vmtool {
		v := int(n.VNI)
		out.Vni = &v
	}
	out.Reservations = reservationsToAPI(n.Reservations)
	return out
}

func (h *handlers) CreateNetwork(ctx context.Context, req api.CreateNetworkRequestObject) (api.CreateNetworkResponseObject, error) {
	if req.Body == nil {
		return api.CreateNetwork400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Code: statusCode(400), Error: "a body with a name is required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		// A primitive: the number, when given, is the caller's (the store's
		// registry allocated it); this route never calls the store.
		if req.Body.Member != nil && *req.Body.Member {
			// The member form of a network that spans hosts: the bridge and the
			// MTU only, so a subnet or a domain is a mistake, and the number is
			// what makes it the home's network.
			if req.Body.Vni == nil || req.Body.Subnet != nil || req.Body.Domain != nil {
				return nil, &app.Error{Kind: app.KindInvalid, Op: "net create", Err: fmt.Errorf("a member network takes a vni and nothing else (no subnet, no domain)")}
			}
			if *req.Body.Vni < 1 || *req.Body.Vni > 1<<24-1 {
				return nil, &app.Error{Kind: app.KindInvalid, Op: "net create", Err: fmt.Errorf("vni %d: want 1 to 16777215", *req.Body.Vni)}
			}
			return h.svc.CreateNetworkMember(ctx, m, req.Body.Name, uint32(*req.Body.Vni))
		}
		var vni uint32
		if req.Body.Vni != nil {
			if *req.Body.Vni < 1 || *req.Body.Vni > 1<<24-1 {
				return nil, &app.Error{Kind: app.KindInvalid, Op: "net create", Err: fmt.Errorf("vni %d: want 1 to 16777215", *req.Body.Vni)}
			}
			vni = uint32(*req.Body.Vni)
		}
		return h.svc.CreateNetworkWithVNI(ctx, m, req.Body.Name, derefStr(req.Body.Subnet), derefStr(req.Body.Domain), vni)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.CreateNetwork400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.CreateNetwork404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.CreateNetwork409JSONResponse(apiErr(err)), nil
		}
		return nil, err
	}
	return api.CreateNetwork201JSONResponse(netToAPI(v.(*vmtool.NetworkInfo))), nil
}

func (h *handlers) GetNetwork(ctx context.Context, req api.GetNetworkRequestObject) (api.GetNetworkResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.Network(ctx, m, req.Name)
	})
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			return api.GetNetwork404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		}
		return nil, err
	}
	return api.GetNetwork200JSONResponse(netToAPI(v.(*vmtool.NetworkInfo))), nil
}

func (h *handlers) DeleteNetwork(ctx context.Context, req api.DeleteNetworkRequestObject) (api.DeleteNetworkResponseObject, error) {
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return nil, h.svc.DeleteNetwork(ctx, m, req.Name)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindNotFound:
			return api.DeleteNetwork404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindConflict:
			return api.DeleteNetwork409JSONResponse(apiErr(err)), nil
		}
		return nil, err
	}
	return api.DeleteNetwork204Response{}, nil
}

func (h *handlers) SetNetworkPeers(ctx context.Context, req api.SetNetworkPeersRequestObject) (api.SetNetworkPeersResponseObject, error) {
	if req.Body == nil {
		return api.SetNetworkPeers400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Code: statusCode(400), Error: "a body with lan_ip, lan_dev and peers is required"}}, nil
	}
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return nil, h.svc.SetNetworkPeers(ctx, m, req.Name, req.Body.LanIp, req.Body.LanDev, req.Body.Peers)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.SetNetworkPeers400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.SetNetworkPeers404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.SetNetworkPeers409JSONResponse(apiErr(err)), nil
		}
		return nil, err
	}
	return api.SetNetworkPeers204Response{}, nil
}

func (h *handlers) DeleteNetworkPeers(ctx context.Context, req api.DeleteNetworkPeersRequestObject) (api.DeleteNetworkPeersResponseObject, error) {
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return nil, h.svc.DeleteNetworkPeers(ctx, m, req.Name)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindNotFound:
			return api.DeleteNetworkPeers404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindConflict:
			return api.DeleteNetworkPeers409JSONResponse(apiErr(err)), nil
		}
		return nil, err
	}
	return api.DeleteNetworkPeers204Response{}, nil
}

func (h *handlers) GetUnderlay(ctx context.Context, req api.GetUnderlayRequestObject) (api.GetUnderlayResponseObject, error) {
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.Underlay(ctx, m, req.Params.To)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.GetUnderlay400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.GetUnderlay404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.GetUnderlay409JSONResponse(apiErr(err)), nil
		}
		return nil, err
	}
	u := v.(*vmtool.Underlay)
	return api.GetUnderlay200JSONResponse{Ip: u.IP, Dev: u.Dev}, nil
}
