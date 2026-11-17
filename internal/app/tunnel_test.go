package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const appTok = "app-test-token-7f3a"

func appTunnel() vmtool.Tunnel { return vmtool.Tunnel{Exe: "/opt/vmtool/vmtool", Token: appTok} }

func TestWithTunnelIsACopy(t *testing.T) {
	base := &Service{PlaybookDir: "pb", HostNames: []string{"jetfire"}}
	got := base.WithTunnel(appTunnel())
	if got == base || got.Tunnel != appTunnel() || got.PlaybookDir != "pb" {
		t.Fatalf("WithTunnel = %+v", got)
	}
	if base.Tunnel != (vmtool.Tunnel{}) {
		t.Fatal("WithTunnel mutated the receiver")
	}
}

func TestServiceSSHCmdUsesTunnel(t *testing.T) {
	t.Chdir(t.TempDir())
	s := (&Service{}).WithTunnel(appTunnel())
	c, err := s.SSHCmd(VMRef{Host: "jetfire", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Args, "ProxyCommand=/opt/vmtool/vmtool tunnel jetfire/web") || !slices.Contains(c.Args, "jetfire/web") {
		t.Fatalf("argv %q", c.Args)
	}
	for _, a := range c.Args {
		if strings.Contains(a, appTok) {
			t.Fatalf("token in argv %q", c.Args)
		}
	}
	if !slices.Contains(c.Env, "VMTOOL_TOKEN="+appTok) {
		t.Fatal("token not in env")
	}
	if _, err := s.SSHCmd(VMRef{Host: "..", Name: "web"}); err == nil {
		t.Fatal("SSHCmd accepted an invalid ref")
	}
}

func TestServiceWriteInventory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("ansible", 0o755); err != nil {
		t.Fatal(err)
	}
	s := (&Service{}).WithTunnel(appTunnel())
	p := filepath.Join("ansible", "inventory.yml")
	if err := s.WriteInventory(VMRef{Host: "jetfire", Name: "web"}, p, vmtool.Auth{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), appTok) || !strings.Contains(string(b), "ansible_host: jetfire/web") ||
		!strings.Contains(string(b), "ProxyCommand=/opt/vmtool/vmtool tunnel jetfire/web") {
		t.Fatalf("inventory:\n%s", b)
	}
}

// Create writes the persistent inventory through the tunnel: host/vm, no IP,
// no token.
func TestCreateVMInventoryUsesTunnel(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("ansible", 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join("ansible", "inventory.yml")
	sshStub(t)
	s := (&Service{InventoryPath: p}).WithTunnel(appTunnel())
	m := &createVMs{ip: "10.0.0.5"}
	if _, err := s.CreateVM(context.Background(), m, CreateVMInput{Host: "jetfire", Name: "web", Image: "u.qcow2"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if strings.Contains(got, appTok) || strings.Contains(got, "10.0.0.5") ||
		!strings.Contains(got, "ansible_host: jetfire/web") || !strings.Contains(got, "    web:\n") {
		t.Fatalf("inventory:\n%s", got)
	}
}
