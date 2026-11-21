package store

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/jaredeh/vmtool/internal/store/db"
	"github.com/jaredeh/vmtool/internal/storeapi"
)

// Credential limits. A secret field is a password or a key; a PUT body that
// holds the largest legal record is well under maxCredentialBody.
const (
	maxSecretLen      = 64 * 1024
	maxAddressLen     = 1024
	maxNameLen        = 128
	maxCredentialBody = 1 << 20
)

var (
	// Mirrors pkg/vmtool machineNameRE and ValidHostName (which cannot be
	// imported: pkg/vmtool pulls in libvirt), plus a length bound.
	machineNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	uuidRE        = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

func validMachineName(n string) bool {
	return len(n) <= maxNameLen && machineNameRE.MatchString(n)
}

func validHostName(n string) bool { return validMachineName(n) && !strings.Contains(n, ".") }

// validSecret accepts a bounded string with no NUL. Newlines are legal: a
// private key is multi-line.
func validSecret(s string) bool {
	return len(s) <= maxSecretLen && !strings.ContainsRune(s, 0)
}

func validAddress(s string) bool {
	if s == "" || len(s) > maxAddressLen {
		return false
	}
	return strings.IndexFunc(s, unicode.IsControl) < 0
}

// fieldErr is a 400 body that names a field and never its value.
func fieldErr(field string) storeapi.BadRequestJSONResponse {
	return storeapi.BadRequestJSONResponse{Error: "invalid field: " + field}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// guardCredentialBody sits in front of the generated routes. For a PUT to a
// credential route or a host's inventory report (/v1/hosts/), and a PUT or
// POST to a network route (/v1/networks), it requires a JSON content type, refuses a declared body
// over maxCredentialBody (413) and caps what an undeclared (chunked) body can
// deliver; the decoder then fails with the fixed "bad request".
func guardCredentialBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes := r.Method == http.MethodPut || r.Method == http.MethodPost
		if (r.Method == http.MethodPut && (strings.HasPrefix(r.URL.Path, "/v1/credentials/") || strings.HasPrefix(r.URL.Path, "/v1/hosts/"))) ||
			(writes && (r.URL.Path == "/v1/networks" || strings.HasPrefix(r.URL.Path, "/v1/networks/"))) {
			// The orphan mark has no body: it needs no content type.
			bodyless := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/orphan")
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); !bodyless && (err != nil || mt != "application/json") {
				writeError(w, http.StatusBadRequest, "invalid field: Content-Type")
				return
			}
			if r.ContentLength > maxCredentialBody {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxCredentialBody)
		}
		next.ServeHTTP(w, r)
	})
}

func (i *images) dbReady() bool { return i != nil && i.db != nil }

// ---- VM credentials -------------------------------------------------------

