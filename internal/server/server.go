package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/resurgentech/keyclient/keylist"
	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/pkg/vmtool"
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
	// StoreOptions are extra options for the store client the service builds
	// for each request (tests inject their CA). Production leaves it empty:
	// the system trust store.
	StoreOptions []storeclient.Option
	// FetchRootCAs, when set, replaces the system trust store for fetchImage's
	// download from store_url (tests inject their CA). Production leaves it nil.
	FetchRootCAs *x509.CertPool

	// now is the clock of the local token's last_used and hourly line; nil is
	// time.Now (tests inject one).
	now func() time.Time

	// keys is the pulled key list (nil unless personal_keys.url is set) and
	// local the local token's use counter; initKeys builds both, once.
	keys     *keylist.Source
	local    *localTokenUse
	keysInit bool
	keysErr  error

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
			NetworkHome: r.NetworkHome,
			// create's waits are the workstation's config.yml (nil: 120s).
			IPTimeout:  s.Config.IPWait(),
			SSHTimeout: s.Config.SSHWait(),
			// createVM on a remote host uploads the image from this
			// machine's own pools.
			Images: app.LocalImages{Dir: imageDir(s.Config), Pools: localImages(r, s.Config == nil || s.Config.Role != config.RoleHypervisor)},
		},
		listen: s.Listen,
		hosts:  r,
	}
	if s.Config != nil {
		h.role = s.Config.Role
		h.networkHome = s.Config.NetworkHome
		h.requestURL = s.Config.PersonalKeys.RequestURL
		// A hypervisor daemon's own use of the store is the inventory
		// reporter (inventory.go, with its host token from store_token_file,
		// read at start); fetchImage's download from store_url uses a ticket
		// the request brings.
		if s.Config.Role == config.RoleHypervisor {
			h.fetchURL = s.Config.StoreURL
			h.fetchRoots = s.FetchRootCAs
		}
		if s.Config.StoreConfigured() && s.Config.Role != config.RoleHypervisor {
			dir := s.Config.Dir
			h.storeURL = s.Config.Server.URL
			h.storeToken = func() (string, error) { return storeKeyOf(dir) }
			h.storeOpts = s.StoreOptions
		}
	}
	if err := s.initKeys(); err != nil {
		// httpServer refuses to start on this; a bare Handler serves with
		// token_sha256 alone.
		slog.Error(err.Error())
	}
	h.keys, h.local = s.keys, s.local
	s.h = h
	var hash string
	if s.Config != nil {
		hash = s.Config.TokenSHA256
	}
	public := publicRoutes()
	return chain(h, func(next http.Handler) http.Handler {
		return requireAuth(&authenticator{hash: hash, keys: s.keys, local: s.local}, public, next)
	})
}

// imageDir is c's resolved image_dir; "" when unset.
func imageDir(c *config.Config) string {
	if c == nil {
		return ""
	}
	return c.ResolvedImageDir()
}

