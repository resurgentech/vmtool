package cli

import (
	"encoding/pem"
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

	"github.com/resurgentech/vmtool/internal/config"
)

// localSvc is a TLS listener on 127.0.0.1 that accepts exactly good at
// /v1/personal-keys and counts every request it gets.
func localSvc(t *testing.T, good string) (listen, certPath string, reqs *atomic.Int32) {
	t.Helper()
	reqs = new(atomic.Int32)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+good {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(ts.Close)
	certPath = filepath.Join(t.TempDir(), "svc.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(ts.URL)
	return u.Host, certPath, reqs
}

func realCheck(t *testing.T) {
	t.Helper()
	old := localKeyCheck
	localKeyCheck = realLocalKeyCheck
	t.Cleanup(func() { localKeyCheck = old })
}

func thisMachine(t *testing.T, ips ...string) {
	t.Helper()
	old := machineIPs
	machineIPs = func() []net.IP {
		var out []net.IP
		for _, s := range ips {
			out = append(out, net.ParseIP(s))
		}
		return out
	}
	t.Cleanup(func() { machineIPs = old })
}

func TestIsThisMachine(t *testing.T) {
	thisMachine(t, "198.51.100.7", "2001:db8::7")
	for _, ok := range []string{"127.0.0.1", "127.9.9.9", "::1", "198.51.100.7", "2001:db8::7"} {
		if !isThisMachine(net.ParseIP(ok)) {
			t.Errorf("%s must be allowed", ok)
		}
	}
	for _, no := range []string{"192.0.2.10", "198.51.100.8", "2001:db8::8", "10.0.0.1"} {
		if isThisMachine(net.ParseIP(no)) {
			t.Errorf("%s must be refused", no)
		}
	}
}

// The gate's check never dials an address that is not this machine's, for a
// token-file key or a VMTOOL_TOKEN, and says why.
func TestGateKeyNotSentOffMachine(t *testing.T) {
	for _, from := range []string{"file", "env"} {
		t.Run(from, func(t *testing.T) {
			r := newKeyRig(t, "")
			realCheck(t)
			thisMachine(t) // nothing but loopback
			key := r.prov.Key()
			r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: 192.0.2.10:9473\n", "")
			if from == "file" {
				r.putKey(t, key, 0o600)
			} else {
				t.Setenv(config.TokenEnv, key)
			}
			start := time.Now()
			c := r.tripwire(t, "list")
			if d := time.Since(start); d > time.Second {
				t.Errorf("refusal took %v: a dial was attempted", d)
			}
			if len(c.ran) != 0 || !isGateErr(c.err) || c.code != 1 {
				t.Fatalf("err %v ran %v code %d", c.err, c.ran, c.code)
			}
			msg := c.err.Error()
			for _, w := range []string{"not sent", "192.0.2.10", "not an address of this machine"} {
				if !strings.Contains(msg, w) {
					t.Errorf("message lacks %q: %s", w, msg)
				}
			}
			if strings.Contains(msg, "not answering") || strings.Contains(msg, key) {
				t.Errorf("bad message: %s", msg)
			}
		})
	}
}

// A real TLS-less listener on loopback named by config.yml, with the "this
// machine" decision forced to refuse it: the listener gets no connection and
// the error says why.
func TestGateKeyListenerNeverReached(t *testing.T) {
	r := newKeyRig(t, "")
	realCheck(t)
	thisMachine(t)
	key := r.prov.Key()
	reqs := new(atomic.Int32)
	conns := new(atomic.Int32)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			reqs.Add(1)
			c.Close()
		}
	}()
	old := allowKeyCheckIP
	allowKeyCheckIP = func(net.IP) bool { return false }
	t.Cleanup(func() { allowKeyCheckIP = old })
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: "+ln.Addr().String()+"\n", "")
	r.putKey(t, key, 0o600)
	c := r.tripwire(t, "list")
	if len(c.ran) != 0 || c.err == nil || !strings.Contains(c.err.Error(), "not an address of this machine") {
		t.Fatalf("err %v ran %v", c.err, c.ran)
	}
	time.Sleep(50 * time.Millisecond)
	if conns.Load() != 0 {
		t.Errorf("the listener got %d connections", conns.Load())
	}
}

