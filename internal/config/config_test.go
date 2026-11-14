package config

import (
	"errors"
	"os"
	"path/filepath"
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
		"local host name": "hosts:\n  jetfire: { address: a, ssh_user: u }\n",
		"config name":     "name: foo\nhosts:\n  foo: { address: a, ssh_user: u }\n",
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
