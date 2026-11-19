package app

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	stTok  = "PLACEHOLDER-not-a-secret-1"
	stPass = "PLACEHOLDER-not-a-secret-2"
	stUUID = "00000000-0000-4000-8000-000000000001"
)

// fakeStoreServer is an in-process store on loopback TLS. whoami counts the
// authenticated probes. down makes every answer a 503; slow holds whoami.
type fakeStoreServer struct {
	ts     *httptest.Server
	whoami atomic.Int32
	down   atomic.Bool
	slow   chan struct{}
	mu     sync.Mutex
	creds  map[string]map[string]any // "host/name" -> record
}

func newFakeStoreServer(t *testing.T) *fakeStoreServer {
	t.Helper()
	f := &fakeStoreServer{slow: make(chan struct{}), creds: map[string]map[string]any{}}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, `{"error":"down"}`, 503)
			return
		}
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.URL.Path == "/version" {
			out(200, map[string]string{"version": "1.0.0"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+stTok {
			out(401, map[string]string{"error": "unauthorized"})
			return
		}
		switch {
		case r.URL.Path == "/v1/whoami":
			f.whoami.Add(1)
			select {
			case <-f.slow:
			case <-r.Context().Done():
				return
			}
			out(200, map[string]string{"name": "jared"})
		case strings.HasPrefix(r.URL.Path, "/v1/credentials/vms/"):
			key := strings.TrimPrefix(r.URL.Path, "/v1/credentials/vms/")
			f.mu.Lock()
			defer f.mu.Unlock()
			switch r.Method {
			case http.MethodPut:
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				in["updated"] = time.Now()
				f.creds[key] = in
				out(200, map[string]any{"uuid": in["uuid"], "updated": time.Now(), "host": "h", "name": "n"})
			case http.MethodGet:
				rec, ok := f.creds[key]
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
				if _, ok := f.creds[key]; !ok {
					out(404, map[string]any{"error": "not found"})
					return
				}
				delete(f.creds, key)
				w.WriteHeader(204)
			}
		default:
			out(404, map[string]string{"error": "not found"})
		}
	}))
	close(f.slow) // answer at once unless a test replaces the channel
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeStoreServer) pool() *x509.CertPool {
	return f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
}

func (f *fakeStoreServer) session(t *testing.T, token string, notify func(string)) *StoreSession {
	t.Helper()
	return OpenStoreSession(f.ts.URL, func() (string, error) { return token, nil },
		[]storeclient.Option{storeclient.WithRootCAs(f.pool())}, notify)
}

type notices struct {
	mu   sync.Mutex
	list []string
}

func (n *notices) add(s string) { n.mu.Lock(); n.list = append(n.list, s); n.mu.Unlock() }

func TestStoreSessionNotConfigured(t *testing.T) {
	var s *StoreSession = OpenStoreSession("", func() (string, error) { return stTok, nil }, nil, nil)
	if s != nil {
		t.Fatalf("session for no url: %+v", s)
	}
	// A nil session is local mode, with no notice, and local sources.
	if s.Mode(context.Background()) != ModeLocal || s.Notice() != "" {
		t.Fatal("nil session is not quietly local")
	}
	if _, ok := s.Credentials().(LocalCredentials); !ok {
		t.Fatalf("credentials %T", s.Credentials())
	}
	if _, err := s.Catalog().List(context.Background()); !errors.Is(err, ErrNoStore) {
		t.Fatalf("catalog: %v", err)
	}
}

func TestStoreSessionReachableIsStoreModeForTheWholeCommand(t *testing.T) {
	f := newFakeStoreServer(t)
	var n notices
	s := f.session(t, stTok, n.add)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if m := s.Mode(ctx); m != ModeStore {
			t.Fatalf("call %d: mode %v", i, m)
		}
	}
	if got := f.whoami.Load(); got != 1 {
		t.Fatalf("whoami called %d times, want 1", got)
	}
	if s.Caller() != "jared" || s.Notice() != "" || len(n.list) != 0 {
		t.Fatalf("caller %q notice %q notices %v", s.Caller(), s.Notice(), n.list)
	}
	// The store goes away mid-command: the mode does not change.
	f.down.Store(true)
	if m := s.Mode(ctx); m != ModeStore {
		t.Fatalf("mode after outage: %v", m)
	}
	if f.whoami.Load() != 1 {
		t.Fatal("probed again")
	}
	if _, ok := s.CredentialsFor(ctx).(StoreCredentials); !ok {
		t.Fatalf("credentials %T", s.CredentialsFor(ctx))
	}
	if _, ok := s.CatalogFor(ctx).(StoreImages); !ok {
		t.Fatalf("catalog %T", s.CatalogFor(ctx))
	}
}

