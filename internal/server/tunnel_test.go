package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/remote"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

const want101 = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: vmtool-tunnel\r\n\r\n"

// echoServer stands in for a guest's sshd: it echoes every byte back and
// reports each connection it finishes.
type echoServer struct {
	ln       net.Listener
	accepted atomic.Int32
	closed   chan struct{}
}

func newEcho(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &echoServer{ln: ln, closed: make(chan struct{}, 16)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			e.accepted.Add(1)
			go func() {
				buf := make([]byte, 32<<10)
				io.CopyBuffer(struct{ io.Writer }{c}, struct{ io.Reader }{c}, buf)
				c.Close()
				e.closed <- struct{}{}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return e
}

// tunnelFixture is an in-process TLS server running the real chain (log,
// bearer auth, body limit, mux) with a fake VM lookup and a dial that lands
// on the echo server whatever address it is asked for (and records it).
type tunnelFixture struct {
	srv     *httptest.Server
	echo    *echoServer
	lookups atomic.Int32
	mu      sync.Mutex
	dials   []string
	dialErr error
	// guest, when set, replaces the echo server as the dialled connection.
	guest func() net.Conn
}

func newTunnelFixture(t *testing.T, http2 bool) *tunnelFixture {
	t.Helper()
	f := &tunnelFixture{echo: newEcho(t)}
	h := &handlers{
		hosts: noLibvirt(t),
		vmIP: func(_ context.Context, host, name string) (string, error) {
			f.lookups.Add(1)
			if host != "box" {
				return "", fmt.Errorf("host %q: %w", host, vmtool.ErrNotFound)
			}
			switch name {
			case "web":
				return "192.0.2.10", nil
			case "noip":
				return "", nil
			case "badip":
				return "192.0.2.10:2222", nil
			}
			return "", fmt.Errorf("domain %q: %w", name, vmtool.ErrNotFound)
		},
		dialGuest: func(ctx context.Context, addr string) (net.Conn, error) {
			f.mu.Lock()
			f.dials = append(f.dials, addr)
			err, guest := f.dialErr, f.guest
			f.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if guest != nil {
				return guest(), nil
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", f.echo.ln.Addr().String())
		},
	}
	hash := config.Hash(testToken)
	f.srv = httptest.NewUnstartedServer(chain(h, func(next http.Handler) http.Handler {
		return requireBearer(hash, publicRoutes(), next)
	}))
	f.srv.EnableHTTP2 = http2
	if !http2 {
		// Short server timeouts: a tunnel must outlive them.
		f.srv.Config.ReadTimeout = 300 * time.Millisecond
		f.srv.Config.WriteTimeout = 300 * time.Millisecond
	}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *tunnelFixture) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(f.srv.Certificate())
	return p
}

func (f *tunnelFixture) dialed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dials...)
}

