package cli

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keyclienttest"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/storeclient"
)

// keyRig is a work dir with the operator's hash in config.yml, an empty key
// environment, a token file path in a temp dir and a fake key provider.
type keyRig struct {
	dir, tokenPath string
	prov           *keyclienttest.Provider
	localCalls     atomic.Int32
}

func newKeyRig(t *testing.T, cfgExtra string) *keyRig {
	t.Helper()
	t.Setenv(config.TokenEnv, "")
	t.Setenv(config.ServerTokenEnv, "")
	r := &keyRig{prov: keyclienttest.NewProvider(t)}
	r.prov.SetInterval(1)
	r.tokenPath = filepath.Join(t.TempDir(), "keys", "token")
	t.Setenv(config.TokenFileEnv, r.tokenPath)
	t.Setenv("VMTOOL_KEY_REQUEST_URL", r.prov.RequestURL())
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\n"+cfgExtra, "")
	terminal(t, false)
	// The local service accepts the provider's key and nothing else.
	old, oldDelay := localKeyCheck, keyRetryDelay
	keyRetryDelay = time.Millisecond
	localKeyCheck = func(_ context.Context, _ *config.Config, key string) (int, error) {
		r.localCalls.Add(1)
		if key == r.prov.Key() {
			return 200, nil
		}
		return 401, nil
	}
	t.Cleanup(func() { localKeyCheck, keyRetryDelay = old, oldDelay })
	return r
}

func terminal(t *testing.T, on bool) {
	old := keyIsTerminal
	keyIsTerminal = func() bool { return on }
	t.Cleanup(func() { keyIsTerminal = old })
}

func (r *keyRig) putKey(t *testing.T, key string, mode os.FileMode) {
	t.Helper()
	writeKeyFileT(t, r.tokenPath, key+"\n", mode)
}

