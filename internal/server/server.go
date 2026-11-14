package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
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
}

// Handler returns the generated mux wrapped around StrictServerInterface,
// behind the bearer check. There is no unauthenticated mode.
func (s *Server) Handler() http.Handler {
	if s.PlaybookDir == "" {
		s.PlaybookDir = "ansible/playbooks"
	}
	h := &handlers{
		svc:    &app.Service{PlaybookDir: s.PlaybookDir},
		listen: s.Listen,
	}
	var hash string
	if s.Config != nil {
		hash = s.Config.TokenSHA256
	}
	public := publicRoutes()
	return chain(h, func(next http.Handler) http.Handler {
		return requireBearer(hash, public, next)
	})
}

// handlerFor is the unauthenticated chain, for handler-level tests only.
func handlerFor(h *handlers) http.Handler { return chain(h, nil) }

// chain wires: request log -> auth (if any) -> body limit -> mux. Auth runs
// before any body is read or handler runs; the log still records a 401.
func chain(h *handlers, auth func(http.Handler) http.Handler) http.Handler {
	if h.svc == nil {
		h.svc = &app.Service{PlaybookDir: "ansible/playbooks"}
	}
	strict := api.NewStrictHandler(h, nil)
	var next http.Handler = withMaxBytes(withRequest(api.HandlerFromMux(strict, http.NewServeMux())))
	if auth != nil {
		next = auth(next)
	}
	return slogRequests(next)
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
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
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
// refusal happens before binding.
func (s *Server) ListenAndServe() error {
	srv, err := s.httpServer()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	slog.Info("vmtool API listening", "url", "https://"+srv.Addr+"/")
	return serve(srv, ln)
}

func withMaxBytes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
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

func slogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.code,
			"duration", time.Since(start),
		)
	})
}
