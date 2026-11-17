package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
)

// Server is the vmtool REST API.
type Server struct {
	// Listen overrides Config.Listen when set (the --listen flag).
	Listen      string
	PlaybookDir string
	// Config is the loaded config.yml. Every non-public request needs the
	// bearer token whose SHA-256 is Config.TokenSHA256. A nil Config or an
	// empty hash authenticates nothing, and ListenAndServe refuses both.
	Config *config.Config

	// h is the handlers Handler last built (ListenAndServe warms its pool
	// directory).
	h *handlers
}

// Handler returns the generated mux wrapped around StrictServerInterface,
// behind the bearer check. There is no unauthenticated mode.
func (s *Server) Handler() http.Handler {
	if s.PlaybookDir == "" {
		s.PlaybookDir = "ansible/playbooks"
	}
	r := hosts.New(s.Config)
	h := &handlers{
		svc: &app.Service{
			PlaybookDir: s.PlaybookDir,
			HostNames:   r.AllNames(),
			// createVM on a remote host uploads the image from this
			// machine's own pools.
			LocalImages: localImages(r, s.Config == nil || s.Config.Role != config.RoleHypervisor),
		},
		listen: s.Listen,
		hosts:  r,
	}
	if s.Config != nil {
		h.role = s.Config.Role
	}
	s.h = h
	var hash string
	if s.Config != nil {
		hash = s.Config.TokenSHA256
	}
	public := publicRoutes()
	return chain(h, func(next http.Handler) http.Handler {
		return requireBearer(hash, public, next)
	})
}

// localImages finds an image in the local host's libvirt pools, opening the
// local Manager for the lookup only.
func localImages(r *hosts.Resolver, workstation bool) func(string) (string, error) {
	return func(name string) (string, error) {
		m, done, err := r.Resolve(r.Local())
		if err != nil {
			return "", err
		}
		defer done()
		return app.FindLocalImage(m, workstation, name)
	}
}

// handlerFor is the unauthenticated chain, for handler-level tests only.
func handlerFor(h *handlers) http.Handler { return chain(h, nil) }

// chain wires: request log -> auth (if any) -> body limit -> mux. Auth runs
// before any body is read or handler runs; the log still records a 401.
func chain(h *handlers, auth func(http.Handler) http.Handler) http.Handler {
	if h.hosts == nil {
		h.hosts = hosts.New(nil)
	}
	if h.svc == nil {
		h.svc = &app.Service{PlaybookDir: "ansible/playbooks", HostNames: h.hosts.AllNames()}
	}
	// tunnelMux serves openVMTunnel itself (it takes over the connection);
	// every other operation goes to the strict adapter.
	mux := tunnelMux{ServerInterface: api.NewStrictHandler(h, []api.StrictMiddlewareFunc{noteOperation}), h: h}
	var next http.Handler = withMaxBytes(withRequest(api.HandlerFromMux(mux, http.NewServeMux())))
	// A hypervisor refuses the workstation-only operations here, after auth
	// and before the body is read or any handler runs.
	next = gateWorkstationOnly(h.role, h.hosts.Local(), next)
	if auth != nil {
		next = auth(next)
	}
	return slogRequests(h, next)
}

// addr is the listen address: the Listen field (flag), else config.yml
// listen, else the default.
func (s *Server) addr() string {
	switch {
	case s.Listen != "":
		return s.Listen
	case s.Config != nil && s.Config.Listen != "":
		return s.Config.Listen
	}
	return config.DefaultListen
}