// raw opens an HTTP/1.1 TLS connection to the server.
func (f *tunnelFixture) raw(t *testing.T) *tls.Conn {
	t.Helper()
	c, err := tls.Dial("tcp", f.srv.Listener.Addr().String(), &tls.Config{RootCAs: f.pool(), NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

func upgradeReq(path, token string, extra ...string) string {
	var b strings.Builder
	b.WriteString("GET " + path + " HTTP/1.1\r\nHost: vmtool\r\n")
	if token != "" {
		b.WriteString("Authorization: Bearer " + token + "\r\n")
	}
	if extra == nil {
		extra = []string{"Connection: Upgrade", "Upgrade: vmtool-tunnel"}
	}
	for _, h := range extra {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

// open sends the upgrade request plus early bytes in one write and checks
// the exact 101 response.
func (f *tunnelFixture) open(t *testing.T, early string) *tls.Conn {
	t.Helper()
	c := f.raw(t)
	if _, err := c.Write([]byte(upgradeReq("/v1/hosts/box/vms/web/tunnel", testToken) + early)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want101))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read 101: %v (got %q)", err, got)
	}
	if string(got) != want101 {
		t.Fatalf("response head %q, want %q", got, want101)
	}
	return c
}

func expectRead(t *testing.T, r io.Reader, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read %q: %v (got %q)", want, err, got)
	}
	if string(got) != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuf is a bytes.Buffer safe to read while slog writes it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureSlogSync(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestTunnelRoundTrip(t *testing.T) {
	f := newTunnelFixture(t, false)
	c := f.open(t, "early-bytes")
	// Bytes sent in the same write as the request headers reach the guest.
	expectRead(t, c, "early-bytes")
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, c, "ping")
	if d := f.dialed(); len(d) != 1 || d[0] != "192.0.2.10:22" {
		t.Fatalf("dialed %v, want exactly [192.0.2.10:22]", d)
	}
}

// The server's ReadTimeout/WriteTimeout must not end a long session.
func TestTunnelOutlivesServerTimeouts(t *testing.T) {
	f := newTunnelFixture(t, false)
	c := f.open(t, "")
	time.Sleep(700 * time.Millisecond)
	if _, err := c.Write([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, c, "still here")
}

// The 1 MiB request body limit does not apply to the tunnel stream.
func TestTunnelMoreThanOneMiB(t *testing.T) {
	f := newTunnelFixture(t, false)
	c := f.open(t, "")
	payload := make([]byte, 3<<20)
	rand.Read(payload)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Write(payload)
		errc <- err
	}()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload corrupted")
	}
}

// Closing the client ends both copies: the guest connection is closed and
// the handler returns (the request log records 101). No token is logged.
func TestTunnelClientCloseTearsDown(t *testing.T) {
	buf := captureSlogSync(t)
	f := newTunnelFixture(t, false)
	c := f.open(t, "x")
	expectRead(t, c, "x")
	c.Close()
	select {
	case <-f.echo.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("guest connection not closed after the client closed")
	}
	waitFor(t, "request log with status=101", func() bool { return strings.Contains(buf.String(), "status=101") })
	out := buf.String()
	for _, s := range []string{testToken, "Authorization", "Bearer"} {
		if strings.Contains(out, s) {
			t.Errorf("log contains %q:\n%s", s, out)
		}
	}
}

// The guest closing ends the client's stream.
func TestTunnelGuestCloseTearsDown(t *testing.T) {
	buf := captureSlogSync(t)
	f := newTunnelFixture(t, false)
	srvSide, guestSide := net.Pipe()
	f.guest = func() net.Conn { return srvSide }
	c := f.open(t, "")
	go func() {
		b := make([]byte, 5)
		io.ReadFull(guestSide, b)
		guestSide.Write([]byte("bye"))
		guestSide.Close()
	}()
	c.Write([]byte("hello"))
	expectRead(t, c, "bye")
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("client read after guest close: want EOF/error")
	}
	waitFor(t, "request log with status=101", func() bool { return strings.Contains(buf.String(), "status=101") })
}

func readResp(t *testing.T, c net.Conn) (*http.Response, string) {
	t.Helper()
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	res.Body.Close()
	return res, string(b)
}

func checkJSONErr(t *testing.T, label string, res *http.Response, body string, code int) {
	t.Helper()
	if res.StatusCode != code {
		t.Errorf("%s: status %d, want %d (%s)", label, res.StatusCode, code, body)
		return
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s: content-type %q", label, ct)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error == "" {
		t.Errorf("%s: body %q is not the Error shape (%v)", label, body, err)
	}
}

// No token, or a wrong one: 401, and nothing behind auth ran.
func TestTunnelNeedsToken(t *testing.T) {
	f := newTunnelFixture(t, false)
	for _, tok := range []string{"", wrongToken} {
		c := f.raw(t)
		c.Write([]byte(upgradeReq("/v1/hosts/box/vms/web/tunnel", tok) + "early"))
		res, body := readResp(t, c)
		checkJSONErr(t, "token "+tok, res, body, http.StatusUnauthorized)
		if strings.Contains(body, wrongToken) {
			t.Error("401 echoes the token")
		}
	}
	if n := f.lookups.Load(); n != 0 {
		t.Errorf("VM lookup ran %d times without a valid token", n)
	}
	if d := f.dialed(); len(d) != 0 {
		t.Errorf("dialed %v without a valid token", d)
	}
	if n := f.echo.accepted.Load(); n != 0 {
		t.Errorf("guest saw %d connections without a valid token", n)
	}
}

func TestTunnelRefusals(t *testing.T) {
	f := newTunnelFixture(t, false)
	for _, tc := range []struct {
		name, path string
		headers    []string
		body       string
		code       int
		lookup     bool // whether the VM lookup may run
	}{
		{"no upgrade header", "/v1/hosts/box/vms/web/tunnel", []string{"Connection: Upgrade"}, "", 400, false},
		{"no connection header", "/v1/hosts/box/vms/web/tunnel", []string{"Upgrade: vmtool-tunnel"}, "", 400, false},
		{"websocket", "/v1/hosts/box/vms/web/tunnel", []string{"Connection: Upgrade", "Upgrade: websocket"}, "", 400, false},
		{"connection close", "/v1/hosts/box/vms/web/tunnel", []string{"Connection: close", "Upgrade: vmtool-tunnel"}, "", 400, false},
		{"request body", "/v1/hosts/box/vms/web/tunnel", []string{"Connection: Upgrade", "Upgrade: vmtool-tunnel", "Content-Length: 5"}, "hello", 400, false},
		{"bad name", "/v1/hosts/box/vms/bad%20name/tunnel", nil, "", 400, false},
		{"slash in name", "/v1/hosts/box/vms/a%2Fb/tunnel", nil, "", 400, false},
		{"unknown vm", "/v1/hosts/box/vms/nope/tunnel", nil, "", 404, true},
		{"unknown host", "/v1/hosts/other/vms/web/tunnel", nil, "", 404, true},
		{"no ip", "/v1/hosts/box/vms/noip/tunnel", nil, "", 409, true},
		{"not an ip", "/v1/hosts/box/vms/badip/tunnel", nil, "", 409, true},
	} {
		before := f.lookups.Load()
		c := f.raw(t)
		c.Write([]byte(upgradeReq(tc.path, testToken, tc.headers...) + tc.body))
		res, body := readResp(t, c)
		checkJSONErr(t, tc.name, res, body, tc.code)
		if ran := f.lookups.Load() != before; ran != tc.lookup {
			t.Errorf("%s: lookup ran = %v, want %v", tc.name, ran, tc.lookup)
		}
	}
	if d := f.dialed(); len(d) != 0 {
		t.Errorf("dialed %v for refused requests", d)
	}
	if n := f.echo.accepted.Load(); n != 0 {
		t.Errorf("guest saw %d connections", n)
	}
}

// A failed dial is an ordinary JSON 502, sent before any upgrade.
func TestTunnelDialFailure502(t *testing.T) {
	f := newTunnelFixture(t, false)
	f.dialErr = errors.New("connection refused")
	c := f.raw(t)
	c.Write([]byte(upgradeReq("/v1/hosts/box/vms/web/tunnel", testToken)))
	res, body := readResp(t, c)
	checkJSONErr(t, "dial failure", res, body, http.StatusBadGateway)
	if d := f.dialed(); len(d) != 1 || d[0] != "192.0.2.10:22" {
		t.Fatalf("dialed %v", d)
	}
}

// The default lookup goes through the host resolver: an unknown host is 404
// without touching libvirt.
func TestTunnelUnknownHostViaResolver(t *testing.T) {
	h := handlerFor(&handlers{hosts: noLibvirt(t, "remote1"), dialGuest: func(context.Context, string) (net.Conn, error) {
		t.Error("dial for an unknown host")
		return nil, errors.New("no")
	}})
	for _, p := range []string{"/v1/hosts/nope/vms/web/tunnel", "/v1/hosts/remote1/vms/web/tunnel"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "vmtool-tunnel")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		res := rec.Result()
		checkJSONErr(t, p, res, rec.Body.String(), http.StatusNotFound)
		if !strings.Contains(rec.Body.String(), "box") {
			t.Errorf("%s: body %q does not name the served host", p, rec.Body)
		}
	}
}

