package vmtool

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testTok = "tok-SECRET-0123456789"

func testTunnel() Tunnel { return Tunnel{Exe: "/opt/vmtool/vmtool", Token: testTok} }

func vmtoolTokenEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "VMTOOL_TOKEN=") {
			out = append(out, kv)
		}
	}
	return out
}

func assertTokenOnlyInEnv(t *testing.T, args, env []string) {
	t.Helper()
	for _, a := range args {
		if strings.Contains(a, testTok) {
			t.Fatalf("token in argv: %q", args)
		}
	}
	got := vmtoolTokenEnv(env)
	if len(got) != 1 || got[0] != "VMTOOL_TOKEN="+testTok {
		t.Fatalf("env VMTOOL_TOKEN entries = %q, want exactly one", got)
	}
}

func credsFor(t *testing.T, user string) {
	t.Helper()
	dir := CredsDir("jetfire", "web")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: "+user+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSSHCmdTunnelGoldenArgv(t *testing.T) {
	t.Chdir(t.TempDir())
	credsFor(t, "bot")
	t.Setenv("VMTOOL_TOKEN", "stale-parent-token")
	c, err := SSHCmd(testTunnel(), "jetfire", "web", "uptime")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ProxyCommand=/opt/vmtool/vmtool tunnel jetfire/web",
		"-o", "IdentitiesOnly=yes", "-i", filepath.Join(".machines", "web.jetfire.vmtool", "id_ed25519"),
		"-l", "bot",
		"jetfire/web", "uptime",
	}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("argv\n got %q\nwant %q", c.Args, want)
	}
	assertTokenOnlyInEnv(t, c.Args, c.Env)
}

func TestRemoteCmdTunnelSSHPass(t *testing.T) {
	t.Chdir(t.TempDir())
	c, err := RemoteCmd(testTunnel(), "jetfire", "web", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sshpass", "-e", "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ProxyCommand=/opt/vmtool/vmtool tunnel jetfire/web",
		"-l", "packer",
		"jetfire/web", "'true'",
	}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("argv\n got %q\nwant %q", c.Args, want)
	}
	assertTokenOnlyInEnv(t, c.Args, c.Env)
	if !slices.Contains(c.Env, "SSHPASS=packer") {
		t.Fatal("SSHPASS missing from env")
	}
}

func TestTunnelNeedsExe(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := SSHCmd(Tunnel{Token: testTok}, "jetfire", "web"); err == nil {
		t.Fatal("SSHCmd accepted a Tunnel with no Exe")
	}
}

func TestProxyCommandQuotesExe(t *testing.T) {
	tn := Tunnel{Exe: "/opt/my tools/vmtool"}
	if got, want := tn.ProxyCommand("jetfire", "web"), `'/opt/my tools/vmtool' tunnel jetfire/web`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	tn = Tunnel{Exe: "/opt/it's/vmtool"}
	if got, want := tn.ProxyCommand("jetfire", "web"), `'/opt/it'\''s/vmtool' tunnel jetfire/web`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestInventoryYAMLVMTunnelGolden(t *testing.T) {
	got, err := vmInventoryYAML(testTunnel(), "jetfire", "web", Auth{User: "bot", KeyPath: ".machines/jetfire/web/id_ed25519"}, "/srv/vmtool/.machines/jetfire/web")
	if err != nil {
		t.Fatal(err)
	}
	want := `all:
  hosts:
    web:
      ansible_host: jetfire/web
      ansible_become: true
      ansible_ssh_user: bot
      ansible_ssh_common_args: "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -o 'ProxyCommand=/opt/vmtool/vmtool tunnel jetfire/web'"
      ansible_ssh_private_key_file: ".machines/jetfire/web/id_ed25519"
      vmtool_machines_dir: "/srv/vmtool/.machines/jetfire/web"
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestVMInventoryFilesNeverHoldToken(t *testing.T) {
	t.Chdir(t.TempDir())
	path, cleanup, err := TempVMInventory(testTunnel(), "jetfire", "web", DefaultAuth(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), testTok) || !strings.Contains(string(b), "ansible_host: jetfire/web") {
		t.Fatalf("temp inventory %q", b)
	}
	if err := os.MkdirAll("ansible", 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join("ansible", "inventory.yml")
	if err := WriteVMInventory(p, testTunnel(), "jetfire", "web", DefaultAuth(), "/x"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), testTok) {
		t.Fatal("token in persistent inventory")
	}
	changed, err := EnsureVMInventory(p, testTunnel(), "jetfire", "web", DefaultAuth(), "/x")
	if err != nil || changed {
		t.Fatalf("EnsureVMInventory changed=%v err=%v, want unchanged", changed, err)
	}
	changed, err = EnsureVMInventory(p, testTunnel(), "veritech01", "web", DefaultAuth(), "/x")
	if err != nil || !changed {
		t.Fatalf("EnsureVMInventory other host changed=%v err=%v, want rewrite", changed, err)
	}
}

func TestAnsibleCmdsCarryTokenInEnvOnly(t *testing.T) {
	t.Setenv("VMTOOL_TOKEN", "stale-parent-token")
	c := playbookCmd(testTunnel(), "/tmp/inv.yml", "pb.yml", map[string]string{"deviceid": "web"})
	assertTokenOnlyInEnv(t, c.Args, c.Env)
	if c.Args[0] != "ansible-playbook" {
		t.Fatalf("argv %q", c.Args)
	}
	c = adhocCmd(testTunnel(), "/tmp/inv.yml", "true")
	assertTokenOnlyInEnv(t, c.Args, c.Env)
	// Metal passes no Tunnel: the environment is left alone.
	c = playbookCmd(Tunnel{}, "/tmp/inv.yml", "pb.yml", nil)
	if got := vmtoolTokenEnv(c.Env); len(got) != 1 || got[0] != "VMTOOL_TOKEN=stale-parent-token" {
		t.Fatalf("zero Tunnel changed env: %q", got)
	}
}
