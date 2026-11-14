package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test token only; never a real token.
const testToken = "test-token-remote-4e2a"

// seen is what the fake daemon observed of one request.
type seen struct {
	path, rawPath, auth, conn, upgrade string
	protoMajor                         int
}

// fakeDaemon answers the tunnel contract: on a good request it hijacks,
// writes the 101 and echoes; otherwise it sends status with a JSON error.
func fakeDaemon(t *testing.T, http2 bool, status int, errBody string) (*httptest.Server, *x509.CertPool, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var log []seen
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, seen{r.URL.Path, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Connection"), r.Header.Get("Upgrade"), r.ProtoMajor})
		mu.Unlock()
		if status != http.StatusSwitchingProtocols || r.ProtoMajor != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, errBody)
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
		buf := make([]byte, 32<<10)
		io.CopyBuffer(struct{ io.Writer }{c}, struct{ io.Reader }{c}, buf)
	}))
	srv.EnableHTTP2 = http2
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), log...)
	}
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

func TestDialRoundTrip(t *testing.T) {
	// The daemon offers h2; the client must still speak HTTP/1.1.
	srv, pool, log := fakeDaemon(t, true, http.StatusSwitchingProtocols, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := Dial(ctx, srv.URL, "box", "we b", testToken, &tls.Config{RootCAs: pool})
	cancel() // the stream outlives the dial context
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, conn, "hello")

	l := log()
	if len(l) != 1 {
		t.Fatalf("requests %v", l)
	}
	s := l[0]
	if s.protoMajor != 1 {
		t.Errorf("client spoke HTTP/%d", s.protoMajor)
	}
	if s.path != "/v1/hosts/box/vms/we b/tunnel" || s.rawPath != "/v1/hosts/box/vms/we%20b/tunnel" {
		t.Errorf("path %q raw %q", s.path, s.rawPath)
	}
	if s.auth != "Bearer "+testToken || s.conn != "Upgrade" || s.upgrade != "vmtool-tunnel" {
		t.Errorf("headers auth-ok=%v connection %q upgrade %q", s.auth == "Bearer "+testToken, s.conn, s.upgrade)
	}
}

func TestDialErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   []string
	}{
		{404, `{"error":"domain \"nope\" not found"}`, []string{"404", `domain "nope" not found`}},
		{401, `{"error":"unauthorized: missing or invalid bearer token"}`, []string{"401", "unauthorized"}},
		{502, `{"error":"dial guest: refused"}`, []string{"502", "dial guest"}},
		{200, `{}`, []string{"200"}},
		{500, strings.Repeat("x", 1<<20), []string{"500"}},
	} {
		srv, pool, _ := fakeDaemon(t, false, tc.status, tc.body)
		conn, err := Dial(context.Background(), srv.URL, "box", "nope", testToken, &tls.Config{RootCAs: pool})
		if err == nil {
			conn.Close()
			t.Errorf("status %d: no error", tc.status)
			continue
		}
		msg := err.Error()
		for _, w := range tc.want {
			if !strings.Contains(msg, w) {
				t.Errorf("status %d: error %q lacks %q", tc.status, msg, w)
			}
		}
		if strings.Contains(msg, testToken) {
			t.Errorf("status %d: error contains the token", tc.status)
		}
		if len(msg) > 2048 {
			t.Errorf("status %d: error is %d bytes; want it bounded", tc.status, len(msg))
		}
	}
}

// The system trust store is used when no RootCAs are given: a self-signed
// test server is refused.
func TestDialVerifiesCert(t *testing.T) {
	srv, _, log := fakeDaemon(t, false, http.StatusSwitchingProtocols, "")
	if conn, err := Dial(context.Background(), srv.URL, "box", "web", testToken, nil); err == nil {
		conn.Close()
		t.Fatal("dial succeeded against an untrusted certificate")
	}
	if len(log()) != 0 {
		t.Fatal("request (and token) sent over an unverified connection")
	}
}

// halfConn is one end of an in-memory connection, with CloseWrite.
type halfConn struct {
	net.Conn
	closedWrite chan struct{}
}

func (h *halfConn) CloseWrite() error {
	close(h.closedWrite)
	return nil
}

// Pipe copies stdin to the connection and the connection to stdout, ends
// the write side on stdin EOF and returns when the remote side closes.
func TestPipe(t *testing.T) {
	local, far := net.Pipe()
	hc := &halfConn{Conn: local, closedWrite: make(chan struct{})}
	go func() {
		b := make([]byte, 5)
		io.ReadFull(far, b)
		far.Write(append([]byte("echo:"), b...))
		<-hc.closedWrite // stdin EOF reached the far side as a half-close
		far.Write([]byte(":tail"))
		far.Close()
	}()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Pipe(context.Background(), hc, strings.NewReader("hello"), &out) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after the remote side closed")
	}
	if out.String() != "echo:hello:tail" {
		t.Fatalf("stdout %q", out.String())
	}
}

// Without CloseWrite, stdin EOF closes the connection and Pipe returns.
func TestPipeStdinEOFCloses(t *testing.T) {
	local, far := net.Pipe()
	farDone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, far) // returns once local is closed
		close(farDone)
	}()
	done := make(chan error, 1)
	go func() { done <- Pipe(context.Background(), local, strings.NewReader("bye"), io.Discard) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after stdin EOF")
	}
	select {
	case <-farDone:
	case <-time.After(5 * time.Second):
		t.Fatal("connection not closed after stdin EOF")
	}
}

// Cancelling the context closes the connection and Pipe returns, even with
// stdin still open.
func TestPipeContextCancel(t *testing.T) {
	local, far := net.Pipe()
	defer far.Close()
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Pipe(ctx, local, stdinR, io.Discard) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after cancel")
	}
}