// A VMTOOL_TOKEN that is not the operator's is checked by the local service:
// accepted when it knows the key, refused (unauthorized, token_sha256 text)
// when it does not, and with a clear message when the service is down.
func TestGateEnvKeyChecked(t *testing.T) {
	r := newKeyRig(t, "")
	realCheck(t)
	key := r.prov.Key()
	listen, cert, reqs := localSvc(t, key)
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: "+listen+"\ntls_cert: "+cert+"\n", "")

	t.Setenv(config.TokenEnv, key)
	c := r.tripwire(t, "list")
	if c.err != nil || len(c.ran) != 1 || reqs.Load() != 1 {
		t.Fatalf("pulled key: err %v ran %v requests %d", c.err, c.ran, reqs.Load())
	}
	keyNoSecrets(t, r, c)

	t.Setenv(config.TokenEnv, "not-listed-key")
	c = r.tripwire(t, "list")
	if len(c.ran) != 0 || !isGateErr(c.err) || c.code != 1 {
		t.Fatalf("unlisted: err %v ran %v", c.err, c.ran)
	}
	for _, w := range []string{"VMTOOL_TOKEN does not match token_sha256", "local service did not accept it", "just token-fetch"} {
		if !strings.Contains(c.err.Error(), w) {
			t.Errorf("message lacks %q: %v", w, c.err)
		}
	}
	if strings.Contains(c.err.Error(), "not-listed-key") {
		t.Error("message holds the key")
	}

	// The operator token still passes offline: the service is not asked.
	before := reqs.Load()
	t.Setenv(config.TokenEnv, gateToken)
	c = r.tripwire(t, "list")
	if c.err != nil || len(c.ran) != 1 || reqs.Load() != before {
		t.Fatalf("operator: err %v ran %v requests %d->%d", c.err, c.ran, before, reqs.Load())
	}
}

func TestGateEnvKeyServiceDown(t *testing.T) {
	r := newKeyRig(t, "")
	realCheck(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: "+addr+"\n", "")
	t.Setenv(config.TokenEnv, r.prov.Key())
	c := r.tripwire(t, "list")
	if len(c.ran) != 0 || !isGateErr(c.err) {
		t.Fatalf("err %v ran %v", c.err, c.ran)
	}
	for _, w := range []string{"VMTOOL_TOKEN does not match token_sha256", "vmtool.service is not answering at " + addr, "personal key", "systemctl start vmtool"} {
		if !strings.Contains(c.err.Error(), w) {
			t.Errorf("message lacks %q: %v", w, c.err)
		}
	}
}

// config write never makes the personal key in the token file the operator
// hash, whichever way the token arrives; a different token still writes.
func TestConfigWriteRefusesTokenFileKey(t *testing.T) {
	run := func(t *testing.T, r *keyRig, dir, stdin string, args ...string) (error, string) {
		t.Helper()
		var out, errb strings.Builder
		root, _ := rootFor(dir, &out, &errb)
		root.SetIn(strings.NewReader(stdin))
		root.SetArgs(append([]string{"config", "write", "--name", "ws"}, args...))
		err := root.Execute()
		return err, out.String() + errb.String()
	}
	for _, mode := range []os.FileMode{0o600, 0o644} {
		for _, via := range []string{"env", ".env", "stdin"} {
			t.Run(via+"/"+mode.String(), func(t *testing.T) {
				r := newKeyRig(t, "")
				key := r.prov.Key()
				r.putKey(t, key, mode)
				dir := t.TempDir()
				var stdin string
				var args []string
				switch via {
				case "env":
					t.Setenv(config.TokenEnv, key)
				case ".env":
					if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("VMTOOL_TOKEN="+key+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "stdin":
					stdin, args = key+"\n", []string{"--token-stdin"}
				}
				err, out := run(t, r, dir, stdin, args...)
				if err == nil {
					t.Fatal("config write accepted the token file's key")
				}
				for _, w := range []string{"personal key", "token file", "token_sha256", "--token-stdin"} {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("message lacks %q: %v", w, err)
					}
				}
				if strings.Contains(err.Error()+out, key) {
					t.Error("output holds the key")
				}
				if _, serr := os.Stat(filepath.Join(dir, config.FileName)); serr == nil {
					t.Error("config.yml was written")
				}
			})
		}
	}
	t.Run("another token writes", func(t *testing.T) {
		r := newKeyRig(t, "")
		r.putKey(t, r.prov.Key(), 0o600)
		dir := t.TempDir()
		t.Setenv(config.TokenEnv, gateToken)
		if err, out := run(t, r, dir, ""); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		b, err := os.ReadFile(filepath.Join(dir, config.FileName))
		if err != nil || !strings.Contains(string(b), config.Hash(gateToken)) {
			t.Fatalf("config.yml: %v %q", err, b)
		}
	})
}

// --no-personal-keys drops the whole personal_keys block (every key in it)
// before the other flags apply, and keeps everything else in config.yml.
func TestConfigWriteNoPersonalKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "PLACEHOLDER-not-a-secret-1")
	body := "hosts:\n  hv1: { address: 192.0.2.11, ssh_user: u }\npersonal_keys:\n  url: https://keys.example.lab/l\n  cache_file: c.json\n  token_file: t\n  refresh: 30s\n  request_url: https://keys.example.lab/r\n"
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Alone: the block goes, the hosts stay.
	if _, err := runConfig(t, dir, "", "write", "--no-personal-keys"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, config.FileName))
	if strings.Contains(string(b), "personal_keys") || strings.Contains(string(b), "refresh") || !strings.Contains(string(b), "hv1") {
		t.Errorf("config.yml after clear:\n%s", b)
	}
	// With a request URL: cleared first, then set; nothing else of the block remains.
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, dir, "", "write", "--no-personal-keys", "--key-request-url", "https://keys.example.lab/r2"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := c.PersonalKeys
	if p.URL != "" || p.CacheFile != "" || p.TokenFile != "" || p.RequestURL != "https://keys.example.lab/r2" {
		t.Errorf("personal_keys %+v", p)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, config.FileName)); strings.Contains(string(raw), "refresh") {
		t.Errorf("an old key of the block survived the clear:\n%s", raw)
	}
	// Without the flag an existing block is kept (today's behaviour).
	if _, err := runConfig(t, dir, "", "write", "--name", "ws"); err != nil {
		t.Fatal(err)
	}
	if c, _ = config.Load(dir); c.PersonalKeys.RequestURL == "" {
		t.Error("the block was dropped without the flag")
	}
}

