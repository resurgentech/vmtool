package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/config"
)

// Test keys and a pull token only; never real ones.
const (
	listedKey  = "listed-key-5d2c81"
	listedKey2 = "listed-key-9e47b0"
	otherKey   = "unlisted-key-c3a1f6"
	pullToken  = "pull-token-0b8e7d"
)

var bothRoles = []string{config.RoleWorkstation, config.RoleHypervisor}

// listServer is a daemon of role whose personal_keys block points at l. Relative
// paths resolve against its own temporary config directory.
func listServer(t *testing.T, role string, l *keyclienttest.List, edit ...func(*config.Config)) *Server {
	t.Helper()
	c := &config.Config{
		Dir: t.TempDir(), Name: "hv1", Role: role, TokenSHA256: config.Hash(testToken),
		PersonalKeys: config.PersonalKeys{
			URL: l.URL(), CacheFile: "keys.cache",
			Refresh: time.Hour, RefreshOnMiss: 10 * time.Millisecond,
		},
	}
	for _, e := range edit {
		e(c)
	}
	return &Server{Config: c, PlaybookDir: "playbooks"}
}

// start starts s's key list with a context that ends with the test, and waits
// for the first pull to land when the provider is up.
func start(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.startKeys(ctx); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitOrigin(t *testing.T, s *Server, o keylist.Origin) {
	t.Helper()
	eventually(t, "origin "+string(o), func() bool { return s.keys.Status().Origin == o })
}

func serve1(h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func statusOf(t *testing.T, h http.Handler, key string) api.PersonalKeysStatus {
	t.Helper()
	rec := serve1(h, "GET", "/v1/personal-keys", key)
	if rec.Code != 200 {
		t.Fatalf("GET /v1/personal-keys: %d %s", rec.Code, rec.Body)
	}
	var st api.PersonalKeysStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status body %q: %v", rec.Body, err)
	}
	return st
}

// K-1, R-13: a key from the pulled list passes every authenticated operation
// exactly as the shared token does (same status), on both roles; a key that
// is not listed is refused on every one.
func TestListedKeyPassesEveryOperation(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, role := range bothRoles {
		l := keyclienttest.NewList(t)
		l.Add("alice", "full", listedKey)
		s := listServer(t, role, l)
		h := s.Handler()
		start(t, s)
		waitOrigin(t, s, keylist.OriginProvider)

		n := 0
		for _, o := range specOps(t) {
			if o.public {
				continue
			}
			p := concrete(o.path)
			label := role + " " + o.method + " " + p + " (" + o.id + ")"
			n++
			want := serve1(h, o.method, p, testToken)
			if want.Code == http.StatusUnauthorized {
				t.Errorf("%s: token_sha256's own token is refused", label)
			}
			got := serve1(h, o.method, p, listedKey)
			if got.Code != want.Code {
				t.Errorf("%s: listed key %d, shared token %d (%s)", label, got.Code, want.Code, got.Body)
			}
			check401(t, serve1(h, o.method, p, otherKey), label+" unlisted key", otherKey)
		}
		if n < 10 {
			t.Fatalf("only %d authenticated operations", n)
		}
	}
}

// K-5, R-2: a key that the next pull no longer lists is refused.
func TestListedKeyRemovedByNextPullIsRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	l := keyclienttest.NewList(t)
	l.Add("alice", "full", listedKey)
	l.Add("bob", "full", listedKey2)
	s := listServer(t, config.RoleHypervisor, l)
	h := s.Handler()
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)
	if c := serve1(h, "GET", "/v1/metal", listedKey).Code; c == 401 {
		t.Fatalf("listed key refused: %d", c)
	}
	l.Remove("alice")
	if err := s.keys.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	check401(t, serve1(h, "GET", "/v1/metal", listedKey), "removed key", listedKey)
	if c := serve1(h, "GET", "/v1/metal", listedKey2).Code; c == 401 {
		t.Errorf("the other listed key is refused: %d", c)
	}
}