func (h *handlers) GetVMCredential(ctx context.Context, req storeapi.GetVMCredentialRequestObject) (storeapi.GetVMCredentialResponseObject, error) {
	switch {
	case !validHostName(req.Host):
		return storeapi.GetVMCredential400JSONResponse{BadRequestJSONResponse: fieldErr("host")}, nil
	case !validMachineName(req.Name):
		return storeapi.GetVMCredential400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	case !uuidRE.MatchString(req.Params.Uuid):
		return storeapi.GetVMCredential400JSONResponse{BadRequestJSONResponse: fieldErr("uuid")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.GetVMCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	c, err := h.img.db.GetVMCredential(ctx, req.Host, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.GetVMCredential404JSONResponse{Error: "not found"}, nil
	case err != nil:
		h.img.fail("get vm credential", err)
		return storeapi.GetVMCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	if c.UUID != strings.ToLower(req.Params.Uuid) {
		// A record for another domain is never used, and none of it is sent.
		stale := true
		return storeapi.GetVMCredential404JSONResponse{Error: "not found", Stale: &stale}, nil
	}
	return storeapi.GetVMCredential200JSONResponse{
		Host: c.Host, Name: c.Name, Uuid: c.UUID, Username: c.Username, Password: c.Password,
		PrivateKey: c.PrivateKey, PublicKey: c.PublicKey, Updated: c.Updated,
	}, nil
}

func (h *handlers) PutVMCredential(ctx context.Context, req storeapi.PutVMCredentialRequestObject) (storeapi.PutVMCredentialResponseObject, error) {
	bad := func(f string) (storeapi.PutVMCredentialResponseObject, error) {
		return storeapi.PutVMCredential400JSONResponse{BadRequestJSONResponse: fieldErr(f)}, nil
	}
	b := req.Body
	switch {
	case !validHostName(req.Host):
		return bad("host")
	case !validMachineName(req.Name):
		return bad("name")
	case b == nil:
		return bad("body")
	case !uuidRE.MatchString(b.Uuid):
		return bad("uuid")
	case b.Username == "" || !validSecret(b.Username):
		return bad("username")
	case !validSecret(deref(b.Password)):
		return bad("password")
	case !validSecret(deref(b.PrivateKey)):
		return bad("private_key")
	case !validSecret(deref(b.PublicKey)):
		return bad("public_key")
	}
	if !h.img.dbReady() {
		return storeapi.PutVMCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	rec := db.VMCredential{
		Host: req.Host, Name: req.Name, UUID: strings.ToLower(b.Uuid), Username: b.Username,
		Password: deref(b.Password), PrivateKey: deref(b.PrivateKey), PublicKey: deref(b.PublicKey),
	}
	if err := h.img.db.PutVMCredential(ctx, rec); err != nil {
		h.img.fail("put vm credential", err)
		return storeapi.PutVMCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	got, err := h.img.db.GetVMCredential(ctx, req.Host, req.Name)
	if err != nil {
		h.img.fail("get vm credential", err)
		return storeapi.PutVMCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.PutVMCredential200JSONResponse{Host: got.Host, Name: got.Name, Uuid: got.UUID, Updated: got.Updated}, nil
}

func (h *handlers) DeleteVMCredential(ctx context.Context, req storeapi.DeleteVMCredentialRequestObject) (storeapi.DeleteVMCredentialResponseObject, error) {
	switch {
	case !validHostName(req.Host):
		return storeapi.DeleteVMCredential400JSONResponse{BadRequestJSONResponse: fieldErr("host")}, nil
	case !validMachineName(req.Name):
		return storeapi.DeleteVMCredential400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	case !h.img.dbReady():
		return storeapi.DeleteVMCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	err := h.img.db.DeleteVMCredential(ctx, req.Host, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DeleteVMCredential404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("delete vm credential", err)
		return storeapi.DeleteVMCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.DeleteVMCredential204Response{}, nil
}

// ---- machine credentials --------------------------------------------------

func validKind(k string) bool { return k == "metal" || k == "alien" }

func (h *handlers) ListMachineCredentials(ctx context.Context, _ storeapi.ListMachineCredentialsRequestObject) (storeapi.ListMachineCredentialsResponseObject, error) {
	if !h.img.dbReady() {
		return storeapi.ListMachineCredentials500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	rows, err := h.img.db.ListMachines(ctx)
	if err != nil {
		h.img.fail("list machines", err)
		return storeapi.ListMachineCredentials500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	out := storeapi.MachineList{Machines: make([]storeapi.MachineSummary, 0, len(rows))}
	for _, m := range rows {
		out.Machines = append(out.Machines, storeapi.MachineSummary{Name: m.Name, Kind: m.Kind})
	}
	return storeapi.ListMachineCredentials200JSONResponse(out), nil
}

func (h *handlers) GetMachineCredential(ctx context.Context, req storeapi.GetMachineCredentialRequestObject) (storeapi.GetMachineCredentialResponseObject, error) {
	if !validMachineName(req.Name) {
		return storeapi.GetMachineCredential400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	}
	if !h.img.dbReady() {
		return storeapi.GetMachineCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	m, err := h.img.db.GetMachine(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.GetMachineCredential404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("get machine", err)
		return storeapi.GetMachineCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.GetMachineCredential200JSONResponse{
		Name: m.Name, Kind: m.Kind, Address: m.Address, Username: m.Credentials.Username,
		Password: m.Credentials.Password, PrivateKey: m.Credentials.PrivateKey,
		PublicKey: m.Credentials.PublicKey, Updated: m.Updated,
	}, nil
}

func (h *handlers) PutMachineCredential(ctx context.Context, req storeapi.PutMachineCredentialRequestObject) (storeapi.PutMachineCredentialResponseObject, error) {
	bad := func(f string) (storeapi.PutMachineCredentialResponseObject, error) {
		return storeapi.PutMachineCredential400JSONResponse{BadRequestJSONResponse: fieldErr(f)}, nil
	}
	b := req.Body
	switch {
	case !validMachineName(req.Name):
		return bad("name")
	case b == nil:
		return bad("body")
	case !validKind(string(b.Kind)):
		return bad("kind")
	case !validAddress(b.Address):
		return bad("address")
	case b.Username == "" || !validSecret(b.Username):
		return bad("username")
	case !validSecret(deref(b.Password)):
		return bad("password")
	case !validSecret(deref(b.PrivateKey)):
		return bad("private_key")
	case !validSecret(deref(b.PublicKey)):
		return bad("public_key")
	}
	if !h.img.dbReady() {
		return storeapi.PutMachineCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	rec := db.Machine{
		Name: req.Name, Kind: string(b.Kind), Address: b.Address,
		Credentials: db.Credentials{Username: b.Username, Password: deref(b.Password), PrivateKey: deref(b.PrivateKey), PublicKey: deref(b.PublicKey)},
	}
	if err := h.img.db.PutMachine(ctx, rec); err != nil {
		h.img.fail("put machine", err)
		return storeapi.PutMachineCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	got, err := h.img.db.GetMachine(ctx, req.Name)
	if err != nil {
		h.img.fail("get machine", err)
		return storeapi.PutMachineCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.PutMachineCredential200JSONResponse{Name: got.Name, Kind: got.Kind, Updated: got.Updated}, nil
}

func (h *handlers) DeleteMachineCredential(ctx context.Context, req storeapi.DeleteMachineCredentialRequestObject) (storeapi.DeleteMachineCredentialResponseObject, error) {
	switch {
	case !validMachineName(req.Name):
		return storeapi.DeleteMachineCredential400JSONResponse{BadRequestJSONResponse: fieldErr("name")}, nil
	case !h.img.dbReady():
		return storeapi.DeleteMachineCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	err := h.img.db.DeleteMachine(ctx, req.Name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return storeapi.DeleteMachineCredential404JSONResponse{NotFoundJSONResponse: notFound}, nil
	case err != nil:
		h.img.fail("delete machine", err)
		return storeapi.DeleteMachineCredential500JSONResponse{InternalErrorJSONResponse: internalErr}, nil
	}
	return storeapi.DeleteMachineCredential204Response{}, nil
}
