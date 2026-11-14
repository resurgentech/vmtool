package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHConfigGolden(t *testing.T) {
	dir := writeWorkdir(t, goodConfig(), "VMTOOL_TOKEN="+gateToken+"\n")
	t.Setenv("VMTOOL_TOKEN", "")
	var out bytes.Buffer
	root, _ := rootFor(dir, &out, &bytes.Buffer{})
	root.SetArgs([]string{"ssh-config"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(dir)
	want := `Host */*
    ProxyCommand sh -c 'cd '\''` + abs + `'\'' && exec ./vmtool tunnel %h'
    User bot
    IdentityFile "` + abs + `/.machines/%h/id_ed25519"
    IdentitiesOnly yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
`
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestEnvTunnelCarriesTokenAndAbsExe(t *testing.T) {
	e := &env{dir: t.TempDir(), token: "cli-test-token"}
	tn, err := e.tunnel()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(tn.Exe) || tn.Token != "cli-test-token" {
		t.Fatalf("tunnel = %+v", tn)
	}
	if e.service("").Tunnel != tn {
		t.Fatal("service does not carry the tunnel")
	}
}

func sshBlock(t *testing.T, dir string) string {
	t.Helper()
	got, err := sshConfigBlock(dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Working directories with a space, single quote, double quote and percent.
// ProxyCommand is shell-quoted (nested: the dir inside sh -c '...'), % is
// %%; IdentityFile is ssh_config double-quoted with \" and \\ escapes.
func TestSSHConfigQuotingExact(t *testing.T) {
	cases := []struct{ dir, proxy, identity string }{
		{"/t/a b",
			`sh -c 'cd '\''/t/a b'\'' && exec ./vmtool tunnel %h'`,
			`"/t/a b/.machines/%h/id_ed25519"`},
		{"/t/it's",
			`sh -c 'cd '\''/t/it'\''\'\'''\''s'\'' && exec ./vmtool tunnel %h'`,
			`"/t/it's/.machines/%h/id_ed25519"`},
		{`/t/q"d`,
			`sh -c 'cd '\''/t/q"d'\'' && exec ./vmtool tunnel %h'`,
			`"/t/q\"d/.machines/%h/id_ed25519"`},
		{"/t/50%",
			`sh -c 'cd '\''/t/50%%'\'' && exec ./vmtool tunnel %h'`,
			`"/t/50%%/.machines/%h/id_ed25519"`},
	}
	for _, c := range cases {
		want := "Host */*\n    ProxyCommand " + c.proxy + "\n    User bot\n    IdentityFile " + c.identity +
			"\n    IdentitiesOnly yes\n    StrictHostKeyChecking no\n    UserKnownHostsFile /dev/null\n"
		if got := sshBlock(t, c.dir); got != want {
			t.Errorf("dir %q:\ngot\n%s\nwant\n%s", c.dir, got, want)
		}
	}
}

// sshExpand does what ssh does to a ProxyCommand: %% -> %, %h -> host.
func sshExpand(line, host string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '%' && i+1 < len(line) {
			i++
			if line[i] == 'h' {
				b.WriteString(host)
			} else {
				b.WriteByte(line[i])
			}
			continue
		}
		b.WriteByte(line[i])
	}
	return b.String()
}

func TestSSHConfigProxyCommandRuns(t *testing.T) {
	for _, name := range []string{"a b", "it's", `q"d`, "50%", "50%h", `all' "%`} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stub := "#!/bin/sh\necho \"$1 $2\"\n"
		if err := os.WriteFile(filepath.Join(dir, "vmtool"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		block := sshBlock(t, dir)
		var line string
		for _, l := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(l, "    ProxyCommand "); ok {
				line = rest
			}
		}
		if line == "" {
			t.Fatalf("%q: no ProxyCommand in\n%s", name, block)
		}
		cmd := exec.Command("/bin/sh", "-c", "exec "+sshExpand(line, "jetfire/web"))
		cmd.Dir = t.TempDir() // not the working directory: the line must cd
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "tunnel jetfire/web" {
			t.Errorf("%q: out %q err %v\nline: %s", name, out, err, line)
		}
	}
}

func TestSSHConfigUnsafePath(t *testing.T) {
	for _, dir := range []string{"/t/a\nb", "/t/a\rb", "/t/a\x00b", "/t/a\tb"} {
		got, err := sshConfigBlock(dir)
		if err == nil || !strings.Contains(err.Error(), "control character") || got != "" {
			t.Errorf("%q: got %q err %v", dir, got, err)
		}
	}
}
