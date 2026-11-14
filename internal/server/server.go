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
)

// Server is the vmtool REST API.
type Server struct {
	// Listen overrides Config.Listen when set (the --listen flag).
	Listen      string
	PlaybookDir string
	// Config is the loaded config.yml. With a Config, every non-public
	// request needs the bearer token whose SHA-256 is Config.TokenSHA256 (an
	// empty hash rejects everything). Nil means no config.yml: transitional
	// plain HTTP on loopback with no authentication (removed in T5).
	Config *config.Config
}

// Handler returns the generated mux wrapped around StrictServerInterface,
// behind the bearer check when s.Config is set.
func (s *Server) Handler() http.Handler {
	if s.PlaybookDir == "" {
		s.PlaybookDir = "ansible/playbooks"
	}
	h := &handlers{
		svc:    &app.Service{PlaybookDir: s.PlaybookDir},
		listen: s.Listen,
	}
	if s.Config == nil {
		return handlerFor(h)
	}
	hash, public := s.Config.TokenSHA256, publicRoutes()
	return chain(h, func(next http.Handler) http.Handler {
		return requireBearer(hash, public, next)
	})
}

// handlerFor is the unauthenticated chain (tests and the no-config mode).
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

// httpServer validates the bind rule and TLS settings and returns the server
// to run. It never binds. Rules: a config.yml must carry token_sha256;
// tls_cert and tls_key come together; off loopback needs both TLS and the
// hash; TLS, when configured, is served on loopback too.
func (s *Server) httpServer() (*http.Server, error) {
	addr := s.addr()
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	c := s.Config
	if c == nil {
		if !isLoopback(addr) {
			return nil, fmt.Errorf("refusing to listen on %s: off loopback requires config.yml with tls_cert, tls_key and token_sha256", addr)
		}
		slog.Warn("no config.yml: serving plain HTTP on loopback with no authentication (transitional)", "listen", addr)
		return srv, nil
	}
	if c.TokenSHA256 == "" {
		return nil, fmt.Errorf("config.yml has no token_sha256, so no request could authenticate; run `vmtool config write`")
	}
	switch {
	case c.TLSCert != "" && c.TLSKey == "":
		return nil, fmt.Errorf("config.yml sets tls_cert but not tls_key")
	case c.TLSKey != "" && c.TLSCert == "":
		return nil, fmt.Errorf("config.yml sets tls_key but not tls_cert")
	}
	if c.TLSCert == "" {
		if !isLoopback(addr) {
			return nil, fmt.Errorf("refusing to listen on %s: off loopback requires tls_cert and tls_key in config.yml", addr)
		}
		slog.Warn("config.yml has no tls_cert/tls_key: serving plain HTTP on loopback", "listen", addr)
		return srv, nil
	}
	cert, err := tls.LoadX509KeyPair(c.Path(c.TLSCert), c.Path(c.TLSKey))
	if err != nil {
		return nil, fmt.Errorf("load tls_cert/tls_key: %w", err)
	}
	srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	return srv, nil
}

// serve runs srv on ln, wrapping ln in TLS when srv.TLSConfig is set.
func serve(srv *http.Server, ln net.Listener) error {
	if srv.TLSConfig != nil {
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	return srv.Serve(ln)
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
	scheme := "http"
	if srv.TLSConfig != nil {
		scheme = "https"
	}
	slog.Info("vmtool API listening", "url", scheme+"://"+srv.Addr+"/")
	return serve(srv, ln)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
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