func TestStoreSessionFallsBackToLocalWithOneLine(t *testing.T) {
	// A closed server: the connection is refused.
	dead := newFakeStoreServer(t)
	deadURL, pool := dead.ts.URL, dead.pool()
	dead.ts.Close()
	slow := newFakeStoreServer(t)
	slow.slow = make(chan struct{}) // whoami blocks
	down := newFakeStoreServer(t)
	down.down.Store(true)

	old := WhoamiTimeout
	WhoamiTimeout = 300 * time.Millisecond
	t.Cleanup(func() { WhoamiTimeout = old })

	for name, tc := range map[string]struct {
		s    func(notify func(string)) *StoreSession
		host string
	}{
		"refused": {func(n func(string)) *StoreSession {
			return OpenStoreSession(deadURL, func() (string, error) { return stTok, nil },
				[]storeclient.Option{storeclient.WithRootCAs(pool)}, n)
		}, "127.0.0.1"},
		"slow":        {func(n func(string)) *StoreSession { return slow.session(t, stTok, n) }, "127.0.0.1"},
		"503":         {func(n func(string)) *StoreSession { return down.session(t, stTok, n) }, "127.0.0.1"},
		"wrong token": {func(n func(string)) *StoreSession { return newFakeStoreServerSession(t, "PLACEHOLDER-wrong", n) }, "127.0.0.1"},
	} {
		var nt notices
		s := tc.s(nt.add)
		start := time.Now()
		for i := 0; i < 3; i++ {
			if m := s.Mode(context.Background()); m != ModeLocal {
				t.Fatalf("%s: mode %v", name, m)
			}
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: took %v", name, d)
		}
		want := tc.host + " unreachable: using this machine's .machines/ and image_dir"
		if len(nt.list) != 1 || nt.list[0] != want || s.Notice() != want {
			t.Errorf("%s: notices %q, Notice %q, want one %q", name, nt.list, s.Notice(), want)
		}
		if strings.Contains(s.Notice(), stTok) {
			t.Errorf("%s: notice carries the token", name)
		}
		if _, ok := s.CredentialsFor(context.Background()).(LocalCredentials); !ok {
			t.Errorf("%s: credentials %T", name, s.CredentialsFor(context.Background()))
		}
	}
}

func newFakeStoreServerSession(t *testing.T, token string, notify func(string)) *StoreSession {
	return newFakeStoreServer(t).session(t, token, notify)
}

func TestStoreSessionMissingToken(t *testing.T) {
	f := newFakeStoreServer(t)
	var n notices
	s := OpenStoreSession(f.ts.URL, func() (string, error) { return "", errors.New("no VMTOOL_SERVER_TOKEN in environment or .env") },
		[]storeclient.Option{storeclient.WithRootCAs(f.pool())}, n.add)
	if s.Mode(context.Background()) != ModeLocal {
		t.Fatal("not local")
	}
	if f.whoami.Load() != 0 {
		t.Fatal("probed without a token")
	}
	want := "127.0.0.1: no VMTOOL_SERVER_TOKEN in environment or .env: using this machine's .machines/ and image_dir"
	if len(n.list) != 1 || n.list[0] != want {
		t.Fatalf("notices %q, want %q", n.list, want)
	}
}

func TestStoreSessionStatus(t *testing.T) {
	f := newFakeStoreServer(t)
	ctx := context.Background()
	st := f.session(t, stTok, nil).Status(ctx, time.Second)
	if !st.Reachable || st.Caller != "jared" || st.Version != "1.0.0" || st.Err != nil || st.Host != "127.0.0.1" {
		t.Fatalf("%+v", st)
	}
	st = f.session(t, "PLACEHOLDER-wrong", nil).Status(ctx, time.Second)
	if !st.Reachable || st.Caller != "" || st.Version != "1.0.0" || st.Err == nil {
		t.Fatalf("wrong token: %+v", st)
	}
	f.down.Store(true)
	st = f.session(t, stTok, nil).Status(ctx, time.Second)
	if st.Reachable || st.Err == nil {
		t.Fatalf("down: %+v", st)
	}
	for _, s := range []StoreStatus{st} {
		if strings.Contains(s.Err.Error(), stTok) {
			t.Fatal("status error carries the token")
		}
	}
}