// HTTP/2 cannot hijack: 426 with Upgrade: vmtool-tunnel.
func TestTunnelHTTP2Gets426(t *testing.T) {
	f := newTunnelFixture(t, true)
	client := f.srv.Client()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/hosts/box/vms/web/tunnel", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("test client spoke HTTP/%d, want 2", res.ProtoMajor)
	}
	checkJSONErr(t, "h2", res, string(b), http.StatusUpgradeRequired)
	if n := f.lookups.Load(); n != 0 {
		t.Errorf("lookup ran %d times for an HTTP/2 request", n)
	}

	// Go's HTTP/2 client deletes connection-specific headers (Upgrade among
	// them, RFC 9113 section 8.2.2) from responses, so the header the
	// handler sets is checked on the handler's own output.
	h := handlerFor(&handlers{hosts: noLibvirt(t)})
	r := httptest.NewRequest(http.MethodGet, "/v1/hosts/box/vms/web/tunnel", nil)
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	checkJSONErr(t, "h2 recorder", rec.Result(), rec.Body.String(), http.StatusUpgradeRequired)
	if got := rec.Header().Get("Upgrade"); got != "vmtool-tunnel" {
		t.Errorf("Upgrade header %q, want vmtool-tunnel", got)
	}
}

// End to end with the real client.
func TestTunnelRemoteDial(t *testing.T) {
	f := newTunnelFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := remote.Dial(ctx, f.srv.URL, "box", "web", testToken, &tls.Config{RootCAs: f.pool()})
	cancel() // the stream must outlive the dial context
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("over remote.Dial"))
	expectRead(t, conn, "over remote.Dial")

	_, err = remote.Dial(context.Background(), f.srv.URL, "box", "nope", testToken, &tls.Config{RootCAs: f.pool()})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("unknown VM: err %v, want a 404", err)
	}
}

// The production server speaks HTTP/1.1 to a client that offers h2 too, so
// the tunnel can always upgrade.
func TestServeNegotiatesHTTP1(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	_, _, pool := writeCert(t, dir)
	s := &Server{Listen: "127.0.0.1:0", PlaybookDir: "playbooks", Config: &config.Config{
		Dir: dir, TokenSHA256: config.Hash(testToken), TLSCert: "tls/test.crt", TLSKey: "tls/test.key",
	}}
	srv, err := s.httpServer()
	if err != nil {
		t.Fatal(err)
	}
	if np := srv.TLSConfig.NextProtos; len(np) != 1 || np[0] != "http/1.1" {
		t.Errorf("TLSConfig.NextProtos = %v, want [http/1.1]", np)
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go serve(srv, ln)
	t.Cleanup(func() { srv.Close() })
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{RootCAs: pool, NextProtos: []string{"h2", "http/1.1"}},
	}}
	res, err := client.Get("https://" + ln.Addr().String() + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.ProtoMajor != 1 || res.TLS == nil || res.TLS.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("proto %s, ALPN %q; want HTTP/1.1 negotiated", res.Proto, res.TLS.NegotiatedProtocol)
	}
}
