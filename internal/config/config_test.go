package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setHost(t *testing.T, name string) {
	t.Helper()
	old := hostnameFn
	hostnameFn = func() (string, error) { return name, nil }
	t.Cleanup(func() { hostnameFn = old })
}

func TestLoadDefaults(t *testing.T) {
	setHost(t, "jetfire.labtwo")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "tls_cert: tls/a.crt\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "jetfire" || c.Role != RoleWorkstation || c.Listen != DefaultListen {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if got := c.Path(c.TLSCert); got != filepath.Join(dir, "tls/a.crt") {
		t.Fatalf("Path = %q", got)
	}
	if got := c.Path("/abs/x"); got != "/abs/x" {
		t.Fatalf("Path abs = %q", got)
	}
}

func TestLoadHosts(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "hosts:\n  veritech01: { address: v.labtwo, ssh_user: ubuntu }\n  v2: { address: x, ssh_user: u, port: 1234 }\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Hosts["veritech01"]; h.Address != "v.labtwo" || h.SSHUser != "ubuntu" || h.Port != 9473 {
		t.Fatalf("host: %+v", h)
	}
	if c.Hosts["v2"].Port != 1234 {
		t.Fatal("explicit port lost")
	}
}

func TestLoadNotFound(t *testing.T) {
	_, err := Load(t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadValidation(t *testing.T) {
	setHost(t, "jetfire")
	hash := strings.Repeat("a", 64)
	cases := map[string]string{
		"bad role":        "role: boss\n",
		"bad host name":   "hosts:\n  'bad name': { address: a, ssh_user: u }\n",
		"local host name": "role: hypervisor\nhosts:\n  jetfire: { address: a, ssh_user: u }\n",
		"config name":     "name: foo\nrole: hypervisor\nhosts:\n  foo: { address: a, ssh_user: u }\n",
		"short hash":      "token_sha256: abc\n",
		"upper hash":      "token_sha256: " + strings.ToUpper(hash) + "\n",
		"missing address": "hosts:\n  h1: { ssh_user: u }\n",
		"bad yaml":        "name: [\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "config.yml", body)
			_, err := Load(dir)
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("want validation error, got %v", err)
			}
		})
	}
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "role: hypervisor\ntoken_sha256: "+hash+"\n")
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestTokenEnvWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "VMTOOL_TOKEN=fromfile\n")
	t.Setenv("VMTOOL_TOKEN", "fromenv")
	tok, err := Token(dir)
	if err != nil || tok != "fromenv" {
		t.Fatalf("%q %v", tok, err)
	}
}

func TestTokenDotEnvVariants(t *testing.T) {
	cases := map[string]string{
		"plain":      "VMTOOL_TOKEN=abc\n",
		"double":     "VMTOOL_TOKEN=\"abc\"\n",
		"single":     "VMTOOL_TOKEN='abc'\n",
		"export":     "export VMTOOL_TOKEN=abc\n",
		"comments":   "# hi\n\nOTHER=x\n  # more\nVMTOOL_TOKEN=abc\n",
		"crlf":       "VMTOOL_TOKEN=abc\r\n",
		"no newline": "VMTOOL_TOKEN=abc",
		"spaces":     "VMTOOL_TOKEN = abc \n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("VMTOOL_TOKEN", "")
			dir := t.TempDir()
			writeFile(t, dir, ".env", body)
			tok, err := Token(dir)
			if err != nil || tok != "abc" {
				t.Fatalf("%q %v", tok, err)
			}
		})
	}
}

func TestTokenAbsent(t *testing.T) {
	t.Setenv("VMTOOL_TOKEN", "")
	dir := t.TempDir()
	if _, err := Token(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("no .env: %v", err)
	}
	writeFile(t, dir, ".env", "OTHER=1\nVMTOOL_TOKEN=\n")
	if _, err := Token(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty: %v", err)
	}
}

func TestCheck(t *testing.T) {
	h := Hash("s3cret")
	if len(h) != 64 {
		t.Fatal(h)
	}
	if !Check("s3cret", h) || Check("other", h) || Check("s3cret", "") || Check("", h) || Check("s3cret", "zz") {
		t.Fatal("Check wrong")
	}
}

