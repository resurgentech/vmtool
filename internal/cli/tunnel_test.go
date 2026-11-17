package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
	"github.com/spf13/cobra"
)

// tunnelFakes replaces the tunnel's VM lookup, guest dial and TLS trust for
// one test, so no test reaches libvirt or a real address. Every CLI tunnel
// test calls it before running the command.
type tunnelFakes struct {
	mu      sync.Mutex
	lookups []string
	dials   []string
	echo    net.Listener
}

func fakeTunnel(t *testing.T, pool *x509.CertPool) *tunnelFakes {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(struct{ io.Writer }{c}, struct{ io.Reader }{c})
				c.Close()
			}()
		}
	}()
	f := &tunnelFakes{echo: ln}
	oldIP, oldDial, oldTLS := tunnelVMIP, tunnelDial, tunnelTLS
	t.Cleanup(func() { tunnelVMIP, tunnelDial, tunnelTLS = oldIP, oldDial, oldTLS })
	tunnelVMIP = func(_ *env, ref app.VMRef) (string, error) {
		f.mu.Lock()
		f.lookups = append(f.lookups, ref.String())
		f.mu.Unlock()
		switch ref.Name {
		case "web":
			return "192.0.2.10", nil
		case "noip":
			return "", nil
		}
		return "", fmt.Errorf("domain %q: %w", ref.Name, vmtool.ErrNotFound)
	}
	tunnelDial = func(ctx context.Context, addr string) (net.Conn, error) {
		f.mu.Lock()
		f.dials = append(f.dials, addr)
		f.mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	tunnelTLS = &tls.Config{RootCAs: pool}
	return f
}

func (f *tunnelFakes) calls() (lookups, dials []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lookups...), append([]string(nil), f.dials...)
}

// runTunnel runs `vmtool tunnel args...` with stdin; every other command is
// a tripwire.
func runTunnel(t *testing.T, dir string, stdin io.Reader, stdout io.Writer, args ...string) (errOut string, err error) {
	t.Helper()
	var errb bytes.Buffer
	root, _ := rootFor(dir, stdout, &errb)
	walk(root, func(c *cobra.Command) {
		if !c.Runnable() || c.CommandPath() == "vmtool tunnel" {
			return
		}
		c.Run = nil
		c.RunE = func(c *cobra.Command, _ []string) error {
			t.Errorf("%s ran", c.CommandPath())
			return nil
		}
	})
	root.SetIn(stdin)
	root.SetArgs(append([]string{"tunnel"}, args...))
	err = root.Execute()
	return errb.String(), err
}

// tunnelWorkdir writes config.yml naming the local host ws and, when port is
// non-zero, a remote hv1 at 127.0.0.1:port.
func tunnelWorkdir(t *testing.T, port int) string {
	t.Helper()
	t.Setenv(config.TokenEnv, gateToken)
	cfg := "name: ws\ntoken_sha256: " + config.Hash(gateToken) + "\n"
	if port != 0 {
		cfg += fmt.Sprintf("hosts:\n  hv1: { address: 127.0.0.1, port: %d }\n", port)
	}
	return writeWorkdir(t, cfg, "")
}

func TestTunnelLocal(t *testing.T) {
	f := fakeTunnel(t, nil)
	dir := tunnelWorkdir(t, 0)
	var out bytes.Buffer
	errOut, err := runTunnel(t, dir, strings.NewReader("ssh bytes"), &out, "ws/web")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "ssh bytes" || errOut != "" {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut)
	}
	lookups, dials := f.calls()
	if len(lookups) != 1 || lookups[0] != "ws/web" || len(dials) != 1 || dials[0] != "192.0.2.10:22" {
		t.Fatalf("lookups %v dials %v", lookups, dials)
	}
}

func TestTunnelLocalRefusals(t *testing.T) {
	f := fakeTunnel(t, nil)
	dir := tunnelWorkdir(t, 0)
	for _, tc := range []struct{ arg, want string }{
		{"ws/noip", "no IP"},
		{"ws/nope", "not found"},
		{"web", "host/vm"},
		{"elsewhere/web", "elsewhere"},
	} {
		var out bytes.Buffer
		_, err := runTunnel(t, dir, strings.NewReader("x"), &out, tc.arg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want containing %q", tc.arg, err, tc.want)
		}
		if out.Len() != 0 {
			t.Errorf("%s: stdout %q, want nothing", tc.arg, out.String())
		}
	}
	if _, dials := f.calls(); len(dials) != 0 {
		t.Fatalf("dialed %v", dials)
	}
}

