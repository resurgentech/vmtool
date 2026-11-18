package vmtool

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
	if got := CredsDir("jetfire", "web"); got != filepath.Join(".machines", "web.jetfire.vmtool") {
		t.Fatalf("CredsDir = %q", got)
	}
}

// Credentials are keyed by (host, vm) only; an IP-keyed or bare-name entry
// never answers for a VM, and the same VM name on another host is separate.
func TestResolveVMAuthByHostAndName(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCreds(t, filepath.Join(".machines", "10.0.0.5"), "stale-ip")
	writeCreds(t, filepath.Join(".machines", "web"), "stale-bare")
	writeCreds(t, CredsDir("veritech01", "web"), "other-host")

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

// A machine.yml inside .machines/<vm>.<host>.vmtool/ is never ours to delete.
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
	c, err := RemoteCmd(testTunnel(), "jetfire", "web", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Args, filepath.Join(dir, "id_ed25519")) || !slices.Contains(c.Args, "jetfire/web") || !slices.Contains(c.Args, "bot") {
		t.Fatalf("argv %q", c.Args)
	}
	c, err = SSHCmd(testTunnel(), "veritech01", "web")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Args, "veritech01/web") || !slices.Contains(c.Args, "packer") {
		t.Fatalf("other host argv %q, want packer", c.Args)
	}
}

func TestSSHName(t *testing.T) {
	if got := SSHName("jetfire", "web"); got != "web.jetfire.vmtool" {
		t.Fatalf("SSHName = %q", got)
	}
	if got := SSHName("jetfire", "a.b"); got != "a.b.jetfire.vmtool" {
		t.Fatalf("SSHName dotted = %q", got)
	}
}

func TestParseSSHName(t *testing.T) {
	for _, tc := range []struct{ in, host, vm string }{
		{"web.jetfire.vmtool", "jetfire", "web"},
		{"a.b.jetfire.vmtool", "jetfire", "a.b"},
		{"Web.Jetfire.vmtool", "Jetfire", "Web"},
		{"vt-r7.veritech03.vmtool", "veritech03", "vt-r7"},
	} {
		h, v, err := ParseSSHName(tc.in)
		if err != nil || h != tc.host || v != tc.vm {
			t.Errorf("%q: %q %q %v, want %q %q", tc.in, h, v, err, tc.host, tc.vm)
		}
	}
	msgs := map[string]string{}
	for _, tc := range []struct{ in, want string }{
		{"jetfire.vmtool", "VM"},
		{".jetfire.vmtool", "VM"},
		{"web..vmtool", "host"},
		{"web.jetfire", ".vmtool"},
		{"web.jetfire.vmtool.", ".vmtool"},
		{"web.jetfire.VMTOOL", ".vmtool"},
		{"x y.jetfire.vmtool", "VM"},
		{"-x.jetfire.vmtool", "VM"},
		{"web.j_x!.vmtool", "host"},
		{"", ".vmtool"},
	} {
		_, _, err := ParseSSHName(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err %v, want containing %q", tc.in, err, tc.want)
			continue
		}
		msgs[tc.in] = err.Error()
	}
	if msgs["web.jetfire"] == msgs["jetfire.vmtool"] || msgs["jetfire.vmtool"] == msgs["web..vmtool"] {
		t.Errorf("failures share a message: %v", msgs)
	}
}

