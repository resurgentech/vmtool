package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
)

// `config write --network-home` writes network_home: true.
func TestConfigWriteNetworkHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "tok-123")
	if _, err := runConfig(t, dir, "", "write", "--name", "home1", "--role", "hypervisor", "--network-home"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil || !c.NetworkHome {
		t.Fatalf("NetworkHome %v %v", c != nil && c.NetworkHome, err)
	}
	dir2 := t.TempDir()
	if _, err := runConfig(t, dir2, "", "write", "--name", "hv1", "--role", "hypervisor"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir2, "config.yml")); strings.Contains(string(b), "network_home") {
		t.Fatalf("flag absent but key written:\n%s", b)
	}
}

// `config host-vms <host>` prints the client's hosts.<h>.vms: false for a
// network home, true otherwise (absent means true); an unknown host fails.
func TestConfigHostVMs(t *testing.T) {
	dir := t.TempDir()
	body := "name: jetfire\nhosts:\n" +
		"  hv1: { address: 192.0.2.11, ssh_user: ubuntu }\n" +
		"  hv2: { address: 192.0.2.12, ssh_user: ubuntu, vms: true }\n" +
		"  home1: { address: 192.0.2.13, ssh_user: ubuntu, vms: false }\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{"hv1": "true", "hv2": "true", "home1": "false"} {
		out, err := runConfig(t, dir, "", "host-vms", host)
		if err != nil || strings.TrimSpace(out) != want {
			t.Errorf("%s: %q %v, want %q", host, out, err, want)
		}
	}
	if _, err := runConfig(t, dir, "", "host-vms", "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown host: %v", err)
	}
}
