package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jaredeh/vmtool/internal/store/blob"
	"github.com/jaredeh/vmtool/internal/store/db"
	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/tokenhash"
)

// Server is the store's HTTP server.
type Server struct {
	cfg    *Config
	log    *slog.Logger
	routes *routeTable
	img    *images
}

// Deps are the store's backends. Image routes answer 500 (fixed text) when
// Blob or DB is nil; Now defaults to time.Now and exists so tests can move
// the clock past a ticket's expiry or the inventory's grace period (a set Now
// also becomes the database's clock).
type Deps struct {
	Blob blob.BlobStore
	DB   *db.DB
	Now  func() time.Time
}

// NewServer returns a Server for a validated Config. A nil log uses
// slog.Default().
func NewServer(c *Config, log *slog.Logger, d Deps) *Server {
	if log == nil {
		log = slog.Default()
	}
	if d.DB != nil && d.Now != nil {
		// The inventory rules (gone_since, the sweep's cutoff) read the store's clock.
		d.DB.SetClock(d.Now)
	}
	return &Server{cfg: c, log: log, routes: newRouteTable(), img: newImages(log, d)}
}

// Handler is the whole request pipeline: log -> auth -> generated routes.
func (s *Server) Handler() http.Handler {
	strict := storeapi.NewStrictHandlerWithOptions(&handlers{version: specVersion(), img: s.img}, nil, storeapi.StrictHTTPServerOptions{
		// Fixed text only: a decoder error can quote the input, which may be a secret.
		RequestErrorHandlerFunc:  func(w http.ResponseWriter, _ *http.Request, _ error) { writeError(w, 400, "bad request") },
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) { writeError(w, 500, "internal error") },
	})
	api := storeapi.HandlerWithOptions(strict, storeapi.StdHTTPServerOptions{
		BaseRouter:       http.NewServeMux(),
		ErrorHandlerFunc: paramError,
	})
	return s.logRequests(s.requireAuth(guardCredentialBody(api)))
}

// paramError answers a request whose parameters the generated wrapper
// rejected. A missing required parameter is named (the name comes from the
// spec); anything else is fixed text, because the wrapper's other errors can
// quote the value.
func paramError(w http.ResponseWriter, _ *http.Request, err error) {
	var req *storeapi.RequiredParamError
	if errors.As(err, &req) {
		writeError(w, 400, "invalid field: "+req.ParamName)
		return
	}
	writeError(w, 400, "bad request")
}

// httpServer validates the listening rule again (a Config built by hand is
// not trusted) and returns the server to run. It never binds.
func (s *Server) httpServer() (*http.Server, error) {
	if err := s.cfg.checkListen(); err != nil {
		return nil, err
	}
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if s.cfg.UsesTLS() {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load tls_cert/tls_key: %w", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	}
	return srv, nil
}

// ListenAndServe checks the configuration, binds and serves until it fails.
func (s *Server) ListenAndServe() error {
	srv, err := s.httpServer()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	if srv.TLSConfig != nil {
		return srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	}
	return srv.Serve(ln)
}

// ---- routes -------------------------------------------------------------

// routeTable matches a request to its spec route without running a handler.
// The log and auth middleware sit outside the real mux and need the matched
// pattern; the public set comes from the embedded spec (`security: []`), so
// it cannot drift from the contract. If the spec cannot be loaded nothing is
// public.
type routeTable struct {
	mux    *http.ServeMux
	public map[string]bool // "GET /version" -> true
}

func newRouteTable() *routeTable {
	rt := &routeTable{mux: http.NewServeMux(), public: map[string]bool{}}
	doc, err := storeapi.GetSwagger()
	if err != nil {
		slog.Error("load embedded spec; no route is public")
		return rt
	}
	noop := func(http.ResponseWriter, *http.Request) {}
	for p, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			pat := strings.ToUpper(method) + " " + p
			rt.mux.HandleFunc(pat, noop)
			rt.public[pat] = isPublic(doc, op)
		}
	}
	return rt
}

// isPublic reports whether op's effective security requirement is empty.
func isPublic(doc *openapi3.T, op *openapi3.Operation) bool {
	sec := doc.Security
	if op.Security != nil {
		sec = *op.Security
	}
	return len(sec) == 0
}

// match returns the spec pattern ("GET /v1/images/{name}") for r, or "" when
// no route matches. An unclean path never matches: the mux would redirect,
// and auth should not reason about where.
func (rt *routeTable) match(r *http.Request) string {
	if p := r.URL.Path; p != "/" && path.Clean(p) != p {
		return ""
	}
	_, pat := rt.mux.Handler(r)
	return pat
}

func specVersion() string {
	doc, err := storeapi.GetSwagger()
	if err != nil || doc.Info == nil {
		return "unknown"
	}
	return doc.Info.Version
}

// ---- request info, logging ----------------------------------------------

type ctxKey struct{}

// reqInfo is created by the log middleware and filled in by auth, so the log
// line carries the caller whatever copy of the request reaches it.
type reqInfo struct {
	caller string
	// contentLength is the request's declared body length (-1 if unknown).
	contentLength int64
	// ticketSHA is the image digest a redeemed download ticket was bound to.
	ticketSHA string
}