// R-2: a key added after the last pull works on first use, through the
// refresh on miss.
func TestNewKeyWorksOnFirstUse(t *testing.T) {
	t.Chdir(t.TempDir())
	l := keyclienttest.NewList(t)
	s := listServer(t, config.RoleWorkstation, l)
	h := s.Handler()
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)
	check401(t, serve1(h, "GET", "/v1/metal", listedKey), "before it is listed", listedKey)
	time.Sleep(50 * time.Millisecond) // past the refresh-on-miss window the miss above opened
	l.Add("carol", "full", listedKey)
	pulls := l.Pulls()
	if c := serve1(h, "GET", "/v1/metal", listedKey).Code; c == 401 {
		t.Fatalf("new key refused on first use: %d", c)
	}
	if l.Pulls() <= pulls {
		t.Errorf("no pull on the miss (%d pulls)", l.Pulls())
	}
}

// With refresh_on_miss at 0s a key listed after the last pull is refused
// until the next timed pull.
func TestRefreshOnMissOff(t *testing.T) {
	t.Chdir(t.TempDir())
	l := keyclienttest.NewList(t)
	s := listServer(t, config.RoleWorkstation, l, func(c *config.Config) { c.PersonalKeys.RefreshOnMiss = 0 })
	h := s.Handler()
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)
	l.Add("carol", "full", listedKey)
	check401(t, serve1(h, "GET", "/v1/metal", listedKey), "new key, refresh on miss off", listedKey)
}

// K-1, K-10: with the provider down and a cache from an earlier run, the
// listed keys work; nothing else does.
func TestProviderDownWithCache(t *testing.T) {
	t.Chdir(t.TempDir())
	l := keyclienttest.NewList(t)
	l.Add("alice", "full", listedKey)
	first := listServer(t, config.RoleHypervisor, l)
	first.Handler()
	ctx, stop := context.WithCancel(context.Background())
	if err := first.startKeys(ctx); err != nil {
		t.Fatal(err)
	}
	waitOrigin(t, first, keylist.OriginProvider)
	stop()

	l.FailWith(keyclienttest.Status(500, ""))
	buf := captureSlog(t)
	second := listServer(t, config.RoleHypervisor, l, func(c *config.Config) { c.Dir = first.Config.Dir })
	h := second.Handler()
	start(t, second)
	if st := second.keys.Status(); st.Origin != keylist.OriginCache || st.Entries != 1 {
		t.Fatalf("status after start: %+v", st)
	}
	if !strings.Contains(buf.String(), "personal_keys: list origin=cache entries=1 age=") {
		t.Errorf("start-up line missing:\n%s", buf)
	}
	if c := serve1(h, "GET", "/v1/metal", listedKey).Code; c == 401 {
		t.Errorf("cached key refused while the provider is down: %d", c)
	}
	check401(t, serve1(h, "GET", "/v1/metal", otherKey), "unlisted key", otherKey)
	st := statusOf(t, h, listedKey)
	if st.Origin == nil || *st.Origin != "cache" || st.Entries == nil || *st.Entries != 1 || st.AgeSeconds == nil {
		t.Errorf("status %+v", st)
	}
}

// K-2: with no cache and no provider the daemon still starts and only
// token_sha256 works.
func TestProviderDownNoCache(t *testing.T) {
	t.Chdir(t.TempDir())
	l := keyclienttest.NewList(t)
	l.Add("alice", "full", listedKey)
	l.FailWith(keyclienttest.Status(500, ""))
	s := listServer(t, config.RoleWorkstation, l)
	h := s.Handler()
	start(t, s)
	eventually(t, "the failed pull", func() bool { return s.keys.Status().LastError != nil })
	check401(t, serve1(h, "GET", "/v1/metal", listedKey), "listed key, nothing loaded", listedKey)
	if c := serve1(h, "GET", "/v1/metal", testToken).Code; c == 401 {
		t.Errorf("token_sha256 refused: %d", c)
	}
	st := statusOf(t, h, testToken)
	if !st.Enabled || st.Origin == nil || *st.Origin != "none" || st.Entries == nil || *st.Entries != 0 ||
		st.LastError == nil || st.LastSuccess != nil || st.AgeSeconds != nil || st.ListTime != nil {
		t.Errorf("status %+v", st)
	}
}