// signalWriter records writes and signals once it holds want.
type signalWriter struct {
	mu   sync.Mutex
	b    bytes.Buffer
	want string
	got  chan struct{}
	once sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b.Write(p)
	if strings.Contains(w.b.String(), w.want) {
		w.once.Do(func() { close(w.got) })
	}
	return len(p), nil
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// fakeDaemon is a TLS vmtool daemon that accepts any tunnel request whose
// bearer is gateToken (hijack, 101, echo) and records the request path.
func fakeDaemon(t *testing.T, status int) (port int, pool *x509.CertPool, paths func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+gateToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if status != http.StatusSwitchingProtocols {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"domain \"nope\" not found"}`)
			return
		}
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: vmtool-tunnel\r\n\r\n"))
		if n := rw.Reader.Buffered(); n > 0 {
			b, _ := rw.Reader.Peek(n)
			c.Write(b)
			rw.Reader.Discard(n)
		}
		io.Copy(struct{ io.Writer }{c}, struct{ io.Reader }{c})
	}))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	fmt.Sscan(u.Port(), &port)
	pool = x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return port, pool, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestTunnelRemote(t *testing.T) {
	port, pool, paths := fakeDaemon(t, http.StatusSwitchingProtocols)
	f := fakeTunnel(t, pool)
	dir := tunnelWorkdir(t, port)
	stdinR, stdinW := io.Pipe()
	out := &signalWriter{want: "ssh bytes", got: make(chan struct{})}
	go func() {
		stdinW.Write([]byte("ssh bytes"))
		select {
		case <-out.got:
		case <-time.After(5 * time.Second):
		}
		stdinW.Close() // stdin EOF ends the tunnel
	}()
	errOut, err := runTunnel(t, dir, stdinR, out, "hv1/web")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "ssh bytes" || errOut != "" {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut)
	}
	if p := paths(); len(p) != 1 || p[0] != "/v1/hosts/hv1/vms/web/tunnel" {
		t.Fatalf("daemon saw %v", p)
	}
	if lookups, dials := f.calls(); len(lookups) != 0 || len(dials) != 0 {
		t.Fatalf("remote tunnel used the local path: lookups %v dials %v", lookups, dials)
	}
}

func TestTunnelRemoteError(t *testing.T) {
	port, pool, _ := fakeDaemon(t, http.StatusNotFound)
	fakeTunnel(t, pool)
	dir := tunnelWorkdir(t, port)
	var out bytes.Buffer
	_, err := runTunnel(t, dir, strings.NewReader("x"), &out, "hv1/nope")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err %v, want the daemon's 404", err)
	}
	if strings.Contains(err.Error(), gateToken) {
		t.Fatal("error contains the token")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout %q", out.String())
	}
}

// The ssh name and host/vm reach the same VM.
func TestTunnelSSHName(t *testing.T) {
	f := fakeTunnel(t, nil)
	dir := tunnelWorkdir(t, 0)
	for _, arg := range []string{"web.ws.vmtool", "ws/web"} {
		var out bytes.Buffer
		if _, err := runTunnel(t, dir, strings.NewReader("x"), &out, arg); err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
	}
	lookups, dials := f.calls()
	if len(lookups) != 2 || lookups[0] != "ws/web" || lookups[1] != "ws/web" || len(dials) != 2 || dials[0] != dials[1] {
		t.Fatalf("lookups %v dials %v", lookups, dials)
	}
}

func TestTunnelSSHNameRefusals(t *testing.T) {
	f := fakeTunnel(t, nil)
	dir := tunnelWorkdir(t, 0)
	for _, arg := range []string{"jetfire.vmtool", "x y.ws.vmtool", ".ws.vmtool", "web.vmtool"} {
		_, _, want := vmtool.ParseSSHName(arg)
		if want == nil {
			t.Fatalf("%q: ParseSSHName accepted", arg)
		}
		var out bytes.Buffer
		_, err := runTunnel(t, dir, strings.NewReader("x"), &out, arg)
		if err == nil || !strings.Contains(err.Error(), want.Error()) {
			t.Errorf("%s: err %v, want containing %q", arg, err, want)
		}
	}
	if lookups, dials := f.calls(); len(lookups)+len(dials) != 0 {
		t.Fatalf("lookups %v dials %v", lookups, dials)
	}
}
