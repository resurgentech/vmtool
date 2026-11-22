package server

import (
	"context"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// A VM's cards on vmtool networks (V2): the daemon's primitives
// createReservation, deleteReservation, attachInterface and detachInterface.
// The workflow that orders them and undoes a failed step is internal/app's
// (AttachNetwork, DetachNetwork, CreateVM, DeleteVM); these only map kinds.

func attachmentsToAPI(in []vmtool.NetAttachment) *[]api.NetworkAttachment {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.NetworkAttachment, len(in))
	for i, a := range in {
		out[i] = api.NetworkAttachment{Network: a.Network, Mac: a.MAC, Ip: a.IP}
	}
	return &out
}

func reservationsToAPI(in []vmtool.Reservation) *[]api.Reservation {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.Reservation, len(in))
	for i, r := range in {
		out[i] = api.Reservation{Network: r.Network, Mac: r.MAC, Ip: r.IP, Name: r.Name}
	}
	return &out
}

func (h *handlers) CreateReservation(ctx context.Context, req api.CreateReservationRequestObject) (api.CreateReservationResponseObject, error) {
	if req.Body == nil {
		return api.CreateReservation400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "a body with a vm is required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.AddReservation(ctx, m, req.Name, req.Body.Vm, derefStr(req.Body.Mac), derefStr(req.Body.Ip))
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.CreateReservation400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.CreateReservation404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.CreateReservation409JSONResponse(apiErr(err)), nil
		}
		return api.CreateReservation500JSONResponse(apiErr(err)), nil
	}
	r := v.(*vmtool.Reservation)
	return api.CreateReservation201JSONResponse(api.Reservation{Network: r.Network, Mac: r.MAC, Ip: r.IP, Name: r.Name}), nil
}

func (h *handlers) DeleteReservation(ctx context.Context, req api.DeleteReservationRequestObject) (api.DeleteReservationResponseObject, error) {
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return nil, h.svc.RemoveReservation(ctx, m, req.Name, req.Mac)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindNotFound:
			return api.DeleteReservation404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindConflict, app.KindInvalid:
			return api.DeleteReservation409JSONResponse(apiErr(err)), nil
		}
		return api.DeleteReservation500JSONResponse(apiErr(err)), nil
	}
	return api.DeleteReservation204Response{}, nil
}

func (h *handlers) AttachInterface(ctx context.Context, req api.AttachInterfaceRequestObject) (api.AttachInterfaceResponseObject, error) {
	if req.Body == nil {
		return api.AttachInterface400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: "a body with a network and a mac is required"}}, nil
	}
	v, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return h.svc.AttachInterface(ctx, m, req.Name, req.Body.Network, req.Body.Mac)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			return api.AttachInterface400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindNotFound:
			return api.AttachInterface404JSONResponse(apiErr(err)), nil
		case app.KindConflict:
			return api.AttachInterface409JSONResponse(apiErr(err)), nil
		}
		return api.AttachInterface500JSONResponse(apiErr(err)), nil
	}
	a := v.(*vmtool.NetAttachment)
	return api.AttachInterface201JSONResponse(api.NetworkAttachment{Network: a.Network, Mac: a.MAC, Ip: a.IP}), nil
}

func (h *handlers) DetachInterface(ctx context.Context, req api.DetachInterfaceRequestObject) (api.DetachInterfaceResponseObject, error) {
	_, err := h.withManager(ctx, req.Host, func(m app.Manager) (any, error) {
		return nil, h.svc.DetachInterface(ctx, m, req.Name, req.Mac)
	})
	if err != nil {
		switch kindOf(err) {
		case app.KindNotFound:
			return api.DetachInterface404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}, nil
		case app.KindConflict, app.KindInvalid:
			return api.DetachInterface409JSONResponse(apiErr(err)), nil
		}
		return api.DetachInterface500JSONResponse(apiErr(err)), nil
	}
	return api.DetachInterface204Response{}, nil
}
