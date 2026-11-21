package server

import (
	"context"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
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
		return api.CreateNetwork400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "a body with a name is required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.CreateNetwork(ctx, m, req.Body.Name, derefStr(req.Body.Subnet), derefStr(req.Body.Domain))
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
		return api.CreateNetwork500JSONResponse(apiErr(err)), nil
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
		return api.GetNetwork500JSONResponse(apiErr(err)), nil
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
		return api.DeleteNetwork500JSONResponse(apiErr(err)), nil
	}
	return api.DeleteNetwork204Response{}, nil
}