// The "this machine" rule is the gate's local check only: `key check` probes
// each host's daemon directly, and those are other machines. With every
// address refused as "not this machine", a host's direct row still answers.
func TestKeyCheckHostProbesNotLimitedToThisMachine(t *testing.T) {
	r := newKeyRig(t, "")
	key := r.prov.Key()
	local, pool := fakeTLS(t, key, func(p string) (int, string) { return 200, "{}" })
	hv1, _ := fakeTLS(t, key, func(p string) (int, string) { return 200, "{}" })
	certPath := filepath.Join(t.TempDir(), "local.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: local.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	lu, _ := url.Parse(local.URL)
	hu, _ := url.Parse(hv1.URL)
	hh, hp, _ := net.SplitHostPort(hu.Host)
	r.dir = writeWorkdir(t, "token_sha256: "+config.Hash(gateToken)+"\nlisten: "+lu.Host+"\ntls_cert: "+certPath+
		"\nhosts:\n  hv1: { address: "+hh+", ssh_user: u, port: "+hp+" }\n", "")
	oldR, oldAllow := keyProbeRoots, allowKeyCheckIP
	keyProbeRoots = pool
	allowKeyCheckIP = func(net.IP) bool { return false }
	t.Cleanup(func() { keyProbeRoots, allowKeyCheckIP = oldR, oldAllow })
	localKeyCheck = realLocalKeyCheck
	r.putKey(t, key, 0o600)
	c := runContract(t, r.dir, "vmtool key check", "--json", "key", "check")
	if !strings.Contains(c.out, `"direct"`) {
		t.Fatalf("no direct row: exit %d out %q err %q", c.code, c.out, c.errOut)
	}
	for _, line := range strings.Split(c.out, "{") {
		if strings.Contains(line, `"direct"`) && !strings.Contains(line, `"accepted"`) {
			t.Errorf("the direct probe of hv1 was held to this machine: %q", line)
		}
	}
	if strings.Contains(c.out+c.errOut, key) {
		t.Error("key in output")
	}
}