// An entry that carries token_sha256's hash is skipped (the local token wins),
// and so is an entry with another role.
func TestEntryWithLocalHashSkipped(t *testing.T) {
	t.Chdir(t.TempDir())
	l := keyclienttest.NewList(t)
	l.Add("operator", "full", testToken)
	l.Add("alice", "full", listedKey)
	l.Add("auditor", "read-only", listedKey2)
	s := listServer(t, config.RoleWorkstation, l)
	h := s.Handler()
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)
	if n := s.keys.Len(); n != 1 {
		t.Fatalf("live entries %d, want 1 (alice)", n)
	}
	if _, _, ok := s.keys.Lookup(testToken); ok {
		t.Error("the entry with token_sha256's hash is live")
	}
	// The local token is still the local token: it is counted as such.
	if c := serve1(h, "GET", "/v1/metal", testToken).Code; c == 401 {
		t.Errorf("token_sha256 refused: %d", c)
	}
	check401(t, serve1(h, "GET", "/v1/metal", listedKey2), "role read-only", listedKey2)
	if st := statusOf(t, h, listedKey); st.LocalToken.Count != 1 {
		t.Errorf("local token count %d, want 1", st.LocalToken.Count)
	}
}

// The unset url is today's daemon: no source, no file, no log line, the
// status route answers 200 with enabled false.
func TestListOffChangesNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	buf := captureSlog(t)
	for _, role := range bothRoles {
		s := authedServer()
		s.Config.Dir = t.TempDir()
		s.Config.Role = role
		h := s.Handler()
		if err := s.startKeys(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.keys != nil {
			t.Fatal("a key source exists with no url")
		}
		check401(t, serve1(h, "GET", "/v1/metal", listedKey), "unlisted key", listedKey)
		if c := serve1(h, "GET", "/v1/metal", testToken).Code; c == 401 {
			t.Errorf("token_sha256 refused: %d", c)
		}
		st := statusOf(t, h, testToken)
		if st.Enabled || st.Origin != nil || st.Entries != nil || st.AgeSeconds != nil || st.ListTime != nil ||
			st.LastSuccess != nil || st.LastError != nil || st.CacheFailing != nil {
			t.Errorf("status with the list off: %+v", st)
		}
		if st.LocalToken.Count != 2 {
			t.Errorf("local token count %d, want 2 (one operation, one status)", st.LocalToken.Count)
		}
		if es, _ := os.ReadDir(s.Config.Dir); len(es) != 0 {
			t.Errorf("files written to the config dir: %v", es)
		}
		check401(t, serve1(h, "GET", "/v1/personal-keys", ""), "status, no token")
	}
	if out := buf.String(); strings.Contains(out, "personal_keys") || strings.Contains(out, "keylist") {
		t.Errorf("a key list line with the list off:\n%s", out)
	}
}