func writeKeyFileT(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// tripwire runs args with every command stubbed: ran says which got through.
func (r *keyRig) tripwire(t *testing.T, args ...string) contractRun {
	t.Helper()
	return runContract(t, r.dir, "", args...)
}

func keyNoSecrets(t *testing.T, r *keyRig, c contractRun) {
	t.Helper()
	all := c.out + c.errOut + c.leak
	if c.err != nil {
		all += c.err.Error()
	}
	for _, s := range []string{r.prov.PollSecret(), r.prov.Key(), gateToken} {
		if s != "" && strings.Contains(all, s) {
			t.Errorf("output holds a secret: %q", all)
		}
	}
}

func errCode(t *testing.T, c contractRun) (code, msg string) {
	t.Helper()
	var obj struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	// A waiting run may print the notice first: the error object is the last line.
	lines := strings.Split(strings.TrimSpace(c.errOut), "\n")
	last := lines[len(lines)-1]
	if err := json.Unmarshal([]byte(last), &obj); err != nil || oneObject(last) != nil {
		t.Fatalf("stderr does not end in one error object: %v: %s", err, c.errOut)
	}
	return obj.Error.Code, obj.Error.Message
}

// --- the gate with a token-file key (offline order, check by the service)

func TestGateDotEnvWinsOverTokenFile(t *testing.T) {
	r := newKeyRig(t, "")
	r.putKey(t, r.prov.Key(), 0o600)
	writeKeyFileT(t, filepath.Join(r.dir, ".env"), "VMTOOL_TOKEN="+gateToken+"\n", 0o600)
	c := r.tripwire(t, "list")
	if c.err != nil || len(c.ran) != 1 {
		t.Fatalf("err %v ran %v", c.err, c.ran)
	}
	if r.localCalls.Load() != 0 {
		t.Error(".env key must be checked by token_sha256 alone, not by the service")
	}
}

func TestGateTokenFileKey(t *testing.T) {
	r := newKeyRig(t, "")
	r.putKey(t, r.prov.Key(), 0o600)
	c := r.tripwire(t, "list")
	if c.err != nil || len(c.ran) != 1 || r.localCalls.Load() != 1 {
		t.Fatalf("err %v ran %v calls %d", c.err, c.ran, r.localCalls.Load())
	}
	keyNoSecrets(t, r, c)
}

func TestGateTokenFileKeyRefused(t *testing.T) {
	r := newKeyRig(t, "")
	r.putKey(t, "some-other-key", 0o600)
	c := r.tripwire(t, "list")
	if len(c.ran) != 0 || !isGateErr(c.err) || c.code != 1 {
		t.Fatalf("err %v ran %v", c.err, c.ran)
	}
	for _, w := range []string{r.tokenPath, "vmtool key status"} {
		if !strings.Contains(c.err.Error(), w) {
			t.Errorf("message lacks %q: %v", w, c.err)
		}
	}
	if strings.Contains(c.err.Error(), "some-other-key") {
		t.Error("message holds the key")
	}
}

func TestGateTokenFileServiceDown(t *testing.T) {
	r := newKeyRig(t, "")
	r.putKey(t, r.prov.Key(), 0o600)
	localKeyCheck = func(context.Context, *config.Config, string) (int, error) { return 0, fmt.Errorf("refused") }
	c := r.tripwire(t, "list")
	want := "vmtool.service is not answering at 127.0.0.1:9473; start it: sudo systemctl start vmtool"
	if len(c.ran) != 0 || c.err == nil || !strings.Contains(c.err.Error(), want) {
		t.Fatalf("err %v ran %v", c.err, c.ran)
	}
}

func TestGateTokenFileBadMode(t *testing.T) {
	r := newKeyRig(t, "")
	r.putKey(t, r.prov.Key(), 0o644)
	c := r.tripwire(t, "list")
	if len(c.ran) != 0 || !isGateErr(c.err) || !strings.Contains(c.err.Error(), r.tokenPath) || r.prov.Key() != "" && strings.Contains(c.err.Error(), r.prov.Key()) {
		t.Fatalf("err %v ran %v", c.err, c.ran)
	}
	if len(r.prov.Filed()) != 0 {
		t.Error("a refused file must not trigger a request")
	}
	// A symlink is refused too.
	os.Remove(r.tokenPath)
	real := filepath.Join(t.TempDir(), "real")
	writeKeyFileT(t, real, r.prov.Key(), 0o600)
	if err := os.Symlink(real, r.tokenPath); err != nil {
		t.Fatal(err)
	}
	c = r.tripwire(t, "list")
	if len(c.ran) != 0 || !isGateErr(c.err) {
		t.Fatalf("symlink: err %v ran %v", c.err, c.ran)
	}
}

// A child process gets the file key as VMTOOL_TOKEN; its gate accepts it.
func TestTunnelChildCarriesFileKey(t *testing.T) {
	r := newKeyRig(t, "")
	r.putKey(t, r.prov.Key(), 0o600)
	c := r.tripwire(t, "list")
	if c.err != nil {
		t.Fatal(c.err)
	}
	e := &env{dir: r.dir}
	root := newRootCmd(e)
	root.SetArgs([]string{"config", "host", "x"})
	if err := e.gate(root); err != nil {
		t.Fatal(err)
	}
	e.tunnelExe = "/x/vmtool"
	tun, _ := e.tunnel()
	got := strings.Join(tun.Environ([]string{"VMTOOL_SERVER_TOKEN=s", "VMTOOL_TOKEN=old"}), "\n")
	if !strings.Contains(got, "VMTOOL_TOKEN="+r.prov.Key()) || strings.Contains(got, "VMTOOL_SERVER_TOKEN") || strings.Contains(got, "=old") {
		t.Errorf("child env %q", got)
	}
	// The child's own gate sees VMTOOL_TOKEN = the file's key and still passes.
	t.Setenv(config.TokenEnv, r.prov.Key())
	c = r.tripwire(t, "list")
	if c.err != nil || len(c.ran) != 1 {
		t.Fatalf("child gate: err %v ran %v", c.err, c.ran)
	}
}

// --- the first-use request

func TestKeyRequestTerminalEndToEnd(t *testing.T) {
	r := newKeyRig(t, "")
	terminal(t, true)
	r.prov.Script(keyclienttest.Pending(), keyclienttest.Approved())
	c := r.tripwire(t, "list")
	if c.err != nil || len(c.ran) != 1 {
		t.Fatalf("err %v ran %v stderr %q", c.err, c.ran, c.errOut)
	}
	b, err := os.ReadFile(r.tokenPath)
	if err != nil || strings.TrimSpace(string(b)) != r.prov.Key() {
		t.Fatalf("token file: %v", err)
	}
	if fi, _ := os.Stat(r.tokenPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if len(r.prov.Filed()) != 1 || r.localCalls.Load() != 1 {
		t.Errorf("filed %d, service checks %d", len(r.prov.Filed()), r.localCalls.Load())
	}
	if !strings.Contains(strings.ToLower(c.errOut), "approve") {
		t.Errorf("no notice on stderr: %q", c.errOut)
	}
	keyNoSecrets(t, r, c)
	// The next run uses the file and files nothing.
	c = r.tripwire(t, "list")
	if c.err != nil || len(r.prov.Filed()) != 1 {
		t.Fatalf("second run: %v, filed %d", c.err, len(r.prov.Filed()))
	}
}

func TestKeyRequestFreshKeyRetriedOnce(t *testing.T) {
	r := newKeyRig(t, "")
	terminal(t, true)
	r.prov.Script(keyclienttest.Approved())
	var n atomic.Int32
	localKeyCheck = func(context.Context, *config.Config, string) (int, error) {
		if n.Add(1) == 1 {
			return 401, nil
		}
		return 200, nil
	}
	c := r.tripwire(t, "list")
	if c.err != nil || n.Load() != 2 {
		t.Fatalf("err %v, checks %d", c.err, n.Load())
	}
	// A key already in the file is not retried.
	n.Store(0)
	localKeyCheck = func(context.Context, *config.Config, string) (int, error) { n.Add(1); return 401, nil }
	c = r.tripwire(t, "list")
	if c.err == nil || n.Load() != 1 {
		t.Fatalf("err %v, checks %d", c.err, n.Load())
	}
}

func TestKeyRequestDeniedExpired(t *testing.T) {
	for _, tc := range []struct {
		code string
		out  keyclienttest.Outcome
	}{
		{"key_denied", keyclienttest.Denied()},
		{"key_expired", keyclienttest.Expired()},
		{"key_consumed", keyclienttest.Consumed()},
	} {
		t.Run(tc.code, func(t *testing.T) {
			r := newKeyRig(t, "")
			terminal(t, true)
			r.prov.Script(tc.out)
			c := r.tripwire(t, "--json", "list")
			if c.code != 1 || len(c.ran) != 0 || c.out != "" {
				t.Fatalf("exit %d ran %v", c.code, c.ran)
			}
			if code, _ := errCode(t, c); code != tc.code {
				t.Errorf("code %q", code)
			}
			keyNoSecrets(t, r, c)
		})
	}
}

func TestKeyRequestNotTerminalPendingThenResumes(t *testing.T) {
	r := newKeyRig(t, "")
	r.prov.Script(keyclienttest.Pending(), keyclienttest.Pending(), keyclienttest.Pending())
	c := r.tripwire(t, "--json", "list")
	if c.code != 75 || len(c.ran) != 0 || c.out != "" {
		t.Fatalf("exit %d ran %v out %q err %q", c.code, c.ran, c.out, c.errOut)
	}
	code, msg := errCode(t, c)
	if code != "key_pending" || !strings.Contains(msg, "key request pending; run this again after approving") || !strings.Contains(strings.ToLower(msg), "approve") {
		t.Errorf("code %q message %q", code, msg)
	}
	keyNoSecrets(t, r, c)
	// Human mode: the same lines, exit 75.
	c = r.tripwire(t, "list")
	if c.code != 75 || !strings.Contains(c.errOut, "key request pending; run this again after approving") {
		t.Errorf("human: exit %d stderr %q", c.code, c.errOut)
	}
	// After approval a later run collects the key and goes on: still one filing.
	r.prov.Script(keyclienttest.Approved())
	deadline := time.Now().Add(8 * time.Second)
	for {
		c = r.tripwire(t, "list")
		if c.code == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if c.code != 0 || len(c.ran) != 1 {
		t.Fatalf("after approval: exit %d err %v stderr %q", c.code, c.err, c.errOut)
	}
	if n := len(r.prov.Filed()); n != 1 {
		t.Fatalf("filed %d requests, want exactly 1", n)
	}
	keyNoSecrets(t, r, c)
}

func TestKeyRequestOff(t *testing.T) {
	r := newKeyRig(t, "")
	c := r.tripwire(t, "--no-key-request", "list")
	if c.err == nil || !isGateErr(c.err) || !strings.Contains(c.err.Error(), "no VMTOOL_TOKEN in the environment or") || len(r.prov.Filed()) != 0 {
		t.Fatalf("err %v filed %d", c.err, len(r.prov.Filed()))
	}
	// A VMTOOL_TOKEN with no store token never files a request.
	t.Setenv(config.TokenEnv, gateToken)
	c = r.tripwire(t, "list")
	if c.err != nil || len(r.prov.Filed()) != 0 {
		t.Fatalf("err %v filed %d", c.err, len(r.prov.Filed()))
	}
}

func TestKeyRequestNoProviderKeepsTodaysText(t *testing.T) {
	r := newKeyRig(t, "")
	t.Setenv("VMTOOL_KEY_REQUEST_URL", "")
	c := r.tripwire(t, "list")
	want := "vmtool: no VMTOOL_TOKEN in the environment or " + r.dir + "/.env; run 'just token' (new) or 'just token-fetch' (from OpenBao)"
	if c.err == nil || c.err.Error() != want || c.code != 1 {
		t.Fatalf("got %v", c.err)
	}
	// --key-request-url is the same as the variable.
	t.Setenv("VMTOOL_KEY_REQUEST_URL", "")
	terminal(t, true)
	r.prov.Script(keyclienttest.Approved())
	c = r.tripwire(t, "--key-request-url", r.prov.RequestURL(), "list")
	if c.err != nil || len(c.ran) != 1 {
		t.Fatalf("flag: %v", c.err)
	}
}

// --- vmtool key request / status / check

func TestKeyRequestCommand(t *testing.T) {
	r := newKeyRig(t, "")
	// No config.yml needed when the URL is given.
	os.Remove(filepath.Join(r.dir, config.FileName))
	r.prov.Script(keyclienttest.Approved())
	c := runContract(t, r.dir, "vmtool key request", "--json", "key", "request")
	if c.code != 75 {
		t.Fatalf("not a terminal: exit %d err %q", c.code, c.errOut)
	}
	if code, _ := errCode(t, c); code != "key_pending" {
		t.Errorf("code %q", code)
	}
	terminal(t, true)
	var out struct {
		TokenFile          string `json:"token_file"`
		Result, Name, Role string
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		c = runContract(t, r.dir, "vmtool key request", "--json", "key", "request", "--role", "full")
		if c.code == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if c.code != 0 {
		t.Fatalf("exit %d: %s", c.code, c.errOut)
	}
	if err := json.Unmarshal([]byte(c.out), &out); err != nil || oneObject(c.out) != nil {
		t.Fatal(err, c.out)
	}
	if out.TokenFile != r.tokenPath || (out.Result != "stored" && out.Result != "present") {
		t.Errorf("%+v", out)
	}
	if f := r.prov.Filed(); len(f) != 1 || f[0].Role != "full" && f[0].Role != "" {
		t.Errorf("filed %+v", f)
	}
	keyNoSecrets(t, r, c)
	c = runContract(t, r.dir, "vmtool key request", "key", "request")
	if c.code != 0 || !strings.Contains(c.out, "a key is already at "+r.tokenPath) {
		t.Errorf("present: exit %d out %q", c.code, c.out)
	}
	c = runContract(t, r.dir, "vmtool key request", "--json", "key", "request")
	if !strings.Contains(c.out, `"present"`) {
		t.Errorf("present json: %q", c.out)
	}
}

func TestKeyStatus(t *testing.T) {
	r := newKeyRig(t, "")
	type status struct {
		TokenFile    string `json:"token_file"`
		TokenFileSt  string `json:"token_file_state"`
		DaemonSource string `json:"daemon_source"`
		StoreSource  string `json:"store_source"`
		Pending      *struct {
			ApproveURL string `json:"approve_url"`
			Code       string `json:"code"`
			ExpiresAt  string `json:"expires_at"`
		} `json:"pending"`
	}
	get := func() status {
		c := runContract(t, r.dir, "vmtool key status", "--json", "key", "status")
		if c.code != 0 || oneObject(c.out) != nil {
			t.Fatalf("exit %d out %q err %q", c.code, c.out, c.errOut)
		}
		var s status
		if err := json.Unmarshal([]byte(c.out), &s); err != nil {
			t.Fatal(err)
		}
		keyNoSecrets(t, r, c)
		return s
	}
	if s := get(); s.TokenFile != r.tokenPath || s.TokenFileSt != "absent" || s.DaemonSource != "none" || s.StoreSource != "none" || s.Pending != nil {
		t.Errorf("empty: %+v", s)
	}
	r.putKey(t, r.prov.Key(), 0o600)
	if s := get(); s.TokenFileSt != "present" || s.DaemonSource != "file" || s.StoreSource != "file" {
		t.Errorf("file: %+v", s)
	}
	t.Setenv(config.ServerTokenEnv, "s")
	writeKeyFileT(t, filepath.Join(r.dir, ".env"), "VMTOOL_TOKEN="+gateToken+"\n", 0o600)
	if s := get(); s.DaemonSource != ".env" || s.StoreSource != "env" {
		t.Errorf("order: %+v", s)
	}
	os.Chmod(r.tokenPath, 0o644)
	if s := get(); s.TokenFileSt != "refused" {
		t.Errorf("refused: %+v", s)
	}
	// A pending request is shown, read-only: nothing is filed by status.
	os.Remove(r.tokenPath)
	os.Remove(filepath.Join(r.dir, ".env"))
	t.Setenv(config.ServerTokenEnv, "")
	if c := r.tripwire(t, "list"); c.code != 75 {
		t.Fatalf("exit %d", c.code)
	}
	s := get()
	if s.Pending == nil || s.Pending.ApproveURL == "" || s.Pending.Code == "" || s.Pending.ExpiresAt == "" {
		t.Errorf("pending: %+v", s.Pending)
	}
	if len(r.prov.Filed()) != 1 {
		t.Errorf("filed %d", len(r.prov.Filed()))
	}
}

// fakeTLS makes a TLS server answering by path; returns it and its pool.
func fakeTLS(t *testing.T, good string, h func(path string) (int, string)) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+good {
			w.WriteHeader(401)
			return
		}
		code, body := h(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return ts, pool
}

type checkRow struct {
	Target, Kind, Result, Detail string
}

func TestKeyCheckRows(t *testing.T) {
	r := newKeyRig(t, "")
	key := r.prov.Key()
	local, pool := fakeTLS(t, key, func(p string) (int, string) {
		if p == "/v1/personal-keys" || p == "/v1/hosts/hv1/pools" {
			return 200, "{}"
		}
		if p == "/v1/hosts/hv2/pools" {
			return 502, "{}" // forwarded, the host is not answering: not a verdict on the key
		}
		return 404, ""
	})
	hv1, _ := fakeTLS(t, key, func(p string) (int, string) { return 200, "{}" })
	hv2, _ := fakeTLS(t, "another-key", func(p string) (int, string) { return 200, "{}" })
	hv3, _ := fakeTLS(t, key, func(p string) (int, string) { return 404, "" }) // old daemon: no route, after auth
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	dead.Close()
	store, _ := fakeTLS(t, key, func(p string) (int, string) { return 200, `{"name":"alice@laptop"}` })

	certPath := filepath.Join(t.TempDir(), "local.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: local.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	hostLine := func(n string, ts *httptest.Server) string {
		u, _ := url.Parse(ts.URL)
		h, p, _ := net.SplitHostPort(u.Host)
		return fmt.Sprintf("  %s: { address: %s, ssh_user: u, port: %s }\n", n, h, p)
	}
	lu, _ := url.Parse(local.URL)
	cfg := "listen: " + lu.Host + "\ntls_cert: " + certPath + "\nhosts:\n" + hostLine("hv1", hv1) + hostLine("hv2", hv2) + hostLine("hv3", hv3) + hostLine("hv4", dead) +
		"server:\n  url: " + store.URL + "\n"
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\n"+cfg, "")
	oldR, oldO := keyProbeRoots, storeClientOptions
	keyProbeRoots = pool
	storeClientOptions = []storeclient.Option{storeclient.WithRootCAs(pool)}
	t.Cleanup(func() { keyProbeRoots, storeClientOptions = oldR, oldO })
	// The real local check, against the fake service.
	localKeyCheck = realLocalKeyCheck

	// No key file: an error, and no request is filed.
	c := runContract(t, r.dir, "vmtool key check", "--json", "key", "check")
	if c.code != 1 || len(r.prov.Filed()) != 0 {
		t.Fatalf("no file: exit %d", c.code)
	}
	r.putKey(t, key, 0o600)
	// Even with other tokens in .env the file's key is what is presented.
	writeKeyFileT(t, filepath.Join(r.dir, ".env"), "VMTOOL_TOKEN="+gateToken+"\nVMTOOL_SERVER_TOKEN=zzz\n", 0o600)
	c = runContract(t, r.dir, "vmtool key check", "--json", "key", "check")
	var out struct {
		TokenFile string     `json:"token_file"`
		OK        bool       `json:"ok"`
		Targets   []checkRow `json:"targets"`
	}
	if err := json.Unmarshal([]byte(c.out), &out); err != nil || oneObject(c.out) != nil {
		t.Fatalf("%v: %q / %q", err, c.out, c.errOut)
	}
	if c.code != 1 || out.OK || out.TokenFile != r.tokenPath {
		t.Errorf("exit %d ok %v", c.code, out.OK)
	}
	got := map[string]checkRow{}
	for _, row := range out.Targets {
		got[row.Kind+":"+row.Target] = row
	}
	want := map[string]string{
		"local:" + lu.Host: "accepted", "direct:hv1": "accepted", "direct:hv2": "refused", "direct:hv3": "accepted",
		"direct:hv4": "unreachable", "forwarded:hv1": "accepted", "forwarded:hv2": "unreachable",
		"store:" + store.URL: "accepted",
	}
	for k, res := range want {
		row, ok := got[k]
		if !ok {
			// the store target is named by its host
			for gk, gr := range got {
				if strings.HasPrefix(k, "store:") && strings.HasPrefix(gk, "store:") {
					row, ok = gr, true
				}
			}
		}
		if !ok || row.Result != res {
			t.Errorf("%s: %+v (want %s); rows %+v", k, row, res, out.Targets)
		}
	}
	for _, row := range out.Targets {
		if row.Kind == "store" && !strings.Contains(row.Detail, "alice@laptop") {
			t.Errorf("store row lacks the name: %+v", row)
		}
	}
	keyNoSecrets(t, r, c)
	if strings.Contains(c.out+c.errOut, "zzz") {
		t.Error("store token leaked")
	}
	// Human mode lists the rows too and exits 1.
	c = runContract(t, r.dir, "vmtool key check", "key", "check")
	if c.code != 1 || !strings.Contains(c.out, "refused") {
		t.Errorf("human: exit %d out %q", c.code, c.out)
	}
}

func TestKeyCheckAllAcceptedExitsZero(t *testing.T) {
	r := newKeyRig(t, "")
	key := r.prov.Key()
	local, pool := fakeTLS(t, key, func(p string) (int, string) { return 200, "{}" })
	certPath := filepath.Join(t.TempDir(), "local.pem")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: local.Certificate().Raw}), 0o600)
	lu, _ := url.Parse(local.URL)
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: "+lu.Host+"\ntls_cert: "+certPath+"\n", "")
	oldR := keyProbeRoots
	keyProbeRoots = pool
	t.Cleanup(func() { keyProbeRoots = oldR })
	localKeyCheck = realLocalKeyCheck
	r.putKey(t, key, 0o600)
	c := runContract(t, r.dir, "vmtool key check", "--json", "key", "check")
	if c.code != 0 || !strings.Contains(c.out, `"ok": true`) {
		t.Fatalf("exit %d out %q err %q", c.code, c.out, c.errOut)
	}
	// The local service refusing the key gives exit 1.
	r.putKey(t, "wrong", 0o600)
	c = runContract(t, r.dir, "vmtool key check", "--json", "key", "check")
	if c.code != 1 || !strings.Contains(c.out, `"refused"`) {
		t.Fatalf("refused: exit %d out %q", c.code, c.out)
	}
}

// An unreachable target (a host that is asleep) does not fail the check when
// every reachable one accepted; with nothing reachable at all it fails.
func TestKeyCheckUnreachableDoesNotFail(t *testing.T) {
	r := newKeyRig(t, "")
	key := r.prov.Key()
	local, pool := fakeTLS(t, key, func(p string) (int, string) { return 200, "{}" })
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	dead.Close()
	certPath := filepath.Join(t.TempDir(), "local.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: local.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	du, _ := url.Parse(dead.URL)
	dh, dp, _ := net.SplitHostPort(du.Host)
	lu, _ := url.Parse(local.URL)
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: "+lu.Host+"\ntls_cert: "+certPath+
		"\nhosts:\n  hv9: { address: "+dh+", ssh_user: u, port: "+dp+" }\n", "")
	oldR := keyProbeRoots
	keyProbeRoots = pool
	t.Cleanup(func() { keyProbeRoots = oldR })
	localKeyCheck = realLocalKeyCheck
	r.putKey(t, key, 0o600)
	c := runContract(t, r.dir, "vmtool key check", "--json", "key", "check")
	if c.code != 0 || !strings.Contains(c.out, `"ok": true`) || !strings.Contains(c.out, `"unreachable"`) {
		t.Fatalf("one unreachable host: exit %d out %q err %q", c.code, c.out, c.errOut)
	}
	// Nothing reachable: the local service is down too.
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: "+du.Host+"\ntls_cert: "+certPath+"\n", "")
	c = runContract(t, r.dir, "vmtool key check", "--json", "key", "check")
	if c.code != 1 || !strings.Contains(c.out, `"ok": false`) {
		t.Fatalf("nothing reachable: exit %d out %q", c.code, c.out)
	}
}

// config write never takes the token file's key: a personal key must not
// become token_sha256.
func TestConfigWriteIgnoresTokenFile(t *testing.T) {
	r := newKeyRig(t, "")
	r.putKey(t, r.prov.Key(), 0o600)
	dir := writeWorkdir(t, "", "")
	c := runContract(t, dir, "vmtool config write", "config", "write", "--name", "ws")
	if c.err == nil || !strings.Contains(c.err.Error(), "VMTOOL_TOKEN") {
		t.Fatalf("err %v", c.err)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		t.Error("config write wrote config.yml")
	}
	if len(r.prov.Filed()) != 0 {
		t.Error("config write filed a request")
	}
}

// A store command uses the store key in order: .env wins over the token file;
// with only the file, the file's key is the store's.
func TestStoreKeyOrderDotEnvWinsOverFile(t *testing.T) {
	s := newCLIStore(t)
	s.inv = inventoryBody()
	t.Setenv(config.TokenEnv, gateToken)
	t.Setenv(config.ServerTokenEnv, "")
	t.Chdir(t.TempDir())
	tf := filepath.Join(t.TempDir(), "token")
	t.Setenv(config.TokenFileEnv, tf)
	body := "name: ws\ntoken_sha256: " + config.Hash(gateToken) + "\n" + s.cfg()

	writeKeyFileT(t, tf, "not-the-store-key\n", 0o600)
	dir := writeWorkdir(t, body, "VMTOOL_SERVER_TOKEN="+storeTok+"\n")
	if c := runContract(t, dir, "vmtool inventory", "--json", "inventory"); c.code != 0 {
		t.Fatalf(".env key: exit %d: %s", c.code, c.errOut)
	}
	writeKeyFileT(t, tf, storeTok+"\n", 0o600)
	dir = writeWorkdir(t, body, "VMTOOL_SERVER_TOKEN=wrong-dotenv-key\n")
	if c := runContract(t, dir, "vmtool inventory", "--json", "inventory"); c.code == 0 {
		t.Fatal("the wrong .env key must win over the right file key")
	}
	dir = writeWorkdir(t, body, "")
	if c := runContract(t, dir, "vmtool inventory", "--json", "inventory"); c.code != 0 {
		t.Fatalf("file key: exit %d: %s", c.code, c.errOut)
	}
}
