package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/storeclient"
)

// Placeholders only.
const storeTok = "PLACEHOLDER-not-a-secret-1"

const unreachableLine = "127.0.0.1 unreachable: using this machine's .machines/ and image_dir\n"

// cliStore is an in-process store on loopback TLS.
type cliStore struct {
	ts      *httptest.Server
	whoami  atomic.Int32
	version atomic.Int32
	down    atomic.Bool
}

// newCLIStore starts a store and points the CLI's store client at its test
// CA. No system trust store, no network beyond loopback.
func newCLIStore(t *testing.T) *cliStore {
	t.Helper()
	s := &cliStore{}
	s.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if s.down.Load() {
			out(503, map[string]string{"error": "down"})
			return
		}
		switch r.URL.Path {
		case "/version":
			s.version.Add(1)
			out(200, map[string]string{"version": "1.0.0"})
		case "/v1/whoami":
			s.whoami.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+storeTok {
				out(401, map[string]string{"error": "unauthorized"})
				return
			}
			out(200, map[string]string{"name": "jared"})
		default:
			out(404, map[string]string{"error": "not found"})
		}
	}))
	t.Cleanup(s.ts.Close)
	pool := s.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	old := storeClientOptions
	storeClientOptions = []storeclient.Option{storeclient.WithRootCAs(pool)}
	t.Cleanup(func() { storeClientOptions = old })
	return s
}

func (s *cliStore) cfg() string {
	return "server:\n  url: " + s.ts.URL + "\n"
}

func (s *cliStore) host() string {
	u, _ := url.Parse(s.ts.URL)
	return u.Host
}

// storeWorkdir is a working directory whose config.yml names the store; the
// host token and store token are in the environment.
func storeWorkdir(t *testing.T, s *cliStore, token string) string {
	t.Helper()
	t.Setenv(config.TokenEnv, gateToken)
	t.Setenv(config.ServerTokenEnv, token)
	t.Chdir(t.TempDir())
	return writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\n"+s.cfg(), "")
}

// The commands that use credentials or base images decide the mode at their
// start; no other command probes the store. Hand list: change it on purpose.
var storeUsers = map[string]bool{
	"vmtool create": true, "vmtool delete": true, "vmtool ssh": true, "vmtool exec": true,
	"vmtool playbook": true, "vmtool resize-disk": true, "vmtool i": true,
	"vmtool metal add": true, "vmtool metal list": true, "vmtool metal info": true,
	"vmtool metal rm": true, "vmtool metal ssh": true, "vmtool metal exec": true,
	"vmtool metal playbook": true, "vmtool metal reboot": true,
}

func TestStoreModeDecidedAtCommandStart(t *testing.T) {
	s := newCLIStore(t)
	cmds := commandArgs(t)
	for path := range storeUsers {
		if _, ok := cmds[path]; !ok {
			t.Errorf("storeUsers lists %q, which is not in the tree", path)
		}
	}
	dir := storeWorkdir(t, s, storeTok)
	for path, args := range cmds {
		if exemptPaths[path] {
			continue
		}
		before := s.whoami.Load()
		r := runGate(t, dir, nil, args...)
		if r.err != nil {
			t.Errorf("%s: %v", path, r.err)
			continue
		}
		got := s.whoami.Load() - before
		want := int32(0)
		if storeUsers[path] {
			want = 1
		}
		if got != want {
			t.Errorf("%s: %d whoami calls, want %d", path, got, want)
		}
		if r.errOut != "" {
			t.Errorf("%s: stderr %q with a reachable store", path, r.errOut)
		}
	}
}

func TestStoreUnreachableOneStderrLine(t *testing.T) {
	for name, setup := range map[string]func(*cliStore) string{
		"down":        func(s *cliStore) string { s.down.Store(true); return storeTok },
		"wrong token": func(*cliStore) string { return "PLACEHOLDER-wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			s := newCLIStore(t)
			dir := storeWorkdir(t, s, setup(s))
			// The real `metal list`: it builds the service and reads .machines/
			// in a temp dir. Nothing else runs.
			r := runGate(t, dir, map[string]bool{"vmtool metal list": true}, "metal", "list")
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.errOut != unreachableLine {
				t.Fatalf("stderr %q, want exactly %q", r.errOut, unreachableLine)
			}
			for _, tok := range []string{storeTok, "PLACEHOLDER-wrong"} {
				if strings.Contains(r.out+r.errOut, tok) {
					t.Fatal("output carries the store token")
				}
			}
		})
	}
}

