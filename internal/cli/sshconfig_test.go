package cli

import (
	"bytes"
	"path/filepath"
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
    ProxyCommand sh -c 'cd ` + abs + ` && exec ./vmtool tunnel %h'
    User bot
    IdentityFile ` + abs + `/.machines/%h/id_ed25519
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
