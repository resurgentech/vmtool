package store

import (
	"context"
	"errors"
	"strings"

	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/vmnotes"
)

// A VM's notes (origin and comment): kept in the store only, keyed (host,
// uuid), written through PUT /v1/inventory/{host}/{name}/notes by user tokens
// (a host token is refused by the allow-list, server.go hostAllowed). No
// report touches them.

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// originToAPI is a stored origin as the API carries it; nil for none.
func originToAPI(o *db.Origin) *storeapi.VMOrigin {
	if o == nil {
		return nil
	}
	return &storeapi.VMOrigin{
		Image: optStr(o.Image), ImageSha256: optStr(o.SHA256), Template: optStr(o.Template),
		Clone: optStr(o.Clone), Pool: optStr(o.Pool), Created: optStr(o.Created),
	}
}

// originFromAPI validates an origin from a request. The field name is
// "origin" for any fault; never a value.
func originFromAPI(o *storeapi.VMOrigin) (*db.Origin, bool) {
	if o == nil {
		return nil, true
	}
	for _, s := range []*string{o.Image, o.ImageSha256, o.Template, o.Clone, o.Pool, o.Created} {
		if !plainText(deref(s), maxOriginField) {
			return nil, false
		}
	}
	if !vmnotes.ValidCloneKind(deref(o.Clone)) {
		return nil, false
	}
	return &db.Origin{
		Image: deref(o.Image), SHA256: deref(o.ImageSha256), Template: deref(o.Template),
		Clone: deref(o.Clone), Pool: deref(o.Pool), Created: deref(o.Created),
	}, true
}

func (h *handlers) PutVMNotes(ctx context.Context, req storeapi.PutVMNotesRequestObject) (storeapi.PutVMNotesResponseObject, error) {
	bad := func(f string) (storeapi.PutVMNotesResponseObject, error) {
		return storeapi.PutVMNotes400JSONResponse{BadRequestJSONResponse: fieldErr(f)}, nil
	}
	b := req.Body
	switch {
	case !validHostName(req.Host):
		return bad("host")
	case !validMachineName(req.Name):
		return bad("name")
	case b == nil || (b.Origin == nil && b.Comment == nil):
		return bad("body")
	case b.Uuid != nil && !uuidRE.MatchString(*b.Uuid):
		return bad("uuid")
	case b.Comment != nil && vmnotes.ValidateComment(*b.Comment) != nil:
		return bad("comment")
	}
	origin, ok := originFromAPI(b.Origin)
	if !ok {
		return bad("origin")
	}
	if !h.img.dbReady() {
		return storeapi.PutVMNotes500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	n, err := h.img.db.PutNotes(ctx, db.NotesWrite{
		Host: req.Host, Name: req.Name, UUID: strings.ToLower(deref(b.Uuid)), Origin: origin, Comment: b.Comment,
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.PutVMNotes404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case errors.Is(err, db.ErrGone), errors.Is(err, db.ErrOriginRecorded):
		return storeapi.PutVMNotes409JSONResponse{Error: err.Error()}, nil
	case err != nil:
		h.img.fail("put notes", err)
		return storeapi.PutVMNotes500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.PutVMNotes200JSONResponse{
		Host: n.Host, Name: n.Name, Uuid: n.UUID, Origin: originToAPI(n.Origin), Comment: n.Comment,
	}, nil
}