func TestVerify(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "")
	if err := Verify(dir); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNoToken) {
		t.Fatalf("%v", err)
	}
	writeFile(t, dir, "config.yml", "token_sha256: "+Hash("good")+"\n")
	if err := Verify(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("%v", err)
	}
	t.Setenv("VMTOOL_TOKEN", "bad")
	if err := Verify(dir); !errors.Is(err, ErrTokenMismatch) {
		t.Fatalf("%v", err)
	}
	t.Setenv("VMTOOL_TOKEN", "good")
	if err := Verify(dir); err != nil {
		t.Fatal(err)
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	setHost(t, "jetfire")
	const secret = "SUPERSECRETTOKEN123"
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", secret)
	writeFile(t, dir, "config.yml", "token_sha256: "+Hash("different")+"\n")
	var errs []error
	errs = append(errs, Verify(dir))
	// malformed config containing the token as a role and as a hash
	for _, body := range []string{"role: " + secret + "\n", "token_sha256: " + secret + "\n", "name: [" + secret + "\n"} {
		d := t.TempDir()
		writeFile(t, d, "config.yml", body)
		_, err := Load(d)
		errs = append(errs, err)
	}
	t.Setenv("VMTOOL_TOKEN", "")
	d := t.TempDir()
	writeFile(t, d, ".env", "OTHER="+secret+"\n")
	_, err := Token(d)
	errs = append(errs, err)
	for i, err := range errs {
		if err == nil {
			t.Fatalf("case %d: expected an error", i)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("case %d leaks token: %v", i, err)
		}
	}
}

func TestPathRelativeEmpty(t *testing.T) {
	c := &Config{Dir: "/d"}
	if c.Path("") != "" {
		t.Fatal("empty path should stay empty")
	}
}

// A host name (configured or local) may not equal a registered metal/alien
// machine: both would claim .machines/<name>/.
func TestLoadRefusesHostNamedLikeMachine(t *testing.T) {
	setHost(t, "jetfire")
	machine := func(dir, name string) {
		t.Helper()
		d := filepath.Join(dir, ".machines", name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, d, "machine.yml", "kind: metal\nhost: 10.0.0.42\n")
	}

	dir := t.TempDir()
	machine(dir, "veritech01")
	writeFile(t, dir, "config.yml", "hosts:\n  veritech01: { address: v.labtwo, ssh_user: ubuntu }\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "veritech01") || !strings.Contains(err.Error(), "machine.yml") {
		t.Fatalf("configured host: %v", err)
	}

	dir = t.TempDir()
	machine(dir, "jetfire")
	writeFile(t, dir, "config.yml", "role: workstation\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "jetfire") {
		t.Fatalf("local host: %v", err)
	}

	// A host directory of VM credentials (no machine.yml) is fine.
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".machines", "veritech01", "web"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "config.yml", "hosts:\n  veritech01: { address: v.labtwo, ssh_user: ubuntu }\n")
	if _, err := Load(dir); err != nil {
		t.Fatalf("host dir: %v", err)
	}
}

// A mis-pasted token must never reach an error message; the line may.
const pastedToken = "9f2c4e1ab07d83565c1e2a49bd30f7716a8c5e0d2b94f31c7ea6805d4b3f1a22"

func TestParseErrorNamesLineNotContent(t *testing.T) {
	setHost(t, "jetfire")
	cases := map[string]string{
		"type error":   "name: jetfire\nrole: workstation\nhosts: " + pastedToken + "\n",
		"syntax error": "name: jetfire\nrole: workstation\ntoken_sha256: " + pastedToken + ": x\n",
	}
	for name, body := range cases {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", body)
		_, err := Load(dir)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if !strings.Contains(err.Error(), "line 3") || strings.Contains(err.Error(), pastedToken) {
			t.Errorf("%s: err = %q", name, err)
		}
		if want := "config.yml: invalid YAML or field types at line 3"; err.Error() != want {
			t.Errorf("%s: err = %q, want %q", name, err, want)
		}
	}
}

func TestWriteErrorNamesLineNotContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "name: jetfire\nrole: workstation\ntoken_sha256: "+pastedToken+": x\n")
	err := Write(dir, "tok", WriteOptions{})
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "line 3") || strings.Contains(err.Error(), pastedToken) {
		t.Errorf("err = %q", err)
	}
}

func TestLoadRefusesDottedHostNames(t *testing.T) {
	setHost(t, "jetfire")
	for _, tc := range []struct{ name, yml string }{
		{"name", "name: x.y\n"},
		{"hosts key", "hosts:\n  x.y: { address: 10.0.0.1 }\n"},
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", tc.yml)
		_, err := Load(dir)
		want := `host name "x.y" must not contain '.': VM SSH names are <vm>.<host>.vmtool`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want containing %q", tc.name, err, want)
		}
	}
}

func TestDefaultPool(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "default_pool: nvme\nhosts:\n  h1: { address: a, ssh_user: u, default_pool: fast.1 }\n  h2: { address: b, ssh_user: u }\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultPool != "nvme" || c.Hosts["h1"].DefaultPool != "fast.1" || c.Hosts["h2"].DefaultPool != "" {
		t.Fatalf("default pools: %q %+v", c.DefaultPool, c.Hosts)
	}
}