// The status route with the list on, on both roles: origin, entries, age and
// times; never a hash, a name, a key or the URL.
func TestPersonalKeysStatusRoute(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, role := range bothRoles {
		l := keyclienttest.NewList(t)
		l.Add("alice-name-marker", "full", listedKey)
		l.Add("bob", "full", listedKey2)
		s := listServer(t, role, l)
		h := s.Handler()
		start(t, s)
		waitOrigin(t, s, keylist.OriginProvider)
		rec := serve1(h, "GET", "/v1/personal-keys", listedKey)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %q", role, rec.Code, rec.Header().Get("Content-Type"))
		}
		var st api.PersonalKeysStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if !st.Enabled || st.Origin == nil || *st.Origin != "provider" || st.Entries == nil || *st.Entries != 2 ||
			st.AgeSeconds == nil || st.ListTime == nil || st.LastSuccess == nil || st.LastError != nil ||
			st.CacheFailing == nil || *st.CacheFailing {
			t.Errorf("%s: status %+v", role, st)
		}
		if st.LocalToken.Count != 0 || st.LocalToken.LastUsed != nil {
			t.Errorf("%s: a pulled key counted as the local token: %+v", role, st.LocalToken)
		}
		for _, bad := range []string{config.Hash(listedKey), config.Hash(listedKey2), config.Hash(testToken), "alice-name-marker", "bob", listedKey, l.URL(), "sha256"} {
			if strings.Contains(rec.Body.String(), bad) {
				t.Errorf("%s: the status holds %q: %s", role, bad, rec.Body)
			}
		}
		check401(t, serve1(h, "GET", "/v1/personal-keys", otherKey), "status, unlisted key", otherKey)
	}
}

// last_error is cut to 200 runes and holds no non-printable character.
func TestKeyStatusLastErrorCut(t *testing.T) {
	long := strings.Repeat("é", 150) + "\x00\n\t" + strings.Repeat("x", 400)
	st := keyStatus(keylist.Status{Origin: keylist.OriginProvider, Entries: 3, LastError: errors.New(long)})
	if st.LastError == nil {
		t.Fatal("no last_error")
	}
	if n := utf8.RuneCountInString(*st.LastError); n != 200 {
		t.Errorf("%d runes, want 200", n)
	}
	if strings.ContainsAny(*st.LastError, "\x00\n\t") || !strings.Contains(*st.LastError, "é??") {
		t.Errorf("non-printables kept: %q", *st.LastError)
	}
	if none := keyStatus(keylist.Status{Origin: keylist.OriginNone}); none.LastError != nil || none.AgeSeconds != nil || none.ListTime != nil {
		t.Errorf("empty status: %+v", none)
	}
}

// A fake clock for the local token's last_used and the hourly line.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// The local token's use is counted and its last time kept; one line "local
// token used" at most once an hour, with the route and no token. A listed key
// is not the local token.
func TestLocalTokenUseCountAndHourlyLine(t *testing.T) {
	t.Chdir(t.TempDir())
	buf := captureSlog(t)
	clk := &fakeClock{t: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)}
	l := keyclienttest.NewList(t)
	l.Add("alice", "full", listedKey)
	s := listServer(t, config.RoleWorkstation, l)
	s.now = clk.now
	h := s.Handler()
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)

	if st := statusOf(t, h, listedKey); st.LocalToken.Count != 0 || st.LocalToken.LastUsed != nil {
		t.Fatalf("before any use: %+v", st.LocalToken)
	}
	lines := func() int { return strings.Count(buf.String(), "local token used") }
	serve1(h, "GET", "/v1/metal", testToken)
	clk.add(10 * time.Minute)
	serve1(h, "GET", "/v1/metal", testToken)
	serve1(h, "GET", "/v1/metal", wrongToken)    // refused: not a use
	serve1(h, "GET", "/v1/metal", listedKey)     // a pulled key: not a use
	serve1(h, "GET", "/v1/metal", "")            // no token: not a use
	serve1(h, "GET", "/openapi.yaml", testToken) // public: authenticated by nothing
	if n := lines(); n != 1 {
		t.Fatalf("%d lines within the hour, want 1:\n%s", n, buf)
	}
	st := statusOf(t, h, listedKey)
	if st.LocalToken.Count != 2 || st.LocalToken.LastUsed == nil || !st.LocalToken.LastUsed.Equal(clk.now()) {
		t.Fatalf("local token: %+v (clock %v)", st.LocalToken, clk.now())
	}
	clk.add(50*time.Minute - time.Second)
	serve1(h, "GET", "/v1/hosts", testToken)
	if n := lines(); n != 1 {
		t.Errorf("%d lines 59m59s after the first, want 1", n)
	}
	clk.add(time.Second)
	serve1(h, "GET", "/v1/hosts", testToken)
	if n := lines(); n != 2 {
		t.Errorf("%d lines an hour after the first, want 2", n)
	}
	out := buf.String()
	if !strings.Contains(out, "/v1/metal") || !strings.Contains(out, "/v1/hosts") {
		t.Errorf("the line does not name the route:\n%s", out)
	}
	for _, s := range []string{testToken, wrongToken, listedKey, config.Hash(testToken)} {
		if strings.Contains(out, s) {
			t.Errorf("log contains %q", s)
		}
	}
	// A clock that steps back does not silence the line for good.
	clk.add(-48 * time.Hour)
	serve1(h, "GET", "/v1/hosts", testToken)
	if n := lines(); n != 3 {
		t.Errorf("%d lines after the clock went back, want 3", n)
	}
}

