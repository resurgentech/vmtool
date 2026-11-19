package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Placeholders only.
const (
	credUUID = "00000000-0000-4000-8000-000000000001"
	credPass = "PLACEHOLDER-not-a-secret-2"
	credKey  = "PLACEHOLDER-KEY-not-a-key"
)

// uuidFake is a Manager whose VM has an address and a domain UUID.
type uuidFake struct{ cliFake }

func (uuidFake) Info(string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: "web", IP: "192.0.2.9", UUID: credUUID, State: vmtool.StateRunning}, nil
}

// credCLI runs the real root against a working directory whose config names
// the store double, with every host answered by uuidFake and ssh stubbed: it
// appends each call's argv to the returned file and counts calls. STUB_SSH is
// "first255" or "always255".
type credCLI struct {
	t     *testing.T
	s     *cliStore
	dir   string
	argv  string
	count string
}

func newCredCLI(t *testing.T) *credCLI {
	t.Helper()
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok) // chdirs into a temp dir
	t.Chdir(dir)
	c := &credCLI{t: t, s: s, dir: dir}
	d := t.TempDir()
	c.argv, c.count = filepath.Join(d, "argv"), filepath.Join(d, "count")
	testexec.Stub(t, map[string]string{
		"ssh": "#!/bin/sh\nn=$(cat '" + c.count + "' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '" + c.count + "'\n" +
			"printf '%s\\n' \"$@\" >> '" + c.argv + "'\n" +
			"[ \"$STUB_SSH\" = always255 ] && exit 255\n[ \"$STUB_SSH\" = first255 ] && [ \"$n\" = 1 ] && exit 255\nexit 0\n",
		"sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
	})
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			return uuidFake{cliFake{host: e.cfg.Name, f: &fakeHosts{}}}, func() {}, nil
		})
	}
	return c
}

func (c *credCLI) run(args ...string) (out, errOut string, err error) {
	c.t.Helper()
	var o, e bytes.Buffer
	root, ev := rootFor(c.dir, &o, &e)
	ev.tunnelExe = "/opt/vmtool/vmtool"
	root.SetArgs(args)
	err = root.Execute()
	return o.String(), e.String(), err
}

func (c *credCLI) sshCalls() string {
	b, _ := os.ReadFile(c.count)
	return strings.TrimSpace(string(b))
}

func credDir() string { return vmtool.CredsDir("ws", "web") }

func noSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, secret := range []string{storeTok, credPass, credKey} {
		if strings.Contains(text, secret) {
			t.Errorf("%s carries %q", what, secret)
		}
	}
}

func TestCredsFetchWritesTheCache(t *testing.T) {
	c := newCredCLI(t)
	c.s.vmRecord("ws/web", credUUID, credPass, credKey)
	out, errOut, err := c.run("creds", "fetch", "ws/web")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(credDir(), "id_ed25519")); err != nil || string(b) != credKey {
		t.Fatalf("key not cached: %q %v", b, err)
	}
	if st, _ := os.Stat(credDir()); st.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(credDir(), "id_ed25519")); st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %o", st.Mode().Perm())
	}
	if errOut != "" {
		t.Errorf("stderr %q", errOut)
	}
	noSecrets(t, "stdout", out)
}