func TestDefaultPoolValidation(t *testing.T) {
	setHost(t, "jetfire")
	for name, tc := range map[string]struct{ body, key string }{
		"top slash":   {"default_pool: a/b\n", "default_pool"},
		"top leading": {"default_pool: -x\n", "default_pool"},
		"top space":   {"default_pool: 'a b'\n", "default_pool"},
		"host slash":  {"hosts:\n  h1: { address: a, ssh_user: u, default_pool: ../x }\n", "hosts.h1.default_pool"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "config.yml", tc.body)
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("want error naming %s, got %v", tc.key, err)
			}
		})
	}
}

func TestOwnNameListedAsWorkstation(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "hosts:\n  jetfire: { address: 127.0.0.1, ssh_user: u, default_pool: nvme }\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Hosts["jetfire"]
	if h.Address != "127.0.0.1" || h.Port != DefaultPort || h.DefaultPool != "nvme" {
		t.Fatalf("%+v", h)
	}
	// an explicit name is own too
	writeFile(t, dir, "config.yml", "name: ws1\nhosts:\n  ws1: { address: 127.0.0.1, ssh_user: u, port: 9474 }\n")
	if c, err = Load(dir); err != nil || c.Hosts["ws1"].Port != 9474 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestOwnNameListedValidatedLikeAnyEntry(t *testing.T) {
	setHost(t, "jetfire")
	for name, body := range map[string]string{
		"no address":   "hosts:\n  jetfire: { ssh_user: u }\n",
		"bad pool":     "hosts:\n  jetfire: { address: a, ssh_user: u, default_pool: a/b }\n",
		"hypervisor":   "role: hypervisor\nhosts:\n  jetfire: { address: a, ssh_user: u }\n",
		"top pool too": "default_pool: nvme\nhosts:\n  jetfire: { address: a, ssh_user: u }\n",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", body)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "default_pool: nvme\nhosts:\n  jetfire: { address: a, ssh_user: u }\n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "hosts.jetfire.default_pool") {
		t.Fatalf("want pointer to hosts.jetfire.default_pool, got %v", err)
	}
	// top-level default_pool is fine when the own name is not listed
	writeFile(t, dir, "config.yml", "default_pool: nvme\nhosts:\n  hv1: { address: a, ssh_user: u }\n")
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestImageDir(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "image_dir: images\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.ImageDir != "images" || c.ResolvedImageDir() != filepath.Join(dir, "images") {
		t.Fatalf("%q %q", c.ImageDir, c.ResolvedImageDir())
	}
	writeFile(t, dir, "config.yml", "image_dir: /srv/img\n")
	if c, _ = Load(dir); c.ResolvedImageDir() != "/srv/img" {
		t.Fatalf("abs: %q", c.ResolvedImageDir())
	}
	writeFile(t, dir, "config.yml", "name: x\n")
	if c, _ = Load(dir); c.ResolvedImageDir() != "" {
		t.Fatalf("unset: %q", c.ResolvedImageDir())
	}
}

const allKeys = `# header comment
name: jetfire
role: workstation
listen: 127.0.0.1:9473
token_sha256: ` + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `
tls_cert: tls/a.crt
tls_key: tls/a.key
default_pool: nvme
image_dir: images
ip_wait_seconds: 30
ssh_wait_seconds: 40
future_key: keepme
hosts:
  # hv1 comment
  hv1: { address: a.example, ssh_user: u, port: 9475, default_pool: fast }
`

func TestSetHostRoundTrip(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", allKeys)
	before, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetHost(dir, "PLACEHOLDER-HOST", Host{Address: "10.0.0.9", SSHUser: "ops", Port: 9474, DefaultPool: "nvme2"}); err != nil {
		t.Fatal(err)
	}
	after, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Hosts["PLACEHOLDER-HOST"]; got != (Host{Address: "10.0.0.9", SSHUser: "ops", Port: 9474, DefaultPool: "nvme2"}) {
		t.Fatalf("new entry %+v", got)
	}
	if after.Hosts["hv1"] != before.Hosts["hv1"] {
		t.Fatalf("hv1 changed: %+v", after.Hosts["hv1"])
	}
	// every other key unchanged
	a, b := *after, *before
	a.Hosts, b.Hosts = nil, nil
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("keys changed:\n%+v\n%+v", a, b)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	for _, s := range []string{"# header comment", "future_key: keepme", "# hv1 comment"} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("lost %q:\n%s", s, raw)
		}
	}
	if st, _ := os.Stat(filepath.Join(dir, "config.yml")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	// replace: the entry becomes exactly the new one
	if err := SetHost(dir, "hv1", Host{Address: "b.example", SSHUser: "x"}); err != nil {
		t.Fatal(err)
	}
	after, _ = Load(dir)
	if got := after.Hosts["hv1"]; got != (Host{Address: "b.example", SSHUser: "x", Port: DefaultPort}) || len(after.Hosts) != 2 {
		t.Fatalf("replaced %+v (%d hosts)", got, len(after.Hosts))
	}
}