// localImages finds an image in the local host's libvirt pools, opening the
// local Manager for the lookup only, and only when the own name has no
// daemon: with it listed there are no local pools to search (not found, so
// create uses the host's cached copy). With no libvirt either the answer is
// the same, not a failure.
func localImages(r *hosts.Resolver, workstation bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if r.OwnListed() {
			return "", fmt.Errorf("image %q: %s has a daemon, so it has no local pools: %w", name, r.Local(), vmtool.ErrNotFound)
		}
		m, done, err := r.Resolve(r.Local())
		if err != nil {
			return "", fmt.Errorf("image %q: %v: %w", name, err, vmtool.ErrNotFound)
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
	// create refuses a network home (hosts: vms: false), whoever built the service.
	if h.svc.NetworkHome == nil {
		c := *h.svc
		c.NetworkHome = h.hosts.NetworkHome
		h.svc = &c
	}
	// tunnelMux serves openVMTunnel itself (it takes over the connection);
	// every other operation goes to the strict adapter.
	mux := tunnelMux{ServerInterface: api.NewStrictHandlerWithOptions(h, []api.StrictMiddlewareFunc{noteOperation, h.decideStore}, handlerOptions), h: h}
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
	if err := s.initKeys(); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(c.Path(c.TLSCert), c.Path(c.TLSKey))
	if err != nil {
		return nil, fmt.Errorf("load tls_cert/tls_key: %w", err)
	}
	srv := &http.Server{
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
	}
	// A hijacked console is not tracked by Shutdown: close each with 4503.
	s.h.attachShutdown(srv)
	return srv, nil
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
	// The key list starts before the listener binds, so a request that arrives
	// first meets a loaded cache; the daemon starts with the provider down.
	keyCtx, stopKeys := context.WithCancel(context.Background())
	defer stopKeys()
	if err := s.startKeys(keyCtx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	// After binding: holding the port means no other daemon on it is
	// mid-upload, so its temporary files are stale.
	r := hosts.New(s.Config)
	// libvirt wants its event loop registered before the process opens its
	// first connection (startCleanup is the first), so the link keeper's
	// network events are set up first; its goroutine does the links.
	if s.Config.Role == config.RoleHypervisor {
		if err := vmtool.StartEventLoop(); err != nil {
			slog.Warn("libvirt's event loop did not start, so a network that libvirt starts again is not re-linked until the daemon restarts: " + err.Error())
		}
	}
	netCtx, stopNetLinks := context.WithCancel(context.Background())
	defer stopNetLinks()
	startNetLinks(netCtx, s.Config, r, slog.Default(), watchNetworkStarted)
	startCleanup(s.Config.Role, r)
	startThinCleanup(s.Config.Role, r)
	tick := time.NewTicker(ExportExpiryInterval)
	defer tick.Stop()
	startExportExpiry(s.Config.Role, r, time.Now, tick.C, nil)
	if s.Config.Role == config.RoleHypervisor {
		if s.h != nil {
			s.h.warmPoolDir()
		}
	}
	// The reporter runs in the background after the bind, so it never delays
	// serving; a VM operation only kicks it. Stopped when the server ends.
	ctx, stopReporter := context.WithCancel(context.Background())
	defer stopReporter()
	if rep, _ := startInventory(ctx, s.Config, r, slog.Default()); rep != nil && s.h != nil {
		s.h.kicker = rep
	}
	slog.Info("vmtool API listening", "url", "https://"+srv.Addr+"/")
	return serve(srv, ln)
}

// startThinCleanup is the hypervisor's other start hook: it removes the
// half-built template volumes a stopped daemon left in its thin pools (every
// vmtool_building volume, one log line each). A workstation has no thin pools
// and a helper that cannot run is one log line; neither stops the server.
func startThinCleanup(role string, r *hosts.Resolver) {
	if role != config.RoleHypervisor {
		return
	}
	r.CleanThinBuilding(slog.Default())
}

// ExportExpiryInterval is how often a hypervisor removes expired export
// volumes after the check it makes at start.
const ExportExpiryInterval = time.Hour

// startExportExpiry removes the export volumes older than 24 hours: once now
// (the daemon has just started), then at every tick, until stop is closed (a
// nil stop never is). now is the clock. It does nothing on a workstation (no
// thin pools) and logs only failures (hosts.ThinPools.ExpireExports); a
// removal that fails is tried again at the next tick. The returned channel is
// closed when the loop has ended (at once on a workstation).
func startExportExpiry(role string, r *hosts.Resolver, now func() time.Time, ticks <-chan time.Time, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	if role != config.RoleHypervisor {
		close(done)
		return done
	}
	r.ExpireExports(now(), slog.Default())
	go func() {
		defer close(done)
		for {
			select {
			case <-ticks:
				r.ExpireExports(now(), slog.Default())
			case <-stop:
				return
			}
		}
	}()
	return done
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
		if h.kicker != nil && sw.code >= 200 && sw.code < 300 && vmChangeOps[note.op] {
			h.kicker.Kick()
		}
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

// storeKeyOf is the store key in the client order: VMTOOL_SERVER_TOKEN
// (environment, then .env), then the token file. The service never files a
// key request.
func storeKeyOf(dir string) (string, error) {
	k, _, err := config.StoreKey(dir)
	return k, err
}
