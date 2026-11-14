package vmtool

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeCreds(t *testing.T, dir, user string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: "+user+"\npassword: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCredsDir(t *testing.T) {
	if got := CredsDir("jetfire", "web"); got != filepath.Join(".machines", "jetfire", "web") {
		t.Fatalf("CredsDir = %q", got)
	}
}

// Credentials are keyed by (host, vm) only; an IP-keyed or bare-name entry
// never answers for a VM, and the same VM name on another host is separate.
func TestResolveVMAuthByHostAndName(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCreds(t, filepath.Join(".machines", "10.0.0.5"), "stale-ip")
	writeCreds(t, filepath.Join(".machines", "web"), "stale-bare")
	writeCreds(t, filepath.Join(".machines", "veritech01", "web"), "other-host")

	if a := ResolveVMAuth("jetfire", "web", Auth{User: "u", Password: "p"}); a.User != "u" || a.Password != "p" {
		t.Fatalf("no creds: %+v, want the fallback", a)
	}
	if a := ResolveVMAuth("jetfire", "web", Auth{}); a.User != "packer" || a.Password != "packer" {
		t.Fatalf("no creds, empty fallback: %+v, want packer defaults", a)
	}
	writeCreds(t, CredsDir("jetfire", "web"), "bot")
	if a := ResolveVMAuth("jetfire", "web", DefaultAuth()); a.User != "bot" {
		t.Fatalf("with creds: %+v, want bot", a)
	}
	if a := ResolveVMAuth("veritech01", "web", DefaultAuth()); a.User != "other-host" {
		t.Fatalf("other host: %+v", a)
	}
	for _, bad := range [][2]string{{"", "web"}, {"jetfire", ""}, {"..", "web"}, {"jetfire", "../web"}} {
		if a := ResolveVMAuth(bad[0], bad[1], DefaultAuth()); a.User != "packer" {
			t.Errorf("%v: %+v, want packer defaults", bad, a)
		}
	}
}

func TestForgetVM(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCreds(t, CredsDir("jetfire", "web"), "bot")
	writeCreds(t, CredsDir("jetfire", "db"), "bot")
	writeCreds(t, CredsDir("veritech01", "web"), "bot")
	writeCreds(t, filepath.Join(".machines", "10.0.0.5"), "bot")
	if err := SaveMachine(Machine{Name: "nuc1", Kind: MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}

	if err := ForgetVM("jetfire", "web"); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		CredsDir("jetfire", "web"):             false,
		CredsDir("jetfire", "db"):              true,
		CredsDir("veritech01", "web"):          true,
		filepath.Join(".machines", "10.0.0.5"): true,
		filepath.Join(".machines", "nuc1"):     true,
	} {
		_, err := os.Stat(dir)
		if got := err == nil; got != want {
			t.Errorf("%s exists=%v, want %v", dir, got, want)
		}
	}
	if a := ResolveVMAuth("jetfire", "web", DefaultAuth()); a.User != "packer" {
		t.Errorf("after forget: %+v, want packer defaults", a)
	}
	// Forgetting a VM with no creds is not an error.
	if err := ForgetVM("jetfire", "web"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][2]string{{"", "web"}, {"jetfire", ""}, {"..", "web"}, {"jetfire", ".."}, {"jetfire", "a/b"}} {
		if err := ForgetVM(bad[0], bad[1]); err == nil {
			t.Errorf("ForgetVM(%q, %q) accepted", bad[0], bad[1])
		}
	}
	if !IsMachine("nuc1") {
		t.Fatal("ForgetVM touched a machine")
	}
}

// A machine.yml inside .machines/<host>/<vm>/ is never ours to delete.
func TestForgetVMKeepsManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := CredsDir("jetfire", "web")
	writeCreds(t, dir, "bot")
	if err := os.WriteFile(filepath.Join(dir, "machine.yml"), []byte("kind: metal\nhost: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ForgetVM("jetfire", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "machine.yml")); err != nil {
		t.Fatal("ForgetVM removed a directory with machine.yml")
	}
}

func TestSSHCmdUsesHostScopedKey(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := CredsDir("jetfire", "web")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: bot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := RemoteCmd("jetfire", "web", "10.0.0.5", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Args, filepath.Join(dir, "id_ed25519")) || !slices.Contains(c.Args, "bot@10.0.0.5") {
		t.Fatalf("argv %q", c.Args)
	}
	c, err = SSHCmd("veritech01", "web", "10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Args, "packer@10.0.0.5") {
		t.Fatalf("other host argv %q, want packer", c.Args)
	}
}
