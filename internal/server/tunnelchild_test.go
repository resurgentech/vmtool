package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

var (
	childBinOnce sync.Once
	childBin     string
	childBinErr  error
)

// TestMain removes the built child binary's directory after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if childBin != "" {
		os.RemoveAll(filepath.Dir(childBin))
	}
	os.Exit(code)
}

// builtVMTool builds the real vmtool binary once per test run. It is the
// ProxyCommand child of the daemon, so it is not stubbed.
func builtVMTool(t *testing.T) string {
	t.Helper()
	childBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vmtool-child-bin")
		if err != nil {
			childBinErr = err
			return
		}
		childBin = filepath.Join(dir, "vmtool")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "go", "build", "-o", childBin, "github.com/resurgentech/vmtool/cmd/vmtool").CombinedOutput(); err != nil {
			childBinErr = &buildError{err, string(out)}
		}
	})
	if childBinErr != nil {
		t.Fatalf("go build vmtool: %v", childBinErr)
	}
	return childBin
}

type buildError struct {
	err error
	out string
}

func (b *buildError) Error() string { return b.err.Error() + "\n" + b.out }

// recorder remembers the authenticated requests a daemon answered.
type recorder struct {
	mu   sync.Mutex
	rows []string // "METHOD path status"
}

func (r *recorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sw := &childStatusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, req)
		r.mu.Lock()
		r.rows = append(r.rows, req.Method+" "+req.URL.Path+" "+http.StatusText(sw.code))
		r.mu.Unlock()
	})
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.rows...)
}

type childStatusWriter struct {
	http.ResponseWriter
	code int
}

func (s *childStatusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

// runTunnelChild runs the built binary as ssh would run the daemon's
// ProxyCommand for a request carrying bearer: the argv from vmtool.Tunnel, its
// environment (VMTOOL_TOKEN = bearer), the service's working directory, no
// token file. The host is not in config.yml, so a run that passes the gate
// ends with an unknown-host error, before libvirt or ssh.
func runTunnelChild(t *testing.T, bearer string) (stdout, stderr string) {
	t.Helper()
	bin := builtVMTool(t)
	l := keyclienttest.NewList(t)
	l.Add("alice", "full", listedKey)
	s := listServer(t, config.RoleHypervisor, l)
	h := s.Handler()
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)

	work := t.TempDir()
	certPath, keyPath, _ := writeCert(t, work)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	ts := httptest.NewUnstartedServer(rec.wrap(h))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	cfg := "name: hv1\nrole: hypervisor\nlisten: " + strings.TrimPrefix(ts.URL, "https://") +
		"\ntls_cert: " + certPath + "\ntoken_sha256: " + config.Hash(testToken) + "\n"
	if err := os.WriteFile(filepath.Join(work, config.FileName), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	tun := vmtool.Tunnel{Exe: bin, Token: bearer}
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + empty, "XDG_CONFIG_HOME=" + empty}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "tunnel", "hv9/vm1")
	if !strings.Contains(tun.ProxyCommand("hv9", "vm1"), " tunnel hv9/vm1") {
		t.Fatal("ProxyCommand changed")
	}
	cmd.Dir = work
	cmd.Env = tun.Environ(base)
	cmd.Stdin = strings.NewReader("")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	if err == nil {
		t.Fatalf("child exited 0 for an unknown host")
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon saw %v", rec.all())
		}
	})
	got := rec.all()
	if bearer == listedKey {
		saw := false
		for _, r := range got {
			if r == "GET /v1/personal-keys OK" {
				saw = true
			}
		}
		if !saw {
			t.Errorf("the daemon did not accept a GET /v1/personal-keys with the pulled key: %v", got)
		}
	}
	for _, out := range []string{so.String(), se.String()} {
		if strings.Contains(out, bearer) {
			t.Error("the child printed the key")
		}
	}
	return so.String(), se.String()
}

// The daemon hands its ProxyCommand child the caller's bearer as
// VMTOOL_TOKEN. For a caller whose key comes from the pulled list the child's
// gate must let it through (the local service accepts it); the child then
// fails only on the unknown host.
func TestTunnelChildWithPulledKeyPassesGate(t *testing.T) {
	_, se := runTunnelChild(t, listedKey)
	if strings.Contains(se, "does not match token_sha256") || strings.Contains(se, "not answering") {
		t.Fatalf("the child's gate refused a pulled key: %s", se)
	}
	if !strings.Contains(strings.ToLower(se), "unknown host") {
		t.Errorf("want the unknown-host error after the gate, got: %s", se)
	}
}

// A key that is not in the list is still refused by the child's gate.
func TestTunnelChildWithUnlistedKeyRefused(t *testing.T) {
	_, se := runTunnelChild(t, otherKey)
	if !strings.Contains(se, "does not match token_sha256") {
		t.Fatalf("an unlisted key must be refused by the gate: %s", se)
	}
	if strings.Contains(strings.ToLower(se), "unknown host") {
		t.Errorf("the child went past the gate: %s", se)
	}
}
