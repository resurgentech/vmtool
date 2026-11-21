package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
)

func runConfig(t *testing.T, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := configCmd(func() (string, error) { return dir, nil })
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestConfigWriteCreates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "tok-123")
	out, err := runConfig(t, dir, "", "write", "--name", "jetfire", "--role", "hypervisor",
		"--listen", "0.0.0.0:9473", "--tls-cert", "tls/a.crt", "--tls-key", "tls/a.key")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "tok-123") {
		t.Fatal("token printed")
	}
	st, err := os.Stat(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	c, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "jetfire" || c.Role != config.RoleHypervisor || c.Listen != "0.0.0.0:9473" ||
		c.TLSCert != "tls/a.crt" || c.TLSKey != "tls/a.key" || c.TokenSHA256 != config.Hash("tok-123") {
		t.Fatalf("%+v", c)
	}
}

func TestConfigWritePreserves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "newtok")
	orig := "name: jetfire\ntoken_sha256: " + config.Hash("old") + "\nfuture_key: keepme\nhosts:\n  veritech01: { address: v.labtwo, ssh_user: ubuntu }\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, dir, "", "write", "--listen", "127.0.0.1:1111"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if !strings.Contains(string(b), "future_key: keepme") {
		t.Fatalf("unknown key lost:\n%s", b)
	}
	c, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hosts["veritech01"].Address != "v.labtwo" || c.Listen != "127.0.0.1:1111" ||
		c.Name != "jetfire" || c.TokenSHA256 != config.Hash("newtok") {
		t.Fatalf("%+v", c)
	}
	st, _ := os.Stat(filepath.Join(dir, "config.yml"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestConfigWriteTokenStdin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "")
	if _, err := runConfig(t, dir, "  piped-tok \nignored\n", "write", "--token-stdin"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenSHA256 != config.Hash("piped-tok") {
		t.Fatal("hash not from stdin")
	}
}

func TestConfigWriteNoToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "")
	_, err := runConfig(t, dir, "", "write")
	if err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(filepath.Join(dir, "config.yml")); serr == nil {
		t.Fatal("config.yml written without token")
	}
	if _, err := runConfig(t, dir, "\n", "write", "--token-stdin"); err == nil {
		t.Fatal("empty stdin token should error")
	}
}

func TestConfigWriteRejectsBadRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "x")
	if _, err := runConfig(t, dir, "", "write", "--role", "boss"); err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(filepath.Join(dir, "config.yml")); serr == nil {
		t.Fatal("invalid config written")
	}
}

func TestConfigHost(t *testing.T) {
	dir := t.TempDir()
	body := "hosts:\n  veritech01: { address: v.labtwo, ssh_user: ubuntu }\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runConfig(t, dir, "", "host", "veritech01")
	if err != nil {
		t.Fatal(err)
	}
	if out != "v.labtwo ubuntu 9473\n" {
		t.Fatalf("%q", out)
	}
	if _, err := runConfig(t, dir, "", "host", "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown host err: %v", err)
	}
	if _, err := runConfig(t, t.TempDir(), "", "host", "veritech01"); err == nil {
		t.Fatal("missing config should error")
	}
}

// config host prints "address ssh_user port": 9473 when the entry has no
// port, the entry's own port when it has one.
func TestConfigHostPort(t *testing.T) {
	dir := t.TempDir()
	body := "hosts:\n  hv1: { address: hv1.example, ssh_user: u }\n  PLACEHOLDER-HOST: { address: p.example, ssh_user: u2, port: 9474 }\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"hv1": "hv1.example u 9473\n", "PLACEHOLDER-HOST": "p.example u2 9474\n"} {
		out, err := runConfig(t, dir, "", "host", name)
		if err != nil || out != want {
			t.Errorf("config host %s = %q, %v; want %q", name, out, err, want)
		}
	}
}