func TestStoreMissingTokenIsLocalWithOneLine(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, "")
	r := runGate(t, dir, map[string]bool{"vmtool metal list": true}, "metal", "list")
	want := "127.0.0.1: no VMTOOL_SERVER_TOKEN in environment or .env: using this machine's .machines/ and image_dir\n"
	if r.err != nil || r.errOut != want || s.whoami.Load() != 0 {
		t.Fatalf("err %v stderr %q whoami %d", r.err, r.errOut, s.whoami.Load())
	}
}

func TestStoreTokenFromDotEnv(t *testing.T) {
	s := newCLIStore(t)
	t.Setenv(config.TokenEnv, gateToken)
	t.Setenv(config.ServerTokenEnv, "")
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\n"+s.cfg(), "VMTOOL_SERVER_TOKEN="+storeTok+"\n")
	r := runGate(t, dir, map[string]bool{"vmtool metal list": true}, "metal", "list")
	if r.err != nil || r.errOut != "" || s.whoami.Load() != 1 {
		t.Fatalf("err %v stderr %q whoami %d", r.err, r.errOut, s.whoami.Load())
	}
}

// No server: block: group 1 exactly. No probe, no line, no row.
func TestNoServerConfigured(t *testing.T) {
	s := newCLIStore(t)
	t.Setenv(config.TokenEnv, gateToken)
	t.Setenv(config.ServerTokenEnv, storeTok)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\n", "")
	r := runGate(t, dir, map[string]bool{"vmtool metal list": true}, "metal", "list")
	if r.err != nil || r.errOut != "" {
		t.Fatalf("err %v stderr %q", r.err, r.errOut)
	}
	if s.whoami.Load()+s.version.Load() != 0 {
		t.Fatal("probed a store that is not configured")
	}
}

func TestHostsStoreRow(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	installListHosts(t, map[string]listMgr{"ws": {}})
	r := runGate(t, dir, map[string]bool{"vmtool hosts": true}, "hosts")
	if r.err != nil {
		t.Fatal(r.err)
	}
	ours := mustOurVersion(t)
	want := "NAME ADDRESS REACHABLE API CERT_DAYS ERROR\n" +
		"ws local yes " + ours + " - -\n" +
		"\n" +
		"NAME URL REACHABLE CALLER VERSION ERROR\n" +
		"store " + s.ts.URL + " yes jared 1.0.0 -\n"
	if got := norm(r.out); got != want {
		t.Errorf("hosts output:\n%s\nwant:\n%s", got, want)
	}
	if r.errOut != "" {
		t.Errorf("stderr %q", r.errOut)
	}

	// Wrong token: reachable, caller unknown, the refusal named.
	t.Setenv(config.ServerTokenEnv, "PLACEHOLDER-wrong")
	r = runGate(t, dir, map[string]bool{"vmtool hosts": true}, "hosts")
	if !strings.Contains(norm(r.out), "store "+s.ts.URL+" yes - 1.0.0 ") || strings.Contains(r.out, "PLACEHOLDER-wrong") {
		t.Errorf("wrong token:\n%s", r.out)
	}

	// Down: not reachable, one row, exit status unchanged.
	s.down.Store(true)
	t.Setenv(config.ServerTokenEnv, storeTok)
	r = runGate(t, dir, map[string]bool{"vmtool hosts": true}, "hosts")
	if r.err != nil || !strings.Contains(norm(r.out), "store "+s.ts.URL+" no - - ") || strings.Contains(r.out, storeTok) {
		t.Errorf("down: err %v\n%s", r.err, r.out)
	}
}

func TestHostsNoStoreRowWithoutServer(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, twoHostCfg(""), "")
	installListHosts(t, map[string]listMgr{"jetfire": {}, "veritech01": {st: app.HostStatus{Address: "v.labtwo:9473", Reachable: true, APIVersion: "1.0.0"}}})
	r := runGate(t, dir, map[string]bool{"vmtool hosts": true}, "hosts")
	if r.err != nil || strings.Contains(r.out, "CALLER") || strings.Contains(r.out, "store ") {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
}

// The store token is never in a child's argv (the same check as the host
// token's): not in ssh's, and not in the tunnel's.
func TestStoreTokenNotInArgv(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	e := &env{dir: dir}
	if err := e.gate(); err != nil {
		t.Fatal(err)
	}
	e.tunnelExe = "/opt/vmtool/vmtool"
	svc := e.service("")
	c, err := svc.SSHCmd(app.VMRef{Host: "ws", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(c.Args, func(a string) bool { return strings.Contains(a, storeTok) }) {
		t.Fatalf("store token in argv %q", c.Args)
	}
	if !slices.Contains(c.Args, "ProxyCommand=/opt/vmtool/vmtool tunnel ws/web") {
		t.Fatalf("argv %q", c.Args)
	}
}
