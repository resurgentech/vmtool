package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/config"
)

// publicRoutes returns a mux matching exactly the operations whose effective
// security in the spec is empty (`security: []`). It is built from the
// embedded spec so the exempt set cannot drift from the contract. If the spec
// cannot be loaded the mux is empty and every route requires the token.
func publicRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	doc, err := api.GetSwagger()
	if err != nil {
		slog.Error("load embedded spec; no route is exempt from auth", "err", err)
		return mux
	}
	noop := func(http.ResponseWriter, *http.Request) {}
	for p, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !isPublic(doc, op) {
				continue
			}
			pattern := p
			if strings.HasSuffix(pattern, "/") {
				pattern += "{$}" // same form oapi-codegen emits: exact match, not a subtree
			}
			mux.HandleFunc(strings.ToUpper(method)+" "+pattern, noop)
		}
	}
	return mux
}

// isPublic reports whether op's effective security requirement is empty.
func isPublic(doc *openapi3.T, op *openapi3.Operation) bool {
	sec := doc.Security
	if op.Security != nil {
		sec = *op.Security
	}
	return len(sec) == 0
}

// requireBearer rejects, with 401, every request that is not a public
// operation and does not carry `Authorization: Bearer <token>` whose SHA-256
// equals hash. An empty hash matches nothing. Unknown paths need the token
// too, so an unauthenticated client learns nothing about which routes exist.
func requireBearer(hash string, public *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicRequest(public, r) {
			next.ServeHTTP(w, r)
			return
		}
		tok, ok := bearerToken(r)
		if !ok || !config.Check(tok, hash) {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPublicRequest(public *http.ServeMux, r *http.Request) bool {
	// An unclean path would make ServeMux redirect; require the token rather
	// than reason about where the redirect lands.
	if p := r.URL.Path; p != "/" && path.Clean(p) != p {
		return false
	}
	_, pattern := public.Handler(r)
	return pattern != ""
}

// bearerToken extracts the token from exactly one
// `Authorization: Bearer <token>` header. The scheme is case-insensitive; the
// token must be non-empty and contain no whitespace.
func bearerToken(r *http.Request) (string, bool) {
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", false
	}
	return tok, true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(api.Error{Error: "unauthorized: missing or invalid bearer token"})
}