// StoreCredentials against the in-process store, and LocalCredentials against
// .machines/ in a temp dir, answer the same contract.
func TestCredentialSourcesSameContract(t *testing.T) {
	f := newFakeStoreServer(t)
	for name, mk := range map[string]func(t *testing.T) CredentialSource{
		"local": func(t *testing.T) CredentialSource { t.Chdir(t.TempDir()); return LocalCredentials{} },
		"store": func(t *testing.T) CredentialSource { return f.session(t, stTok, nil).Credentials() },
	} {
		t.Run(name, func(t *testing.T) {
			src := mk(t)
			ctx := context.Background()
			if _, err := src.GetVM(ctx, "hv1", "web", stUUID); !vmtool.IsNotFound(err) {
				t.Fatalf("missing: %v", err)
			}
			in := VMCredential{UUID: stUUID, Username: "bot", Password: stPass, PrivateKey: "PLACEHOLDER-KEY-not-a-key", PublicKey: "PLACEHOLDER-pub"}
			if err := src.PutVM(ctx, "hv1", "web", in); err != nil {
				t.Fatal(err)
			}
			got, err := src.GetVM(ctx, "hv1", "web", stUUID)
			if err != nil || got.Username != "bot" || got.Password != stPass || got.PrivateKey != in.PrivateKey || got.PublicKey != in.PublicKey {
				t.Fatalf("get %+v %v", got, err)
			}
			if err := src.DeleteVM(ctx, "hv1", "web"); err != nil {
				t.Fatal(err)
			}
			if _, err := src.GetVM(ctx, "hv1", "web", stUUID); !vmtool.IsNotFound(err) {
				t.Fatalf("after delete: %v", err)
			}
		})
	}
}

func TestStoreCredentialsStaleUUID(t *testing.T) {
	f := newFakeStoreServer(t)
	src := f.session(t, stTok, nil).Credentials()
	ctx := context.Background()
	if err := src.PutVM(ctx, "hv1", "web", VMCredential{UUID: stUUID, Username: "bot"}); err != nil {
		t.Fatal(err)
	}
	_, err := src.GetVM(ctx, "hv1", "web", "00000000-0000-4000-8000-000000000002")
	if !errors.Is(err, ErrStaleCredential) || !vmtool.IsNotFound(err) {
		t.Fatalf("stale: %v", err)
	}
}

func TestLocalCredentialsLayout(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var src CredentialSource = LocalCredentials{}
	ctx := context.Background()
	if err := src.PutVM(ctx, "hv1", "web", VMCredential{Username: "bot", Password: stPass, PrivateKey: "PLACEHOLDER-KEY-not-a-key", PublicKey: "PLACEHOLDER-pub"}); err != nil {
		t.Fatal(err)
	}
	// The same layout setup_bot.yml writes: ResolveVMAuth reads it back.
	a := vmtool.ResolveVMAuth("hv1", "web", vmtool.Auth{})
	if a.User != "bot" || a.Password != stPass || a.KeyPath == "" {
		t.Fatalf("ResolveVMAuth %+v", a)
	}
	assertMode(t, vmtool.CredsDir("hv1", "web"), 0o700)
	assertMode(t, a.KeyPath, 0o600)
	assertMode(t, a.KeyPath+".pub", 0o600)
	assertMode(t, vmtool.CredsDir("hv1", "web")+"/password.yml", 0o600)
}

func TestLocalCredentialsMachines(t *testing.T) {
	t.Chdir(t.TempDir())
	var src CredentialSource = LocalCredentials{}
	ctx := context.Background()
	if ms, err := src.ListMachines(ctx); err != nil || len(ms) != 0 {
		t.Fatalf("empty: %v %v", ms, err)
	}
	if err := src.PutMachine(ctx, MachineRecord{Name: "nuc1", Kind: vmtool.MachineMetal, Address: "10.0.0.5", Username: "bot", Password: stPass}); err != nil {
		t.Fatal(err)
	}
	ms, err := src.ListMachines(ctx)
	if err != nil || len(ms) != 1 || ms[0].Name != "nuc1" || ms[0].Kind != vmtool.MachineMetal {
		t.Fatalf("list %v %v", ms, err)
	}
	m, err := src.GetMachine(ctx, "nuc1")
	if err != nil || m.Address != "10.0.0.5" || m.Username != "bot" || m.Password != stPass {
		t.Fatalf("get %+v %v", m, err)
	}
	if err := src.DeleteMachine(ctx, "nuc1"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.GetMachine(ctx, "nuc1"); !vmtool.IsNotFound(err) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestStoreCredentialsMachines(t *testing.T) {
	// Machine routes are covered in storeclient; here the store source only
	// has to map "no such machine" to not-found. The fake answers 404.
	f := newFakeStoreServer(t)
	src := f.session(t, stTok, nil).Credentials()
	if _, err := src.GetMachine(context.Background(), "nuc1"); !vmtool.IsNotFound(err) {
		t.Fatalf("missing: %v", err)
	}
}

func TestServiceStoreAccessors(t *testing.T) {
	s := &Service{}
	if _, ok := s.Credentials().(LocalCredentials); !ok {
		t.Fatalf("no session: %T", s.Credentials())
	}
	f := newFakeStoreServer(t)
	sess := f.session(t, stTok, nil)
	c := s.WithStore(sess)
	if c == s || s.Store != nil {
		t.Fatal("WithStore mutated the receiver")
	}
	if _, ok := c.Credentials().(*modeCredentials); !ok {
		t.Fatalf("credentials %T", c.Credentials())
	}
	if f.whoami.Load() != 0 {
		t.Fatal("building a service probed the store")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s: mode %o, want %o", path, got, want)
	}
}
