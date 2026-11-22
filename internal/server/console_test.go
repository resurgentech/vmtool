package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// consoleMgr is a fakeMgr with three VMs: web (has an address), noip (none),
// and everything else unknown.
type consoleMgr struct{ *fakeMgr }

func (consoleMgr) Info(n string) (*vmtool.VMInfo, error) {
	switch n {
	case "web":
		return &vmtool.VMInfo{Name: n, IP: "10.0.0.5"}, nil
	case "noip":
		return &vmtool.VMInfo{Name: n}, nil
	}
	return nil, fmt.Errorf("domain %q: %w", n, vmtool.ErrNotFound)
}

// consoleFix is an in-process TLS server running the real chain (log, bearer
// auth, gate, mux) with a stub ssh. The stub writes "started" to a marker
// file first, records its argv and $TERM, then runs the test's body.
type consoleFix struct {
	t   *testing.T
	srv *httptest.Server
	h   *handlers
	dir string
	log *bytes.Buffer
}

func (f *consoleFix) file(name string) string { return filepath.Join(f.dir, name) }

// newConsoleFix starts the server. body is the stub ssh's shell script after
// the preamble; mutate adjusts the handlers before the chain is built.
func newConsoleFix(t *testing.T, body string, mutate ...func(*handlers)) *consoleFix {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	f := &consoleFix{t: t, dir: t.TempDir(), log: captureLog(t)}
	script := "#!/bin/sh\n" +
		"echo started >> '" + f.file("marker") + "'\n" +
		"printf '%s\\n' \"$@\" > '" + f.file("argv") + "'\n" +
		"echo \"$TERM\" > '" + f.file("term") + "'\n" +
		"echo $$ > '" + f.file("pid") + "'\n" +
		strings.ReplaceAll(body, "@DIR@", f.dir) + "\n"
	testexec.Stub(t, map[string]string{"ssh": script, "sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n"})
	res := hosts.NewWith("box", config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
		return consoleMgr{&fakeMgr{}}, func() {}, nil
	})
	f.h = &handlers{hosts: res, role: config.RoleWorkstation, svc: &app.Service{}, tunnelExe: stubTunnelExe}
	for _, m := range mutate {
		m(f.h)
	}
	hash := config.Hash(testToken)
	f.srv = httptest.NewUnstartedServer(chain(f.h, func(next http.Handler) http.Handler {
		return requireBearer(hash, publicRoutes(), next)
	}))
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	// Safety net: whatever a failing test leaves behind dies with it.
	t.Cleanup(f.killRecorded)
	return f
}

func (f *consoleFix) auth() http.Header {
	return http.Header{"Authorization": {"Bearer " + testToken}}
}

func (f *consoleFix) url(path string) string {
	return "wss://" + strings.TrimPrefix(f.srv.URL, "https://") + path
}

// dial runs the WebSocket handshake with the good token plus extra headers.
func (f *consoleFix) dial(path string, extra http.Header) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()
	hdr := f.auth()
	for k, v := range extra {
		hdr[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return websocket.Dial(ctx, f.url(path), &websocket.DialOptions{HTTPClient: f.srv.Client(), HTTPHeader: hdr})
}

func (f *consoleFix) mustDial(path string) *websocket.Conn {
	f.t.Helper()
	c, resp, err := f.dial(path, nil)
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		f.t.Fatalf("dial %s: status %d: %v", path, st, err)
	}
	f.t.Cleanup(func() { c.CloseNow() })
	return c
}