// infoFrom returns the request's reqInfo, or a throwaway one when the log
// middleware did not run (a handler called without the pipeline).
func infoFrom(ctx context.Context) *reqInfo {
	if ri, ok := ctx.Value(ctxKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{contentLength: -1}
}

func callerFrom(ctx context.Context) string {
	if ri, ok := ctx.Value(ctxKey{}).(*reqInfo); ok {
		return ri.caller
	}
	return ""
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// logRequests writes one line per request: method, route (the matched spec
// pattern, never the raw URL or query), status, caller name, duration.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{contentLength: r.ContentLength}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, ri)))
		route := "unmatched"
		if pat := s.routes.match(r); pat != "" {
			_, route, _ = strings.Cut(pat, " ")
		}
		caller := ri.caller
		if caller == "" {
			caller = "-"
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log.Info("request",
			"method", r.Method,
			"route", route,
			"status", status,
			"caller", caller,
			"duration", time.Since(start),
		)
	})
}

// ---- auth ---------------------------------------------------------------

// requireAuth rejects with 401 every request that is not a public operation
// and does not present a credential that authenticate accepts. Unknown paths
// need a token too, so an unauthenticated client learns nothing about which
// routes exist.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pat := s.routes.match(r)
		if pat != "" && s.routes.public[pat] {
			next.ServeHTTP(w, r)
			return
		}
		name, ok := s.authenticate(r, pat)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, unauthorizedMsg)
			return
		}
		if ri, ok := r.Context().Value(ctxKey{}).(*reqInfo); ok {
			ri.caller = name
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate returns the caller's name for a valid credential on route pat
// (the matched pattern, "" when none matched). A user bearer token is
// accepted on every route; a host bearer token only on the routes in
// hostAllowed; and a download ticket (`Authorization: Ticket <t>`) on exactly
// "GET /v1/images/{name}/blob" for its own image (see ticketCaller); every
// other route refuses a ticket without spending it.
func (s *Server) authenticate(r *http.Request, pat string) (string, bool) {
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return "", false
	}
	scheme, cred, ok := strings.Cut(vals[0], " ")
	if !ok || cred == "" || strings.ContainsAny(cred, " \t") {
		return "", false
	}
	if strings.EqualFold(scheme, "Bearer") {
		if name, ok := s.bearerCaller(cred); ok {
			return name, true
		}
		return s.hostCaller(cred, pat)
	}
	if strings.EqualFold(scheme, "Ticket") {
		return s.ticketCaller(r, pat, cred)
	}
	return "", false
}

// bearerCaller finds the name whose hash matches tok. It checks every entry,
// so the time taken does not depend on which one matched.
func (s *Server) bearerCaller(tok string) (string, bool) {
	var found string
	for name, h := range s.cfg.Tokens {
		if tokenhash.Check(tok, h) {
			found = name
		}
	}
	return found, found != ""
}

// hostAllowed is the whole of what a host token may do: the routes, by spec
// pattern. The report route is in the allow-list before it is in the spec,
// so the list does not change when it arrives. The networks read answers
// 403 for any host but the token's own (the handler checks the path).
var hostAllowed = map[string]bool{
	"GET /v1/whoami":                 true,
	"PUT /v1/hosts/{host}/inventory": true,
	"GET /v1/hosts/{host}/networks":  true,
}

// hostCallerPrefix starts the name of a host token's caller. User token names
// (nameRE) cannot contain '/', so the prefix tells the two kinds apart.
const hostCallerPrefix = "host/"

// hostCaller finds the host whose token hash matches tok and answers
// "host/<name>", but only on a route in hostAllowed; on any other route
// (pat "" included) the token is as good as unknown. Every entry is checked,
// as in bearerCaller.
func (s *Server) hostCaller(tok, pat string) (string, bool) {
	var found string
	for host, h := range s.cfg.HostTokens {
		if tokenhash.Check(tok, h) {
			found = host
		}
	}
	if found == "" || !hostAllowed[pat] {
		return "", false
	}
	return hostCallerPrefix + found, true
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(storeapi.Error{Error: msg})
}

// ---- handlers -----------------------------------------------------------

type handlers struct {
	version string
	img     *images
}

var _ storeapi.StrictServerInterface = (*handlers)(nil)

func (h *handlers) GetVersion(context.Context, storeapi.GetVersionRequestObject) (storeapi.GetVersionResponseObject, error) {
	return storeapi.GetVersion200JSONResponse{Version: h.version}, nil
}

func (h *handlers) Whoami(ctx context.Context, _ storeapi.WhoamiRequestObject) (storeapi.WhoamiResponseObject, error) {
	name := callerFrom(ctx)
	kind := storeapi.WhoamiKindUser
	if strings.HasPrefix(name, hostCallerPrefix) {
		kind = storeapi.WhoamiKindHost
	}
	return storeapi.Whoami200JSONResponse{Name: name, Kind: &kind}, nil
}