func TestValidHostName(t *testing.T) {
	for _, ok := range []string{"jetfire", "veritech03", "a-b_c", "A1"} {
		if !ValidHostName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "x.y", ".x", "x.", "-x", "a b", "a/b", ".."} {
		if ValidHostName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAbsCredsDirNewPath(t *testing.T) {
	t.Chdir(t.TempDir())
	abs, err := AbsCredsDir("jetfire", "a.b")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(abs) || filepath.Base(abs) != "a.b.jetfire.vmtool" || filepath.Base(filepath.Dir(abs)) != ".machines" {
		t.Fatalf("AbsCredsDir = %q", abs)
	}
	for _, bad := range [][2]string{{"x.y", "web"}, {"jetfire", "../web"}, {"", "web"}} {
		if _, err := AbsCredsDir(bad[0], bad[1]); err == nil {
			t.Errorf("AbsCredsDir%v accepted", bad)
		}
	}
}

// The old .machines/<host>/<vm>/ layout is never read.
func TestResolveVMAuthIgnoresOldFolder(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCreds(t, filepath.Join(".machines", "jetfire", "web"), "old-bot")
	os.WriteFile(filepath.Join(".machines", "jetfire", "web", "id_ed25519"), []byte("k"), 0o600)
	if a := ResolveVMAuth("jetfire", "web", DefaultAuth()); a.User != "packer" || a.KeyPath != "" {
		t.Fatalf("read the old folder: %+v", a)
	}
	writeCreds(t, CredsDir("jetfire", "web"), "new-bot")
	os.WriteFile(filepath.Join(CredsDir("jetfire", "web"), "id_ed25519"), []byte("k"), 0o600)
	a := ResolveVMAuth("jetfire", "web", DefaultAuth())
	if a.User != "new-bot" || a.KeyPath != filepath.Join(".machines", "web.jetfire.vmtool", "id_ed25519") {
		t.Fatalf("new folder: %+v", a)
	}
}

func TestForgetVMLeavesOldFolder(t *testing.T) {
	t.Chdir(t.TempDir())
	old := filepath.Join(".machines", "jetfire", "web")
	writeCreds(t, old, "old-bot")
	os.WriteFile(filepath.Join(old, "id_ed25519"), []byte("k"), 0o600)
	writeCreds(t, CredsDir("jetfire", "web"), "bot")
	if err := ForgetVM("jetfire", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(CredsDir("jetfire", "web")); !os.IsNotExist(err) {
		t.Fatalf("new folder remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old, "id_ed25519")); err != nil {
		t.Fatalf("old folder touched: %v", err)
	}
}

// No Go file but ssh.go builds a credentials path from MachinesDir plus a
// host and a VM; CredsDir is the one place.
func TestNoOtherCredsPathJoins(t *testing.T) {
	root := filepath.Join("..", "..")
	re := regexp.MustCompile(`(?i)(machinesDir|MachinesDir)\s*,\s*[^,)]*(host|ref\.|vm)`)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", ".machines", "tmp", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || filepath.Base(p) == "ssh.go" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for i, l := range strings.Split(string(b), "\n") {
			if re.MatchString(l) {
				t.Errorf("%s:%d joins MachinesDir with a host/vm: %s", p, i+1, strings.TrimSpace(l))
			}
		}
		return nil
	})
}

// awkward arguments: spaces, quotes, $, |, glob, empty, newline.
var awkwardArgv = []string{"echo", "a b", "it's", "$HOME", "x|y", "*", "", "l1\nl2"}

func wantQuoted() []string {
	return []string{`'echo'`, `'a b'`, `'it'\''s'`, `'$HOME'`, `'x|y'`, `'*'`, `''`, "'l1\nl2'"}
}

// RemoteCmd and MachineRemoteCmd POSIX-single-quote every argument, so the
// ssh-joined line the guest's shell parses gives back exactly the argv.
func TestRemoteCmdQuotesEveryArgument(t *testing.T) {
	t.Chdir(t.TempDir())
	want := wantQuoted()
	c, err := RemoteCmd(testTunnel(), "jetfire", "web", awkwardArgv)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Args[len(c.Args)-len(want):]; !slices.Equal(got, want) {
		t.Fatalf("RemoteCmd tail %q, want %q", got, want)
	}
	mc := MachineRemoteCmd(Machine{Host: "10.0.0.42"}, 0, awkwardArgv)
	if got := mc.Args[len(mc.Args)-len(want):]; !slices.Equal(got, want) {
		t.Fatalf("MachineRemoteCmd tail %q, want %q", got, want)
	}
	// One argument is quoted too: no single-argument special case.
	c, err = RemoteCmd(testTunnel(), "jetfire", "web", []string{"uname -a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Args[len(c.Args)-1]; got != `'uname -a'` {
		t.Fatalf("single arg %q", got)
	}
}

// ssh joins the remote arguments with spaces and the guest's shell splits the
// line: running that line through sh must reproduce the argv.
func TestQuotedLineRoundTripsThroughSh(t *testing.T) {
	line := strings.Join(quoteArgv([]string{"printf", "[%s]", "a b", "it's", "$HOME", "x|y", "*", "", "l1\nl2"}), " ")
	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "[a b][it's][$HOME][x|y][*][][l1\nl2]"; string(out) != want {
		t.Fatalf("sh printed %q, want %q", out, want)
	}
}

// ttyCases builds RemoteCmd and MachineRemoteCmd with and without WithTTY in
// the sshpass form (default credentials) and the plain ssh form (a key).
func ttyCases(t *testing.T, argv []string) map[string][2]*exec.Cmd {
	t.Helper()
	out := map[string][2]*exec.Cmd{}
	for _, form := range []string{"sshpass", "key"} {
		t.Chdir(t.TempDir())
		if form == "key" {
			for _, d := range []string{CredsDir("jetfire", "web"), MachineDir("nuc1")} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "id_ed25519"), []byte("PLACEHOLDER-not-a-secret-1"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		plain, err := RemoteCmd(testTunnel(), "jetfire", "web", argv)
		if err != nil {
			t.Fatal(err)
		}
		tty, err := RemoteCmd(testTunnel(), "jetfire", "web", argv, WithTTY())
		if err != nil {
			t.Fatal(err)
		}
		out["RemoteCmd/"+form] = [2]*exec.Cmd{plain, tty}
		m := Machine{Name: "nuc1", Host: "10.0.0.42"}
		out["MachineRemoteCmd/"+form] = [2]*exec.Cmd{
			MachineRemoteCmd(m, 0, argv), MachineRemoteCmd(m, 0, argv, WithTTY()),
		}
	}
	return out
}

// WithTTY puts -tt immediately before the destination and changes nothing
// else: without it the argv has no -tt; with it, removing the -tt gives the
// plain argv, so the quoted remote arguments are byte-identical.
func TestRemoteCmdTTYOption(t *testing.T) {
	argv := []string{"echo", "a b", "it's", "$HOME"}
	for name, c := range ttyCases(t, argv) {
		plain, tty := c[0].Args, c[1].Args
		if slices.Contains(plain, "-tt") {
			t.Errorf("%s: plain argv has -tt: %q", name, plain)
		}
		i := slices.Index(tty, "-tt")
		if i < 0 {
			t.Errorf("%s: no -tt with WithTTY: %q", name, tty)
			continue
		}
		if tty[i+1] != plain[i] {
			t.Errorf("%s: -tt is not immediately before the destination: %q", name, tty)
		}
		if got := slices.Delete(slices.Clone(tty), i, i+1); !slices.Equal(got, plain) {
			t.Errorf("%s: argv differs beyond -tt\n tty   %q\n plain %q", name, tty, plain)
		}
		want := quoteArgv(argv)
		if got := tty[len(tty)-len(want):]; !slices.Equal(got, want) {
			t.Errorf("%s: tty remote args %q, want %q", name, got, want)
		}
	}
}