// The counter is safe under concurrent requests (run with -race).
func TestLocalTokenUseConcurrent(t *testing.T) {
	u := &localTokenUse{now: time.Now}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				u.note("GET", "/v1/metal")
				u.snapshot()
			}
		}()
	}
	wg.Wait()
	if n, _ := u.snapshot(); n != 1000 {
		t.Errorf("count %d, want 1000", n)
	}
}

// The workstation forwards the caller's own bearer to a peer, whichever way
// the caller was authenticated: a pulled key goes on unchanged.
func TestPulledKeyForwardedUnchanged(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	hv, pool, hvLog := hypervisor(t, fm, listedKey)
	l := keyclienttest.NewList(t)
	l.Add("alice", "full", listedKey)
	s := listServer(t, config.RoleWorkstation, l)
	h := &handlers{tunnelExe: stubTunnelExe}
	ws := workstationWithKeys(t, s, hv.URL, pool, h)
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)

	rec := serve1(ws, "GET", "/v1/hosts/"+peerHost+"/vms/web", listedKey)
	if rec.Code != 200 {
		t.Fatalf("GET vm: %d %s", rec.Code, rec.Body)
	}
	rec = serve1(ws, "GET", "/v1/hosts/"+peerHost+"/pools", listedKey)
	if rec.Code != 200 {
		t.Fatalf("GET pools: %d %s", rec.Code, rec.Body)
	}
	seen := hvLog.seen()
	if len(seen) < 2 {
		t.Fatalf("hypervisor saw %v", seen)
	}
	for _, s := range seen {
		if !strings.HasSuffix(s, " Bearer "+listedKey) {
			t.Errorf("hypervisor saw %q, want the caller's pulled key", s)
		}
	}
	// A caller the workstation refuses never reaches the peer.
	n := len(seen)
	check401(t, serve1(ws, "GET", "/v1/hosts/"+peerHost+"/vms/web", otherKey), "unlisted", otherKey)
	if len(hvLog.seen()) != n {
		t.Error("a refused caller reached the hypervisor")
	}
	// The forwarded bearer of the workstation's own ssh children is the same
	// request bearer (guestSvc), unchanged: see TestWorkstationForwardsBearer.
}

// workstationWithKeys is remote_test.go's workstation with s's authentication
// (token_sha256 and the pulled list) in front of the chain.
func workstationWithKeys(t *testing.T, s *Server, hvURL string, pool *x509.CertPool, h *handlers) http.Handler {
	t.Helper()
	workstation(t, hvURL, pool, "unused-"+testToken, h) // wires h to the peer; its own auth chain is replaced below
	if err := s.initKeys(); err != nil {
		t.Fatal(err)
	}
	a := &authenticator{hash: s.Config.TokenSHA256, keys: s.keys, local: s.local}
	return chain(h, func(next http.Handler) http.Handler { return requireAuth(a, publicRoutes(), next) })
}