func TestSetHostOnMissingFileAndInvalid(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	if err := SetHost(dir, "hv1", Host{Address: "a", SSHUser: "u"}); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(dir); err != nil || c.Hosts["hv1"].Address != "a" {
		t.Fatalf("%v %+v", err, c)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	for name, h := range map[string]struct {
		n string
		h Host
	}{
		"no address": {"hv2", Host{SSHUser: "u"}},
		"no user":    {"hv2", Host{Address: "a"}},
		"dotted":     {"a.b", Host{Address: "a", SSHUser: "u"}},
		"bad pool":   {"hv2", Host{Address: "a", SSHUser: "u", DefaultPool: "../x"}},
		"bad port":   {"hv2", Host{Address: "a", SSHUser: "u", Port: 70000}},
	} {
		if err := SetHost(dir, h.n, h.h); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if string(before) != string(after) {
		t.Fatal("file changed by rejected set-host")
	}
}

// V6: hosts.<h>.vms: false marks a network home, a host that runs no VMs.
func TestHostVMsSwitch(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "hosts:\n  hv1: { address: a, ssh_user: u }\n  home1: { address: b, ssh_user: u, vms: false }\n  hv2: { address: c, ssh_user: u, vms: true }\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"hv1": true, "home1": false, "hv2": true} {
		if got := c.Hosts[name].HoldsVMs(); got != want {
			t.Errorf("%s HoldsVMs = %v, want %v", name, got, want)
		}
	}
}

func TestSetHostVMsKey(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "name: jetfire\nhosts:\n  hv1: { address: a.example, ssh_user: u }\n")
	no, yes := false, true
	if err := SetHost(dir, "hv1", Host{Address: "a.example", SSHUser: "u", VMs: &no}); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(dir)
	if c.Hosts["hv1"].HoldsVMs() {
		t.Fatal("vms: false not written")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if !strings.Contains(string(raw), "vms: false") {
		t.Fatalf("no vms: false:\n%s", raw)
	}
	// not given: the key is left as it is
	if err := SetHost(dir, "hv1", Host{Address: "b.example", SSHUser: "u"}); err != nil {
		t.Fatal(err)
	}
	c, _ = Load(dir)
	if h := c.Hosts["hv1"]; h.HoldsVMs() || h.Address != "b.example" {
		t.Fatalf("vms key lost or address kept: %+v", h)
	}
	// true: the key is removed
	if err := SetHost(dir, "hv1", Host{Address: "b.example", SSHUser: "u", VMs: &yes}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "config.yml"))
	if strings.Contains(string(raw), "vms") {
		t.Fatalf("vms key not removed:\n%s", raw)
	}
}

func TestSetHostVMsOnly(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	orig := "# top\nname: jetfire\nfuture_key: keepme\nhosts:\n  # hv1 comment\n  hv1: { address: a.example, ssh_user: u, port: 9475, default_pool: fast }\n"
	writeFile(t, dir, "config.yml", orig)
	if err := SetHostVMs(dir, "hv1", false); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(dir)
	h := c.Hosts["hv1"]
	if h.HoldsVMs() || h.Address != "a.example" || h.SSHUser != "u" || h.Port != 9475 || h.DefaultPool != "fast" {
		t.Fatalf("%+v", h)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	for _, s := range []string{"# top", "future_key: keepme", "# hv1 comment"} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("lost %q:\n%s", s, raw)
		}
	}
	if err := SetHostVMs(dir, "hv1", true); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "config.yml"))
	if strings.Contains(string(raw), "vms") {
		t.Fatalf("not removed:\n%s", raw)
	}
	c, _ = Load(dir)
	if h := c.Hosts["hv1"]; h.Port != 9475 || h.Address != "a.example" {
		t.Fatalf("%+v", h)
	}
	// unknown host: refused, file unchanged
	before, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if err := SetHostVMs(dir, "nope", false); err == nil {
		t.Fatal("unknown host accepted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if string(before) != string(after) {
		t.Fatal("file changed")
	}
}