// refused runs the handshake expecting a refusal: it returns the status and
// the JSON error text.
func (f *consoleFix) refused(path string, extra http.Header) (int, string) {
	f.t.Helper()
	c, resp, err := f.dial(path, extra)
	if err == nil {
		c.CloseNow()
		f.t.Fatalf("%s: handshake succeeded, want a refusal", path)
	}
	if resp == nil {
		f.t.Fatalf("%s: no response: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var e struct{ Error string }
	_ = json.Unmarshal(b, &e)
	if e.Error == "" {
		f.t.Fatalf("%s: status %d body %q is not a JSON Error", path, resp.StatusCode, b)
	}
	return resp.StatusCode, e.Error
}

// reset forgets the pids a previous console recorded.
func (f *consoleFix) reset() {
	_ = os.Remove(f.file("pid"))
	_ = os.Remove(f.file("pid2"))
}

func (f *consoleFix) started() int {
	b, _ := os.ReadFile(f.file("marker"))
	return strings.Count(string(b), "started")
}

// pids are the numbers the stub recorded in the named files.
func (f *consoleFix) readPid(name string) int {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(f.file(name)); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("stub never recorded %s", name)
	return 0
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "pid (comm) S ...": the state follows the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

// assertGone fails unless every pid is gone (or a zombie) within 5 s.
func (f *consoleFix) assertGone(pids ...int) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var left []int
		for _, p := range pids {
			if alive(p) {
				left = append(left, p)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("processes still alive after 5s: %v", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killRecorded kills any pid the stub recorded that is still alive.
func (f *consoleFix) killRecorded() {
	for _, name := range []string{"pid", "pid2"} {
		if b, err := os.ReadFile(f.file(name)); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 1 && alive(n) {
				_ = syscall.Kill(n, syscall.SIGKILL)
			}
		}
	}
}

const (
	vmConsole    = "/v1/hosts/box/vms/web/console"
	metalConsole = "/v1/metal/nuc1/console"
	// holdBody keeps the stub alive, reading its terminal, until killed.
	holdBody = "sleep 30 &\necho $! > '@DIR@/pid2'\nwait"
)

// upgradeHeaders make a request a WebSocket handshake for plain http calls.
func upgradeHeaders(h http.Header) {
	h.Set("Connection", "Upgrade")
	h.Set("Upgrade", "websocket")
	h.Set("Sec-WebSocket-Version", "13")
	h.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
}

// Test 1: no token and a wrong token are 401, and nothing starts.
func TestConsoleNeedsToken(t *testing.T) {
	f := newConsoleFix(t, holdBody)
	for _, p := range []string{vmConsole, metalConsole} {
		for name, auth := range map[string]string{"none": "", "wrong": "Bearer PLACEHOLDER-wrong-token"} {
			req, _ := http.NewRequest("GET", f.srv.URL+p, nil)
			upgradeHeaders(req.Header)
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := f.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("%s %s: status %d, want 401", p, name, resp.StatusCode)
			}
		}
	}
	if n := f.started(); n != 0 {
		t.Fatalf("ssh started %d times without a token", n)
	}
}

// Test 2: any Origin header is refused, even with a good token.
func TestConsoleRefusesOrigin(t *testing.T) {
	f := newConsoleFix(t, holdBody)
	for _, p := range []string{vmConsole, metalConsole} {
		for _, origin := range []string{"https://example.invalid", "null"} {
			code, msg := f.refused(p, http.Header{"Origin": {origin}})
			if code != 403 || !strings.Contains(msg, "browser access is not allowed") {
				t.Fatalf("%s origin %q: %d %q", p, origin, code, msg)
			}
		}
	}
	if n := f.started(); n != 0 {
		t.Fatalf("ssh started %d times for an Origin handshake", n)
	}
}

// Test 3: a hypervisor refuses both paths before any work; a GET with Upgrade
// to an exec path opens no console.
func TestConsoleWorkstationOnly(t *testing.T) {
	hv := func(h *handlers) {
		h.role = config.RoleHypervisor
		h.hosts = noLibvirt(t)
	}
	f := newConsoleFix(t, holdBody, hv)
	for _, p := range []string{vmConsole, metalConsole} {
		code, msg := f.refused(p, nil)
		if code != 403 || !strings.Contains(msg, "served by the workstation") {
			t.Fatalf("%s: %d %q", p, code, msg)
		}
	}
	if n := f.started(); n != 0 {
		t.Fatalf("hypervisor started ssh %d times", n)
	}

	w := newConsoleFix(t, holdBody)
	for _, p := range []string{"/v1/hosts/box/vms/web/exec", "/v1/metal/nuc1/exec"} {
		req, _ := http.NewRequest("GET", w.srv.URL+p, nil)
		upgradeHeaders(req.Header)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := w.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 101 || resp.StatusCode < 400 {
			t.Fatalf("GET %s with Upgrade: status %d, want a refusal", p, resp.StatusCode)
		}
	}
	if n := w.started(); n != 0 {
		t.Fatalf("an exec path opened a console (%d starts)", n)
	}
}

// Test 4: bad cols/rows 400, unknown host/VM/machine 404, no IP 409, not an
// upgrade 400, HTTP/2 426; the check order; none starts ssh.
func TestConsoleRefusals(t *testing.T) {
	f := newConsoleFix(t, holdBody)
	for _, tc := range []struct {
		path string
		want int
	}{
		{vmConsole + "?cols=0", 400},
		{vmConsole + "?cols=65536", 400},
		{vmConsole + "?cols=abc", 400},
		{vmConsole + "?cols=1.5", 400},
		{vmConsole + "?rows=-1", 400},
		{vmConsole + "?rows=", 400},
		{metalConsole + "?rows=0", 400},
		{"/v1/hosts/box/vms/bad%20name/console", 400},
		{"/v1/metal/bad%20name/console", 400},
		{"/v1/hosts/box/vms/nope/console", 404},
		{"/v1/hosts/nohost/vms/web/console", 404},
		{"/v1/metal/nope/console", 404},
		{"/v1/hosts/box/vms/noip/console", 409},
	} {
		code, msg := f.refused(tc.path, nil)
		if code != tc.want {
			t.Errorf("%s: %d %q, want %d", tc.path, code, msg, tc.want)
		}
	}
	// Largest and smallest valid sizes are not refused for their value.
	for _, q := range []string{"?cols=1&rows=1", "?cols=65535&rows=65535"} {
		f.reset()
		c := f.mustDial(vmConsole + q)
		pid, pid2 := f.readPid("pid"), f.readPid("pid2")
		c.CloseNow()
		f.assertGone(pid, pid2)
	}

	// Not an upgrade: 400 JSON (before the target is looked up).
	req, _ := http.NewRequest("GET", f.srv.URL+"/v1/hosts/box/vms/nope/console", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), `"error"`) {
		t.Fatalf("not an upgrade: %d %s", resp.StatusCode, b)
	}
	// Order: Origin (3) before bad cols (6); not-an-upgrade (5) before bad cols (6).
	if code, _ := f.refused(vmConsole+"?cols=0", http.Header{"Origin": {"https://example.invalid"}}); code != 403 {
		t.Errorf("origin + bad cols: %d, want 403", code)
	}
	req, _ = http.NewRequest("GET", f.srv.URL+vmConsole+"?cols=0", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://example.invalid")
	resp, err = f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("origin + no upgrade: %d, want 403", resp.StatusCode)
	}
}

// Test 4 (HTTP/2): an HTTP/2 request cannot upgrade: 426.
func TestConsoleHTTP2Is426(t *testing.T) {
	f := newConsoleFix(t, holdBody)
	srv := httptest.NewUnstartedServer(f.srv.Config.Handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	client := srv.Client()
	client.Transport.(*http.Transport).TLSClientConfig.MinVersion = tls.VersionTLS12
	req, _ := http.NewRequest("GET", srv.URL+vmConsole, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Skipf("test server did not speak HTTP/2 (got HTTP/%d)", resp.ProtoMajor)
	}
	if resp.StatusCode != 426 {
		t.Fatalf("HTTP/2: status %d, want 426", resp.StatusCode)
	}
	if n := f.started(); n != 0 {
		t.Fatalf("ssh started %d times over HTTP/2", n)
	}
}

// Test 5: ssh gets -tt just before the destination, no remote command, and
// TERM=xterm-256color; the machine path too.
func TestConsoleArgvAndTerm(t *testing.T) {
	for _, tc := range []struct{ path, dest string }{
		{vmConsole, "box/web"},
		{metalConsole, ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			f := newConsoleFix(t, holdBody)
			c := f.mustDial(tc.path)
			pid, pid2 := f.readPid("pid"), f.readPid("pid2")
			argv := strings.Split(strings.TrimSpace(f.readFile("argv")), "\n")
			n := len(argv)
			if n < 2 || argv[n-2] != "-tt" {
				t.Fatalf("argv %q: want -tt just before the destination and no remote command", argv)
			}
			if tc.dest != "" && argv[n-1] != tc.dest {
				t.Fatalf("destination %q, want %q", argv[n-1], tc.dest)
			}
			if got := strings.TrimSpace(f.readFile("term")); got != "xterm-256color" {
				t.Fatalf("TERM %q", got)
			}
			c.Close(websocket.StatusNormalClosure, "")
			f.assertGone(pid, pid2)
		})
	}
}

func (f *consoleFix) readFile(name string) string {
	f.t.Helper()
	f.readPid("pid") // the stub has run its preamble
	b, err := os.ReadFile(f.file(name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// Test 12: with the cap at 1, a second handshake is 429 and ssh started once;
// closing the first frees the slot.
func TestConsoleCap(t *testing.T) {
	f := newConsoleFix(t, holdBody, func(h *handlers) { h.console.max = 1 })
	c := f.mustDial(vmConsole)
	pid, pid2 := f.readPid("pid"), f.readPid("pid2")
	code, msg := f.refused(vmConsole, nil)
	if code != 429 {
		t.Fatalf("second console: %d %q, want 429", code, msg)
	}
	if n := f.started(); n != 1 {
		t.Fatalf("ssh started %d times, want 1", n)
	}
	c.Close(websocket.StatusNormalClosure, "")
	f.assertGone(pid, pid2)
	// The slot is free again once the first console has ended.
	f.reset()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c2, _, err := f.dial(vmConsole, nil)
		if err == nil {
			p, p2 := f.readPid("pid"), f.readPid("pid2")
			c2.CloseNow()
			f.assertGone(p, p2)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed after the first console closed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