// No key, hash or pull token is ever logged, by the daemon or by the key
// list's own lines (routed through the daemon's logger): through pulls, a
// refresh on miss, a skipped entry, a failing provider and refused keys.
func checkPulledKeysNeverLogged(t *testing.T) {
	t.Chdir(t.TempDir())
	buf := captureSlog(t)
	l := keyclienttest.NewList(t)
	l.RequireToken(pullToken)
	l.Add("operator", "full", testToken) // skipped, logged by name
	l.Add("alice", "full", listedKey)
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "pull-token")
	if err := os.WriteFile(tokFile, []byte(pullToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := listServer(t, config.RoleHypervisor, l, func(c *config.Config) { c.PersonalKeys.TokenFile = tokFile })
	h := s.Handler()
	start(t, s)
	waitOrigin(t, s, keylist.OriginProvider)

	for _, k := range []string{testToken, wrongToken, listedKey, otherKey} {
		serve1(h, "GET", "/v1/metal", k)
	}
	l.Add("bob", "full", listedKey2)
	serve1(h, "GET", "/v1/metal", listedKey2) // refresh on miss
	l.RequireToken("rotated-" + pullToken)    // the provider now refuses the pull token: a failing pull
	_ = s.keys.Pull(context.Background())
	serve1(h, "GET", "/v1/metal", otherKey)
	statusOf(t, h, listedKey)

	out := buf.String()
	if !strings.Contains(out, "status=200") || !strings.Contains(out, "status=401") || !strings.Contains(out, "operator") {
		t.Fatalf("expected request lines and the skipped entry's line:\n%s", out)
	}
	for _, secret := range []string{
		testToken, wrongToken, listedKey, listedKey2, otherKey, pullToken,
		config.Hash(testToken), config.Hash(listedKey), config.Hash(listedKey2), config.Hash(otherKey),
		"Authorization", "Bearer",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
	// The status route's last_error is as clean.
	rec := serve1(h, "GET", "/v1/personal-keys", testToken)
	for _, secret := range []string{pullToken, config.Hash(listedKey), listedKey} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("status body contains %q: %s", secret, rec.Body)
		}
	}
}

// The key source is started before the listener binds: with the port taken,
// ListenAndServe has already logged the list's start-up line when it fails.
func TestListStartedBeforeBind(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	writeCert(t, dir)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	l := keyclienttest.NewList(t)
	s := &Server{Listen: taken.Addr().String(), PlaybookDir: "playbooks", Config: &config.Config{
		Dir: dir, Name: "hv1", Role: config.RoleWorkstation, TokenSHA256: config.Hash(testToken),
		TLSCert: "tls/test.crt", TLSKey: "tls/test.key",
		PersonalKeys: config.PersonalKeys{URL: l.URL(), CacheFile: "keys.cache", Refresh: time.Hour},
	}}
	err = s.ListenAndServe()
	if err == nil {
		t.Fatal("bound a taken port")
	}
	if !strings.Contains(buf.String(), "personal_keys: list origin=none entries=0 age=0s") {
		t.Errorf("the list was not started before the bind (%v):\n%s", err, buf)
	}
}

// A key list that cannot be built is a start-up error that names the setting
// and never echoes the URL.
func TestListBuildErrorStopsStart(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir)
	s := &Server{Config: &config.Config{
		Dir: dir, TokenSHA256: config.Hash(testToken), TLSCert: "tls/test.crt", TLSKey: "tls/test.key",
		PersonalKeys: config.PersonalKeys{URL: "http://keys.example.lab/keys/v1/lists/vmtool", CacheFile: "keys.cache"},
	}}
	_, err := s.httpServer()
	if err == nil || !strings.Contains(err.Error(), "personal_keys") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "keys.example.lab/keys") {
		t.Errorf("the error echoes the URL: %v", err)
	}
}