// httpServer validates the configuration and returns the server to run. It
// never binds. Whatever the listen address, loopback included, config.yml
// must carry token_sha256, tls_cert and tls_key: vmtool serves HTTPS with
// bearer auth or not at all.
func (s *Server) httpServer() (*http.Server, error) {
	c := s.Config
	if c == nil {
		return nil, fmt.Errorf("no config.yml: vmtool server needs token_sha256, tls_cert and tls_key; run 'just install'")
	}
	if c.TokenSHA256 == "" {
		return nil, fmt.Errorf("config.yml has no token_sha256, so no request could authenticate; run 'just install'")
	}
	switch {
	case c.TLSCert == "" && c.TLSKey == "":
		return nil, fmt.Errorf("config.yml has no tls_cert/tls_key; vmtool server serves HTTPS only; run 'just install'")
	case c.TLSKey == "":
		return nil, fmt.Errorf("config.yml sets tls_cert but not tls_key")
	case c.TLSCert == "":
		return nil, fmt.Errorf("config.yml sets tls_key but not tls_cert")
	}
	cert, err := tls.LoadX509KeyPair(c.Path(c.TLSCert), c.Path(c.TLSKey))
	if err != nil {
		return nil, fmt.Errorf("load tls_cert/tls_key: %w", err)
	}
	return &http.Server{
		Addr:              s.addr(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// ALPN offers HTTP/1.1 only. Without "h2" in NextProtos, Serve does
		// not configure HTTP/2, so every client speaks HTTP/1.1 and the
		// tunnel (openVMTunnel) can always take over the connection.
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
		},
	}, nil
}

// serve runs srv on ln over TLS. srv.TLSConfig must be set (httpServer
// always sets it); a server without one is refused rather than served plain.
func serve(srv *http.Server, ln net.Listener) error {
	if srv.TLSConfig == nil {
		ln.Close()
		return fmt.Errorf("refusing to serve without TLS")
	}
	return srv.Serve(tls.NewListener(ln, srv.TLSConfig))
}

// ListenAndServe checks the configuration, then binds and serves. Every
// refusal happens before binding. Between binding and serving, a hypervisor
// removes its image cache's stale temporary files (startCleanup).
func (s *Server) ListenAndServe() error {
	srv, err := s.httpServer()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	// After binding: holding the port means no other daemon on it is
	// mid-upload, so its temporary files are stale.
	startCleanup(s.Config.Role, hosts.New(s.Config))
	if s.Config.Role == config.RoleHypervisor {
		if s.h != nil {
			s.h.warmPoolDir()
		}
	}
	slog.Info("vmtool API listening", "url", "https://"+srv.Addr+"/")
	return serve(srv, ln)
}

// withMaxBytes caps every request body at 1 MiB, except uploadImage's: an
// image is streamed to disk (never held in memory) and bounded by its
// Content-Length instead (images.go).
func withMaxBytes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && !isUpload(r) {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("connection hijack not supported")
	}
	w.code = http.StatusSwitchingProtocols
	return h.Hijack()
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey, r)))
	})
}

// slowRequest is how long a request may take before it is logged as slow.
const slowRequest = 5 * time.Second

// reqNote is what a request's handler tells the request log: the operation
// id and the path's host and name (a VM or an image). Nothing else about the
// request is kept: never the body, a header or the query string.
type reqNote struct{ op, host, name string }

type noteKeyType struct{}

var noteKey noteKeyType

// noteOperation is the strict handler's middleware: it records the
// operation id and path parameters for the request log.
func noteOperation(f api.StrictHandlerFunc, operationID string) api.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		// The generated name is the spec's operationId, capitalised.
		setNote(ctx, strings.ToLower(operationID[:1])+operationID[1:], r)
		return f(ctx, w, r, request)
	}
}

// setNote fills the request's note, if it has one (slogRequests made it).
func setNote(ctx context.Context, op string, r *http.Request) {
	if n, ok := ctx.Value(noteKey).(*reqNote); ok && n != nil {
		n.op, n.host, n.name = op, r.PathValue("host"), r.PathValue("name")
	}
}

func slogRequests(h *handlers, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		note := &reqNote{}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), noteKey, note)))
		d := time.Since(start)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.code,
			"duration", d,
		)
		limit := h.slowAfter
		if limit <= 0 {
			limit = slowRequest
		}
		// A tunnel session (101) lasts as long as the user's ssh does; that
		// is not a slow request.
		if d > limit && sw.code != http.StatusSwitchingProtocols {
			slog.Warn("slow request",
				"operation", note.op,
				"host", note.host,
				"name", note.name,
				"status", sw.code,
				"duration", d,
			)
		}
	})
}
