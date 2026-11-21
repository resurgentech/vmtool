package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	// creds are the VM records by "host/name" (S8); gets counts their GETs and
	// onGet, if set, runs after each with the 1-based count.
	mu    sync.Mutex
	creds map[string]map[string]any
	gets  atomic.Int32
	onGet func(n int32)
	// images are the store's base images (S10): bytes by name and their
	// metadata; pushes counts PUTs; corrupt flips a byte of every blob it
	// serves while the headers still say the true digest.
	images  map[string][]byte
	updated map[string]time.Time
	pushes  atomic.Int32
	corrupt atomic.Bool
	// invQuery is the last GET /v1/inventory request URI; inv is the body.
	invQuery atomic.Value
	inv      map[string]any
	// notes are the notes writes (PUT /v1/inventory/{host}/{name}/notes).
	notes []notesWrite
}

type notesWrite struct {
	path string
	body map[string]any
}

func (s *cliStore) notesWrites() []notesWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notesWrite(nil), s.notes...)
}

// newCLIStore starts a store and points the CLI's store client at its test
// CA. No system trust store, no network beyond loopback.
func newCLIStore(t *testing.T) *cliStore {
	t.Helper()
	s := &cliStore{creds: map[string]map[string]any{}, images: map[string][]byte{}, updated: map[string]time.Time{}}
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
		case "/v1/inventory":
			if r.Header.Get("Authorization") != "Bearer "+storeTok {
				out(401, map[string]string{"error": "unauthorized"})
				return
			}
			s.invQuery.Store(r.URL.RequestURI())
			// Like the store: VMs their host no longer has only with gone=true.
			body := map[string]any{"hosts": s.inv["hosts"], "machines": s.inv["machines"]}
			vms := []any{}
			for _, v := range s.inv["vms"].([]any) {
				if _, isGone := v.(map[string]any)["gone_since"]; !isGone || r.URL.Query().Get("gone") == "true" {
					vms = append(vms, v)
				}
			}
			body["vms"] = vms
			out(200, body)
		default:
			if key, ok := strings.CutPrefix(r.URL.Path, "/v1/credentials/vms/"); ok {
				s.serveVM(w, r, key, out)
				return
			}
			if r.URL.Path == "/v1/images" || strings.HasPrefix(r.URL.Path, "/v1/images/") {
				s.serveImages(w, r, out)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/v1/inventory/") && strings.HasSuffix(r.URL.Path, "/notes") && r.Method == http.MethodPut {
				if r.Header.Get("Authorization") != "Bearer "+storeTok {
					out(401, map[string]string{"error": "unauthorized"})
					return
				}
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				s.mu.Lock()
				s.notes = append(s.notes, notesWrite{path: r.URL.Path, body: in})
				s.mu.Unlock()
				parts := strings.Split(r.URL.Path, "/")
				c, _ := in["comment"].(string)
				out(200, map[string]any{"host": parts[3], "name": parts[4], "uuid": "00000000-0000-4000-8000-000000000001", "comment": c})
				return
			}
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

func (s *cliStore) serveVM(w http.ResponseWriter, r *http.Request, key string, out func(int, any)) {
	if r.Header.Get("Authorization") != "Bearer "+storeTok {
		out(401, map[string]string{"error": "unauthorized"})
		return
	}
	var post func() // runs after the lock is released
	defer func() {
		if post != nil {
			post()
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.creds[key] = in
		out(200, map[string]any{"uuid": in["uuid"], "host": "h", "name": "n", "updated": time.Now()})
	case http.MethodGet:
		n := s.gets.Add(1)
		if s.onGet != nil {
			post = func() { s.onGet(n) }
		}
		rec, ok := s.creds[key]
		switch {
		case !ok:
			out(404, map[string]any{"error": "not found"})
		case rec["uuid"] != r.URL.Query().Get("uuid"):
			out(404, map[string]any{"error": "stale", "stale": true})
		default:
			full := map[string]any{"host": "h", "name": "n", "uuid": rec["uuid"], "username": rec["username"],
				"password": "", "private_key": "", "public_key": "", "updated": time.Now()}
			for _, k := range []string{"password", "private_key", "public_key"} {
				if v, ok := rec[k]; ok {
					full[k] = v
				}
			}
			out(200, full)
		}
	case http.MethodDelete:
		delete(s.creds, key)
		w.WriteHeader(204)
	}
}

func (s *cliStore) imageMeta(name string) map[string]any {
	b := s.images[name]
	sum := sha256.Sum256(b)
	return map[string]any{"name": name, "sha256": hex.EncodeToString(sum[:]), "size": len(b), "updated": s.updated[name]}
}

// serveImages is the store's image routes: list, get, put and blob.
func (s *cliStore) serveImages(w http.ResponseWriter, r *http.Request, out func(int, any)) {
	if r.Header.Get("Authorization") != "Bearer "+storeTok {
		out(401, map[string]string{"error": "unauthorized"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/images"), "/")
	if rest == "" {
		names := make([]string, 0, len(s.images))
		for n := range s.images {
			names = append(names, n)
		}
		sort.Strings(names)
		list := []map[string]any{}
		for _, n := range names {
			list = append(list, s.imageMeta(n))
		}
		out(200, map[string]any{"images": list})
		return
	}
	name, blob := strings.CutSuffix(rest, "/blob")
	b, have := s.images[name]
	switch {
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != r.Header.Get("X-Image-SHA256") || int64(len(body)) != r.ContentLength {
			out(400, map[string]string{"error": "digest or size mismatch"})
			return
		}
		s.pushes.Add(1)
		s.images[name] = body
		s.updated[name] = time.Now()
		out(201, s.imageMeta(name))
	case !have:
		out(404, map[string]string{"error": "not found"})
	case blob:
		meta := s.imageMeta(name)
		w.Header().Set("X-Image-SHA256", meta["sha256"].(string))
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(200)
		if s.corrupt.Load() {
			b = append([]byte{b[0] ^ 0xff}, b[1:]...)
		}
		_, _ = w.Write(b)
	default:
		out(200, s.imageMeta(name))
	}
}

// vmRecord puts a VM record into the store double.
func (s *cliStore) vmRecord(key, uuid, pass, privKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[key] = map[string]any{"uuid": uuid, "username": "bot", "password": pass, "private_key": privKey, "public_key": "PLACEHOLDER-pub"}
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
	"vmtool creds fetch": true, "vmtool creds forget": true,
	"vmtool images push": true, "vmtool images pull": true, "vmtool templates prepare": true,
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
