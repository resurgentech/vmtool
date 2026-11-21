package store

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/jaredeh/vmtool/internal/store/db"
	"github.com/jaredeh/vmtool/internal/storeapi"
)

// The reservations of networks that span hosts (tranche 17, V5): the store
// keeps a copy (network, MAC, address, host, VM) of what the home's daemon
// made, so a client can answer without reaching the home. The store makes no
// reservation and reaches no host. User tokens only; no host token has any of
// these routes.

// normMAC returns the lower-case, colon-separated form of a 6-byte MAC.
func normMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 || len(s) != 17 {
		return "", false
	}
	return strings.ToLower(hw.String()), true
}

func resToAPI(r db.NetworkReservation) storeapi.NetworkReservation {
	out := storeapi.NetworkReservation{Network: r.Network, Mac: r.MAC, Ip: r.IP, Host: r.Host, Vm: r.VM, Created: r.Created}
	if r.UUID != "" {
		u := r.UUID
		out.Uuid = &u
	}
	if !r.OrphanedAt.IsZero() {
		t := r.OrphanedAt
		out.OrphanedAt = &t
	}
	return out
}

// validUUID reports whether s is a libvirt UUID (canonical lowercase form).
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f'):
			return false
		}
	}
	return true
}

func (h *handlers) ListOrphanedNetworkReservations(ctx context.Context, req storeapi.ListOrphanedNetworkReservationsRequestObject) (storeapi.ListOrphanedNetworkReservationsResponseObject, error) {
	if !validMachineName(req.Name) {
		return storeapi.ListOrphanedNetworkReservations400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.ListOrphanedNetworkReservations500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	rs, err := h.img.db.OrphanedReservations(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.ListOrphanedNetworkReservations404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("list orphaned network reservations", err)
		return storeapi.ListOrphanedNetworkReservations500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	out := storeapi.NetworkReservationList{Reservations: make([]storeapi.NetworkReservation, 0, len(rs))}
	for _, r := range rs {
		out.Reservations = append(out.Reservations, resToAPI(r))
	}
	return storeapi.ListOrphanedNetworkReservations200JSONResponse(out), nil
}

func (h *handlers) MarkNetworkReservationOrphaned(ctx context.Context, req storeapi.MarkNetworkReservationOrphanedRequestObject) (storeapi.MarkNetworkReservationOrphanedResponseObject, error) {
	if !validMachineName(req.Name) {
		return storeapi.MarkNetworkReservationOrphaned400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	}
	mac, ok := normMAC(req.Mac)
	if !ok {
		return storeapi.MarkNetworkReservationOrphaned400JSONResponse{BadRequestJSONResponse: fieldErr("mac")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.MarkNetworkReservationOrphaned500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	r, err := h.img.db.MarkReservationOrphaned(ctx, req.Name, mac)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.MarkNetworkReservationOrphaned404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("mark network reservation orphaned", err)
		return storeapi.MarkNetworkReservationOrphaned500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.MarkNetworkReservationOrphaned200JSONResponse(resToAPI(r)), nil
}

func (h *handlers) ListNetworkReservations(ctx context.Context, req storeapi.ListNetworkReservationsRequestObject) (storeapi.ListNetworkReservationsResponseObject, error) {
	if !validMachineName(req.Name) {
		return storeapi.ListNetworkReservations400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.ListNetworkReservations500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	rs, err := h.img.db.NetworkReservations(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.ListNetworkReservations404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("list network reservations", err)
		return storeapi.ListNetworkReservations500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	out := storeapi.NetworkReservationList{Reservations: make([]storeapi.NetworkReservation, 0, len(rs))}
	for _, r := range rs {
		out.Reservations = append(out.Reservations, resToAPI(r))
	}
	return storeapi.ListNetworkReservations200JSONResponse(out), nil
}

func (h *handlers) PutNetworkReservation(ctx context.Context, req storeapi.PutNetworkReservationRequestObject) (storeapi.PutNetworkReservationResponseObject, error) {
	bad := func(f string) (storeapi.PutNetworkReservationResponseObject, error) {
		return storeapi.PutNetworkReservation400JSONResponse{BadRequestJSONResponse: fieldErr(f)}, nil
	}
	b := req.Body
	if !validMachineName(req.Name) {
		return bad("name")
	}
	mac, macOK := normMAC(req.Mac)
	if !macOK {
		return bad("mac")
	}
	if b == nil {
		return bad("body")
	}
	ip, ipOK := lanAddr(b.Ip)
	switch {
	case !ipOK:
		return bad("ip")
	case !validHostName(b.Host):
		return bad("host")
	case !validMachineName(b.Vm):
		return bad("vm")
	case b.Uuid != nil && *b.Uuid != "" && !validUUID(*b.Uuid):
		return bad("uuid")
	case !h.img.dbReady():
		return storeapi.PutNetworkReservation500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	uuid := ""
	if b.Uuid != nil {
		uuid = *b.Uuid
	}
	r, err := h.img.db.PutNetworkReservation(ctx, db.NetworkReservation{Network: req.Name, MAC: mac, IP: ip, Host: b.Host, VM: b.Vm, UUID: uuid})
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.PutNetworkReservation404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case errors.Is(err, db.ErrAddressTaken):
		return storeapi.PutNetworkReservation409JSONResponse{Error: err.Error()}, nil
	case err != nil:
		h.img.fail("put network reservation", err)
		return storeapi.PutNetworkReservation500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.PutNetworkReservation200JSONResponse(resToAPI(r)), nil
}

func (h *handlers) DeleteNetworkReservation(ctx context.Context, req storeapi.DeleteNetworkReservationRequestObject) (storeapi.DeleteNetworkReservationResponseObject, error) {
	if !validMachineName(req.Name) {
		return storeapi.DeleteNetworkReservation400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	}
	mac, ok := normMAC(req.Mac)
	if !ok {
		return storeapi.DeleteNetworkReservation400JSONResponse{BadRequestJSONResponse: fieldErr("mac")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.DeleteNetworkReservation500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	err := h.img.db.DeleteNetworkReservation(ctx, req.Name, mac)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DeleteNetworkReservation404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("delete network reservation", err)
		return storeapi.DeleteNetworkReservation500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.DeleteNetworkReservation204Response{}, nil
}