func TestCredsFetchErrors(t *testing.T) {
	c := newCredCLI(t)
	// The store has no record: not found, nothing written.
	if _, _, err := c.run("creds", "fetch", "ws/web"); err == nil || !vmtool.IsNotFound(err) {
		t.Fatalf("no record: %v", err)
	}
	// A record for another VM: refused as stale.
	c.s.vmRecord("ws/web", "00000000-0000-4000-8000-000000000002", credPass, credKey)
	if _, _, err := c.run("creds", "fetch", "ws/web"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale: %v", err)
	}
	if _, err := os.Stat(credDir()); !os.IsNotExist(err) {
		t.Fatal("a stale record was cached")
	}
	// No server: configured: a clear error in local mode.
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\n", "")
	var o, e bytes.Buffer
	root, ev := rootFor(dir, &o, &e)
	ev.tunnelExe = "/opt/vmtool/vmtool"
	root.SetArgs([]string{"creds", "fetch", "ws/web"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("local mode: %v", err)
	}
}

func TestCredsForgetRemovesTheCacheOnly(t *testing.T) {
	c := newCredCLI(t)
	c.s.vmRecord("ws/web", credUUID, credPass, credKey)
	if _, _, err := c.run("creds", "fetch", "ws/web"); err != nil {
		t.Fatal(err)
	}
	if _, errOut, err := c.run("creds", "forget", "ws/web"); err != nil || errOut != "" {
		t.Fatalf("forget: %v %q", err, errOut)
	}
	if _, err := os.Stat(credDir()); !os.IsNotExist(err) {
		t.Fatal("cache kept")
	}
	c.s.mu.Lock()
	_, there := c.s.creds["ws/web"]
	c.s.mu.Unlock()
	if !there {
		t.Fatal("forget removed the store's record")
	}
	root, _ := rootFor(c.dir, io.Discard, io.Discard)
	for _, sub := range root.Commands() {
		if sub.Name() == "creds" {
			for _, s2 := range sub.Commands() {
				if s2.Name() == "forget" && !strings.Contains(s2.Long+s2.Short, "cache") {
					t.Error("creds forget help does not say it removes only the cache")
				}
			}
		}
	}
}

func TestExecFetchesMissingCredentialsFromTheStore(t *testing.T) {
	c := newCredCLI(t)
	c.s.vmRecord("ws/web", credUUID, credPass, credKey)
	if _, errOut, err := c.run("exec", "ws/web", "true"); err != nil || errOut != "" {
		t.Fatalf("exec: %v %q", err, errOut)
	}
	b, _ := os.ReadFile(c.argv)
	if !strings.Contains(string(b), filepath.Join(".machines", "web.ws.vmtool", "id_ed25519")) {
		t.Fatalf("ssh did not use the fetched key:\n%s", b)
	}
	noSecrets(t, "ssh argv", string(b))
}

func TestExecRefetchesOnceOnExit255(t *testing.T) {
	c := newCredCLI(t)
	c.s.vmRecord("ws/web", credUUID, "PLACEHOLDER-OLD-not-a-secret", "PLACEHOLDER-OLD-KEY")
	// The record changes after the first GET (the reconcile); the first ssh
	// fails with 255 and the refetch finds the new record.
	c.s.onGet = func(n int32) {
		if n == 1 {
			c.s.vmRecord("ws/web", credUUID, credPass, credKey)
		}
	}
	t.Setenv("STUB_SSH", "first255")
	if _, _, err := c.run("exec", "ws/web", "true"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if c.sshCalls() != "2" {
		t.Fatalf("%s ssh calls, want 2 (one retry)", c.sshCalls())
	}
	if b, _ := os.ReadFile(filepath.Join(credDir(), "id_ed25519")); string(b) != credKey {
		t.Fatalf("cache not refetched: %q", b)
	}

	// Unchanged record: 255 is reported, no second ssh.
	c2 := newCredCLI(t)
	c2.s.vmRecord("ws/web", credUUID, credPass, credKey)
	t.Setenv("STUB_SSH", "always255")
	_, _, err := c2.run("exec", "ws/web", "true")
	if re, ok := err.(remoteExitError); !ok || re.code != 255 || c2.sshCalls() != "1" {
		t.Fatalf("err %v after %s calls, want exit 255 after exactly 1", err, c2.sshCalls())
	}
}

// ---- tunnel: the ProxyCommand of plain ssh/scp/rsync ----

// fakeTunnelUUID replaces the tunnel's UUID lookup and counts the calls.
func fakeTunnelUUID(t *testing.T) *int {
	t.Helper()
	n := new(int)
	old := tunnelVMUUID
	t.Cleanup(func() { tunnelVMUUID = old })
	tunnelVMUUID = func(_ context.Context, _ *env, _ app.VMRef) (string, error) {
		*n++
		return credUUID, nil
	}
	return n
}

func TestTunnelFetchesMissingCredentialsBeforePiping(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	t.Chdir(dir)
	s.vmRecord("ws/web", credUUID, credPass, credKey)
	fakeTunnel(t, nil)
	fakeTunnelUUID(t)
	var out bytes.Buffer
	errOut, err := runTunnel(t, dir, strings.NewReader("ssh bytes"), &out, "web.ws.vmtool")
	if err != nil {
		t.Fatal(err)
	}
	// stdout is the SSH byte stream and nothing else.
	if out.String() != "ssh bytes" || errOut != "" {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut)
	}
	if b, err := os.ReadFile(filepath.Join(credDir(), "id_ed25519")); err != nil || string(b) != credKey {
		t.Fatalf("the key ssh-config's IdentityFile names was not fetched: %q %v", b, err)
	}
	if st, _ := os.Stat(credDir()); st.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %o", st.Mode().Perm())
	}
}

// With the folder there, the tunnel never touches the store: ssh and scp run
// it for every connection.
func TestTunnelWithCredentialsPresentDoesNotProbeTheStore(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	t.Chdir(dir)
	fakeTunnel(t, nil)
	asked := fakeTunnelUUID(t)
	if err := os.MkdirAll(credDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir(), "id_ed25519"), []byte(credKey), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := runTunnel(t, dir, strings.NewReader("x"), &out, "ws/web"); err != nil {
		t.Fatal(err)
	}
	if s.whoami.Load() != 0 || *asked != 0 {
		t.Fatalf("whoami %d, uuid lookups %d: the tunnel probed with the folder present", s.whoami.Load(), *asked)
	}
}

// The store is down and the folder is missing: the tunnel still pipes, says
// so on stderr, and stdout stays clean.
func TestTunnelStoreDownStillPipes(t *testing.T) {
	s := newCLIStore(t)
	s.down.Store(true)
	dir := storeWorkdir(t, s, storeTok)
	t.Chdir(dir)
	fakeTunnel(t, nil)
	fakeTunnelUUID(t)
	var out bytes.Buffer
	errOut, err := runTunnel(t, dir, strings.NewReader("ssh bytes"), &out, "ws/web")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "ssh bytes" {
		t.Fatalf("stdout %q", out.String())
	}
	if errOut != unreachableLine {
		t.Fatalf("stderr %q, want %q", errOut, unreachableLine)
	}
}
