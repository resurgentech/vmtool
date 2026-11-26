package store

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/keyclient/keylist"
	"github.com/resurgentech/vmtool/internal/keycfg"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/tokenhash"
)

const (
	keyP1     = "PLACEHOLDER-pulled-key-1"
	keyP2     = "PLACEHOLDER-pulled-key-2"
	keyP3     = "PLACEHOLDER-pulled-key-3"
	keyOther  = "PLACEHOLDER-unlisted-key"
	pullToken = "PLACEHOLDER-pull-token"
)

// keyEnv is a store with the pulled key list on: tokens.yaml has alice and
// bob, host_tokens.yaml has hv1.
type keyEnv struct {
	ts   *httptest.Server
	logs *syncBuf
	s    *Server
	l    *keyclienttest.List
	cfg  *Config
}

func newKeyEnv(t *testing.T, l *keyclienttest.List, edit ...func(*Config)) *keyEnv {
	t.Helper()
	e := &keyEnv{logs: &syncBuf{}, l: l}
	e.cfg = &Config{
		Listen:     "127.0.0.1:8095",
		Tokens:     map[string]string{"alice": tokenhash.Hash(tokA), "bob": tokenhash.Hash(tokB)},
		HostTokens: map[string]string{"hv1": tokenhash.Hash(tokH)},
	}
	if l != nil {
		e.cfg.PersonalKeys = keycfg.Settings{
			URL:           l.URL(),
			CacheFile:     filepath.Join(t.TempDir(), "keys.cache"),
			Refresh:       time.Hour,
			RefreshOnMiss: time.Millisecond,
		}
	}
	for _, f := range edit {
		f(e.cfg)
	}
	e.s = NewServer(e.cfg, slog.New(slog.NewJSONHandler(e.logs, nil)), Deps{Blob: blob.NewMemory(), DB: openTestDB(t)})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e.ts = httptest.NewServer(e.s.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *keyEnv) waitProvider(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.s.keys.Load().Status().Origin == keylist.OriginProvider {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the list never came from the provider")
}

func (e *keyEnv) pull(t *testing.T) {
	t.Helper()
	if err := e.s.keys.Load().Pull(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func (e *keyEnv) code(t *testing.T, method, p, tok, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+p, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func newList(t *testing.T) *keyclienttest.List {
	t.Helper()
	l := keyclienttest.NewList(t)
	l.Add("carol", "full", keyP1)
	return l
}

// ---- the pulled key as a caller -----------------------------------------

func TestPulledKeyReadsAndWritesEverything(t *testing.T) {
	e := newKeyEnv(t, newList(t))
	e.waitProvider(t)
	tok := keyP1

	// credentials
	body := `{"kind":"metal","address":"hv1.example.lab","username":"u"}`
	if c := e.code(t, "PUT", "/v1/credentials/machines/m1", tok, body); c != 200 {
		t.Fatalf("put machine credential = %d", c)
	}
	if c := e.code(t, "GET", "/v1/credentials/machines/m1", tok, ""); c != 200 {
		t.Fatalf("get machine credential = %d", c)
	}
	if c := e.code(t, "DELETE", "/v1/credentials/machines/m1", tok, ""); c != 204 {
		t.Fatalf("delete machine credential = %d", c)
	}

	// images
	img := payload(1, 2048)
	req, _ := http.NewRequest("PUT", e.ts.URL+"/v1/images/img1", bytes.NewReader(img))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Image-SHA256", sum(img))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("put image = %d", resp.StatusCode)
	}
	if c := e.code(t, "GET", "/v1/images", tok, ""); c != 200 {
		t.Fatalf("list images = %d", c)
	}
	if c := e.code(t, "GET", "/v1/images/img1", tok, ""); c != 200 {
		t.Fatalf("get image = %d", c)
	}
	if c := e.code(t, "DELETE", "/v1/images/img1", tok, ""); c != 204 {
		t.Fatalf("delete image = %d", c)
	}

	// inventory: a host reports, the key reads it and delists the VM
	rep := reportBody(t, time.Now(), vm("vm1", phUUID1))
	if c := e.code(t, "PUT", "/v1/hosts/hv1/inventory", tokH, rep); c != 200 {
		t.Fatalf("host report = %d", c)
	}
	code, out := do(t, "GET", e.ts.URL+"/v1/inventory", "Bearer "+tok, "")
	if code.StatusCode != 200 || !strings.Contains(out, "vm1") {
		t.Fatalf("read inventory = %d %s", code.StatusCode, out)
	}
	if c := e.code(t, "POST", "/v1/inventory/hv1/vm1/delist", tok, ""); c != 200 {
		t.Fatalf("delist = %d", c)
	}
	if c := e.code(t, "GET", "/v1/networks", tok, ""); c != 200 {
		t.Fatalf("list networks = %d", c)
	}
}

func TestPulledKeyRemovedByNextPullIsRefused(t *testing.T) {
	l := newList(t)
	e := newKeyEnv(t, l)
	e.waitProvider(t)
	if c := e.code(t, "GET", "/v1/whoami", keyP1, ""); c != 200 {
		t.Fatalf("listed key = %d", c)
	}
	l.Remove("carol")
	e.pull(t)
	if c := e.code(t, "GET", "/v1/whoami", keyP1, ""); c != 401 {
		t.Fatalf("removed key = %d, want 401", c)
	}
}

func TestNewPulledKeyWorksOnFirstUse(t *testing.T) {
	l := newList(t)
	e := newKeyEnv(t, l)
	e.waitProvider(t)
	l.Add("dave", "full", keyP2)
	if c := e.code(t, "GET", "/v1/whoami", keyP2, ""); c != 200 {
		t.Fatalf("new key on first use = %d, want 200 (refresh on miss)", c)
	}
}

func TestWhoamiPulledKey(t *testing.T) {
	e := newKeyEnv(t, newList(t))
	e.waitProvider(t)
	resp, body := do(t, "GET", e.ts.URL+"/v1/whoami", "Bearer "+keyP1, "")
	var w storeapi.Whoami
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &w) != nil || w.Name != "carol" || w.Kind == nil || *w.Kind != storeapi.WhoamiKindUser {
		t.Fatalf("whoami = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(e.logs.String(), `"caller":"carol"`) {
		t.Errorf("the request log does not name the pulled key:\n%s", e.logs)
	}
}

// ---- local tokens win ----------------------------------------------------

func TestListedEntriesTakenLocallyAreSkipped(t *testing.T) {
	l := keyclienttest.NewList(t)
	l.Add("hv1", "full", keyP1)           // a host's name, a new hash
	l.Add("alice", "full", keyP2)         // a tokens.yaml caller's name, a new hash
	l.Add("renamed-host", "full", tokH)   // a host token's hash under another name
	l.Add("renamed-caller", "full", tokA) // a tokens.yaml hash under another name
	l.Add("shouty", "full", keyP3)        // control: accepted
	l.Add("reader", "readonly", keyOther) // an unknown role: skipped
	e := newKeyEnv(t, l)
	e.waitProvider(t)

	if n := e.s.keys.Load().Len(); n != 1 {
		t.Fatalf("%d entries live, want 1 (the control)", n)
	}
	for _, k := range []string{keyP1, keyP2, keyOther} {
		if c := e.code(t, "GET", "/v1/whoami", k, ""); c != 401 {
			t.Errorf("a skipped entry's key = %d, want 401", c)
		}
	}
	// The local tokens are what they were.
	whoami := func(tok string) string {
		_, body := do(t, "GET", e.ts.URL+"/v1/whoami", "Bearer "+tok, "")
		var w storeapi.Whoami
		_ = json.Unmarshal([]byte(body), &w)
		return w.Name
	}
	if n := whoami(tokA); n != "alice" {
		t.Errorf("tokA is %q", n)
	}
	if n := whoami(tokH); n != "host/hv1" {
		t.Errorf("tokH is %q", n)
	}
	if n := whoami(keyP3); n != "shouty" {
		t.Errorf("the control key is %q", n)
	}
}

func TestHostTokenOnNonHostRouteCausesNoPull(t *testing.T) {
	l := newList(t)
	e := newKeyEnv(t, l)
	e.waitProvider(t)
	// A pull from an unknown token does happen, so the check below can fail.
	before := l.Pulls()
	if c := e.code(t, "GET", "/v1/images", keyOther, ""); c != 401 {
		t.Fatalf("unknown token = %d", c)
	}
	if l.Pulls() <= before {
		t.Fatalf("an unknown token started no pull (%d, was %d); the control is void", l.Pulls(), before)
	}
	time.Sleep(10 * time.Millisecond) // past the refresh-on-miss window
	before = l.Pulls()
	for _, p := range []string{"/v1/images", "/v1/credentials/machines", "/v1/inventory", "/v1/personal-keys"} {
		if c := e.code(t, "GET", p, tokH, ""); c != 401 {
			t.Errorf("host token on %s = %d, want 401", p, c)
		}
	}
	if c := e.code(t, "GET", "/v1/whoami", tokH, ""); c != 200 {
		t.Errorf("host token on whoami = %d", c)
	}
	time.Sleep(50 * time.Millisecond) // a trailing pull would land here
	if got := l.Pulls(); got != before {
		t.Fatalf("a known host token caused %d pull(s)", got-before)
	}
}

// ---- start-up -------------------------------------------------------------

func TestStoreStartsWithListEndpointDown(t *testing.T) {
	l := newList(t)
	l.FailWith(keyclienttest.Status(500, ""))
	e := newKeyEnv(t, l)
	if c := e.code(t, "GET", "/v1/whoami", tokA, ""); c != 200 {
		t.Fatalf("tokens.yaml caller with the list down = %d", c)
	}
	if c := e.code(t, "GET", "/v1/whoami", keyP1, ""); c != 401 {
		t.Fatalf("pulled key with the list down and no cache = %d", c)
	}
	if !strings.Contains(e.logs.String(), "personal_keys: list origin=none entries=0 age=0s") {
		t.Errorf("no start-up line:\n%s", e.logs)
	}
	// It comes up when the endpoint does.
	l.Recover()
	e.pull(t)
	if c := e.code(t, "GET", "/v1/whoami", keyP1, ""); c != 200 {
		t.Fatalf("pulled key after the endpoint recovered = %d", c)
	}
}

func TestListStartedBeforeBind(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	l := newList(t)
	logs := &syncBuf{}
	c := &Config{
		Listen: taken.Addr().String(),
		Tokens: map[string]string{"alice": tokenhash.Hash(tokA)},
		PersonalKeys: keycfg.Settings{
			URL: l.URL(), CacheFile: filepath.Join(t.TempDir(), "keys.cache"), Refresh: time.Hour,
		},
	}
	s := NewServer(c, slog.New(slog.NewJSONHandler(logs, nil)), Deps{})
	if err := s.ListenAndServe(); err == nil {
		t.Fatal("bound a taken port")
	}
	if !strings.Contains(logs.String(), "personal_keys: list origin=") {
		t.Errorf("the list was not started before the bind:\n%s", logs)
	}
}

func TestUnsetURLBehavesAsBefore(t *testing.T) {
	e := newKeyEnv(t, nil)
	if e.s.keys.Load() != nil {
		t.Fatal("a key list with no url")
	}
	if c := e.code(t, "GET", "/v1/whoami", tokA, ""); c != 200 {
		t.Fatalf("tokens.yaml caller = %d", c)
	}
	if c := e.code(t, "GET", "/v1/whoami", keyP1, ""); c != 401 {
		t.Fatalf("unknown token = %d", c)
	}
	lines := strings.Split(strings.TrimSpace(e.logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want the 2 request lines:\n%s", len(lines), e.logs)
	}
	if strings.Contains(e.logs.String(), "personal_keys") {
		t.Errorf("the unset list logged:\n%s", e.logs)
	}
}

// ---- status and discovery ------------------------------------------------

func (e *keyEnv) status(t *testing.T, tok string) (int, storeapi.PersonalKeysStatus, string) {
	t.Helper()
	resp, body := do(t, "GET", e.ts.URL+"/v1/personal-keys", "Bearer "+tok, "")
	var st storeapi.PersonalKeysStatus
	if resp.StatusCode == 200 {
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatalf("status body %q: %v", body, err)
		}
	}
	return resp.StatusCode, st, body
}

func TestPersonalKeysStatusRoute(t *testing.T) {
	l := newList(t)
	l.Add("dave", "full", keyP2)
	e := newKeyEnv(t, l)
	e.waitProvider(t)

	// Counts: alice twice, bob never, a pulled key and a host token not at all.
	e.code(t, "GET", "/v1/whoami", tokA, "")
	e.code(t, "GET", "/v1/whoami", keyP1, "")
	e.code(t, "GET", "/v1/whoami", tokH, "")
	e.code(t, "GET", "/v1/whoami", keyOther, "")
	code, st, body := e.status(t, tokA) // the third request alice's token authenticates
	if code != 200 || !st.Enabled {
		t.Fatalf("status = %d %s", code, body)
	}
	if st.Origin == nil || *st.Origin != storeapi.Provider || st.Entries == nil || *st.Entries != 2 || st.AgeSeconds == nil || st.ListTime == nil || st.LastSuccess == nil {
		t.Errorf("list fields: %s", body)
	}
	if st.CacheFailing == nil || *st.CacheFailing || st.LastError != nil {
		t.Errorf("health fields: %s", body)
	}
	if len(st.LocalTokens) != 2 || st.LocalTokens[0].Name != "alice" || st.LocalTokens[1].Name != "bob" {
		t.Fatalf("local_tokens = %+v, want alice then bob", st.LocalTokens)
	}
	if a := st.LocalTokens[0]; a.Count != 2 || a.LastUsed == nil {
		t.Errorf("alice = %+v, want count 2 and a last_used", a)
	}
	if b := st.LocalTokens[1]; b.Count != 0 || b.LastUsed != nil {
		t.Errorf("bob = %+v, want count 0 and no last_used", b)
	}
	// A pulled key may read the status; it is counted nowhere.
	code, st, _ = e.status(t, keyP1)
	if code != 200 || st.LocalTokens[0].Count != 2 {
		t.Errorf("pulled key on status: %d %+v", code, st.LocalTokens)
	}
	// No hash, key, name list or URL.
	for _, leak := range []string{tokenhash.Hash(keyP1), tokenhash.Hash(tokA), keyP1, tokA, "carol", "dave", l.URL(), "keys.cache"} {
		if strings.Contains(body, leak) {
			t.Errorf("status body contains %q: %s", leak, body)
		}
	}
	// A host token is refused here, as everywhere outside its list.
	if c, _, _ := e.status(t, tokH); c != 401 {
		t.Errorf("host token on status = %d, want 401", c)
	}
	if c, _, _ := e.status(t, keyOther); c != 401 {
		t.Errorf("unknown token on status = %d, want 401", c)
	}
}

func TestPersonalKeysStatusListOff(t *testing.T) {
	e := newKeyEnv(t, nil)
	e.code(t, "GET", "/v1/whoami", tokB, "")
	code, st, body := e.status(t, tokA)
	if code != 200 || st.Enabled || st.Origin != nil || st.Entries != nil || st.LastError != nil {
		t.Fatalf("status with the list off = %d %s", code, body)
	}
	if len(st.LocalTokens) != 2 || st.LocalTokens[1].Name != "bob" || st.LocalTokens[1].Count != 1 || st.LocalTokens[0].Count != 1 {
		t.Errorf("local_tokens = %+v", st.LocalTokens)
	}
	if strings.Contains(body, `"local_tokens":null`) {
		t.Errorf("local_tokens is null: %s", body)
	}
}

func TestPersonalKeysStatusLastErrorCut(t *testing.T) {
	l := newList(t)
	l.FailWith(keyclienttest.Status(500, ""))
	e := newKeyEnv(t, l)
	deadline := time.Now().Add(5 * time.Second)
	for e.s.keys.Load().Status().LastError == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_, st, body := e.status(t, tokA)
	if st.LastError == nil || len([]rune(*st.LastError)) > 200 || st.Origin == nil || *st.Origin != storeapi.None {
		t.Fatalf("status = %s", body)
	}
	if st.ListTime != nil || st.AgeSeconds != nil || st.LastSuccess != nil {
		t.Errorf("age fields with no list: %s", body)
	}
}

func TestKeyRequestDiscovery(t *testing.T) {
	const ru = "https://keys.example.lab/v1/services/example/requests"
	e := newKeyEnv(t, nil, func(c *Config) { c.PersonalKeys.RequestURL = ru })
	resp, body := do(t, "GET", e.ts.URL+"/v1/key-request", "", "")
	var d storeapi.KeyRequestDiscovery
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &d) != nil || d.RequestUrl != ru {
		t.Fatalf("discovery = %d %s", resp.StatusCode, body)
	}
	off := newKeyEnv(t, nil)
	if resp, _ := do(t, "GET", off.ts.URL+"/v1/key-request", "", ""); resp.StatusCode != 404 {
		t.Fatalf("unset request_url = %d, want 404", resp.StatusCode)
	}
	// request_url alone does not turn the list on.
	if e.s.keys.Load() != nil {
		t.Error("request_url started a list")
	}
}

// ---- no secret in any log line -------------------------------------------

func TestPulledKeysNeverLogged(t *testing.T) {
	l := keyclienttest.NewList(t)
	l.RequireToken(pullToken)
	l.Add("carol", "full", keyP1)
	l.Add("alice", "full", keyP2) // skipped, logged by name
	tokFile := filepath.Join(t.TempDir(), "pull-token")
	if err := os.WriteFile(tokFile, []byte(pullToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newKeyEnv(t, l, func(c *Config) { c.PersonalKeys.TokenFile = tokFile })
	e.waitProvider(t)
	for _, k := range []string{tokA, tokH, keyP1, keyP2, keyOther} {
		e.code(t, "GET", "/v1/whoami", k, "")
	}
	l.Add("dave", "full", keyP3)
	e.code(t, "GET", "/v1/whoami", keyP3, "") // refresh on miss
	l.RequireToken("rotated-" + pullToken)    // the next pull fails
	_ = e.s.keys.Load().Pull(context.Background())
	e.code(t, "GET", "/v1/whoami", keyOther, "")
	_, _, body := e.status(t, tokA)

	out := e.logs.String() + body
	if !strings.Contains(out, `"caller":"carol"`) || !strings.Contains(out, "alice") {
		t.Fatalf("expected request lines and the skipped entry's line:\n%s", out)
	}
	for _, secret := range []string{
		tokA, tokB, tokH, keyP1, keyP2, keyP3, keyOther, pullToken, "rotated-" + pullToken,
		tokenhash.Hash(tokA), tokenhash.Hash(tokH), tokenhash.Hash(keyP1), tokenhash.Hash(keyP2), tokenhash.Hash(keyP3),
		"Authorization", "Bearer",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("a log line or the status body contains %q:\n%s", secret, out)
		}
	}
}

// ---- config ----------------------------------------------------------------

// A config file written for 1.4.0 has no personal_keys block and loads
// unchanged, with the list off.
func TestConfigWithoutPersonalKeysLoads(t *testing.T) {
	f := newFixture(t)
	f.hostTokens = "hv1: " + tokenhash.Hash(tokH) + "\n"
	f.extra = "bucket: placeholder-bucket\ns3_endpoint: 127.0.0.1:9000\npath_prefix: /vmstore\n"
	c := mustLoad(t, f)
	p := c.PersonalKeys
	if p.URL != "" || p.TokenFile != "" || p.CacheFile != "" || p.RequestURL != "" || p.AllowInsecureHTTP {
		t.Fatalf("personal_keys is not off: %+v", p)
	}
	if len(c.Tokens) != 2 || len(c.HostTokens) != 1 || c.PathPrefix != "/vmstore" {
		t.Errorf("the rest of the config changed: %+v", c)
	}
}

func TestConfigPersonalKeys(t *testing.T) {
	f := newFixture(t)
	f.extra = "personal_keys:\n  url: http://keys.internal:8080/keys/v1/lists/vmtool\n  allow_insecure_http: true\n  token_file: pull-token\n  cache_file: keys.cache\n  refresh: 30s\n  request_url: https://keys.example.lab/v1/services/example/requests\n"
	c := mustLoad(t, f)
	p := c.PersonalKeys
	if p.URL != "http://keys.internal:8080/keys/v1/lists/vmtool" || !p.AllowInsecureHTTP || p.TokenFile != "pull-token" || p.CacheFile != "keys.cache" ||
		p.Refresh != 30*time.Second || p.RefreshOnMiss != 5*time.Second || p.RequestURL == "" {
		t.Fatalf("personal_keys = %+v", p)
	}
	// refresh_on_miss: 0s is off, told from unset.
	f = newFixture(t)
	f.extra = "personal_keys:\n  url: https://keys.example.lab/keys/v1/lists/vmtool\n  cache_file: keys.cache\n  refresh_on_miss: 0s\n"
	if c := mustLoad(t, f); c.PersonalKeys.RefreshOnMiss != 0 || c.PersonalKeys.Refresh != time.Minute {
		t.Fatalf("personal_keys = %+v", c.PersonalKeys)
	}
	// request_url alone is fine and leaves the list off.
	f = newFixture(t)
	f.extra = "personal_keys:\n  request_url: https://keys.example.lab/v1/services/example/requests\n"
	if c := mustLoad(t, f); c.PersonalKeys.URL != "" || c.PersonalKeys.RequestURL == "" {
		t.Fatalf("personal_keys = %+v", c.PersonalKeys)
	}
}

func TestConfigPersonalKeysRefused(t *testing.T) {
	const url = "  url: https://keys.example.lab/keys/v1/lists/vmtool\n"
	for name, c := range map[string]struct{ extra, want string }{
		"no cache file":        {"personal_keys:\n" + url, "personal_keys.cache_file"},
		"cache without url":    {"personal_keys:\n  cache_file: keys.cache\n", "need personal_keys.url"},
		"insecure without url": {"personal_keys:\n  allow_insecure_http: true\n", "need personal_keys.url"},
		"plain http":           {"personal_keys:\n  url: http://keys.internal:8080/x\n  cache_file: keys.cache\n", "allow_insecure_http"},
		"credentials in url":   {"personal_keys:\n  url: https://u:PLACEHOLDER-pw@keys.example.lab/x\n  cache_file: keys.cache\n", "credentials"},
		"refresh zero":         {"personal_keys:\n" + url + "  cache_file: keys.cache\n  refresh: 0s\n", "personal_keys.refresh"},
		"bad request_url":      {"personal_keys:\n  request_url: http://keys.example.lab/r\n", "request_url"},
		"unknown key":          {"personal_keys:\n  nope: 1\n", "parse"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.extra = c.extra
			mustRefuse(t, f, c.want)
		})
	}
	// tokens.yaml stays required, with the list on.
	f := newFixture(t)
	f.tokens = "{}\n"
	f.extra = "personal_keys:\n" + url + "  cache_file: keys.cache\n"
	mustRefuse(t, f, "no token")
}