func TestConfigSetHost(t *testing.T) {
	dir := t.TempDir()
	// no token in the environment or .env: set-host must not need one
	t.Setenv("VMTOOL_TOKEN", "")
	orig := "name: jetfire\nfuture_key: keepme\nhosts:\n  hv1: { address: a.example, ssh_user: u }\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runConfig(t, dir, "", "set-host", "PLACEHOLDER-HOST", "--address", "10.0.0.9",
		"--ssh-user", "ops", "--port", "9474", "--default-pool", "nvme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "PLACEHOLDER-HOST") {
		t.Fatalf("out %q", out)
	}
	c, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Hosts["PLACEHOLDER-HOST"]; h != (config.Host{Address: "10.0.0.9", SSHUser: "ops", Port: 9474, DefaultPool: "nvme"}) {
		t.Fatalf("%+v", h)
	}
	if c.Hosts["hv1"].Address != "a.example" {
		t.Fatal("hv1 lost")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if !strings.Contains(string(b), "future_key: keepme") {
		t.Fatalf("unknown key lost:\n%s", b)
	}
	if _, err := runConfig(t, dir, "", "set-host", "hv2", "--address", "a"); err == nil {
		t.Fatal("missing --ssh-user accepted")
	}
}

// install.sh runs `config write` with exactly these flags; every hosts:
// entry (own name included) and image_dir must survive it.
func TestConfigWriteInstallKeepsHosts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "PLACEHOLDER-not-a-secret-1")
	orig := "name: hv1\nrole: workstation\nimage_dir: /srv/PLACEHOLDER-images\nhosts:\n" +
		"  hv1: { address: 127.0.0.1, ssh_user: ubuntu, port: 9474 }\n" +
		"  PLACEHOLDER-HOST: { address: ph.example, ssh_user: ubuntu }\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, dir, "", "write", "--name", "hv1", "--role", "workstation",
		"--listen", "127.0.0.1:9473", "--tls-cert", "tls/hv1.labtwo.crt", "--tls-key", "tls/hv1.labtwo.key"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.ImageDir != "/srv/PLACEHOLDER-images" || len(c.Hosts) != 2 ||
		c.Hosts["hv1"].Port != 9474 || c.Hosts["hv1"].Address != "127.0.0.1" ||
		c.Hosts["PLACEHOLDER-HOST"].Address != "ph.example" || c.Listen != "127.0.0.1:9473" {
		t.Fatalf("%+v", c)
	}
}

func TestConfigSetHostVMsFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "")
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("name: jetfire\nhosts:\n  hv1: { address: a.example, ssh_user: u }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, dir, "", "set-host", "hv1", "--address", "a.example", "--ssh-user", "u", "--vms=false"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if !strings.Contains(string(b), "vms: false") {
		t.Fatalf("vms: false not written:\n%s", b)
	}
	// without --vms the key stays
	if _, err := runConfig(t, dir, "", "set-host", "hv1", "--address", "c.example", "--ssh-user", "u"); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load(dir)
	if c.Hosts["hv1"].HoldsVMs() || c.Hosts["hv1"].Address != "c.example" {
		t.Fatalf("%+v", c.Hosts["hv1"])
	}
	if _, err := runConfig(t, dir, "", "set-host", "hv1", "--address", "c.example", "--ssh-user", "u", "--vms=true"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "config.yml"))
	if strings.Contains(string(b), "vms") {
		t.Fatalf("vms not removed:\n%s", b)
	}
}

func TestConfigSetHostLoneVMs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "")
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("name: jetfire\nhosts:\n  hv1: { address: a.example, ssh_user: u, port: 9475 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, dir, "", "set-host", "hv1", "--vms=false"); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load(dir)
	if h := c.Hosts["hv1"]; h.HoldsVMs() || h.Address != "a.example" || h.SSHUser != "u" || h.Port != 9475 {
		t.Fatalf("%+v", h)
	}
	if _, err := runConfig(t, dir, "", "set-host", "hv1", "--vms=true"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if strings.Contains(string(b), "vms") {
		t.Fatalf("not removed:\n%s", b)
	}
	before := string(b)
	if _, err := runConfig(t, dir, "", "set-host", "nope", "--vms=false"); err == nil {
		t.Fatal("unknown host accepted")
	}
	b, _ = os.ReadFile(filepath.Join(dir, "config.yml"))
	if string(b) != before {
		t.Fatal("file changed")
	}
}
