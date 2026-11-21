package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// network_home is a hypervisor daemon's key: it runs no VMs. Absent is false.
func TestNetworkHomeLoad(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "name: home1\nrole: hypervisor\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.NetworkHome {
		t.Fatal("unset: NetworkHome is true")
	}
	writeFile(t, dir, "config.yml", "name: home1\nrole: hypervisor\nnetwork_home: true\n")
	if c, err = Load(dir); err != nil || !c.NetworkHome {
		t.Fatalf("set: %v %v", c.NetworkHome, err)
	}
}

// `config write` sets network_home from the option, and a rewrite without it
// (a redeploy after the client's entry changed) does not keep the old value:
// deploy removes config.yml first, so the file is the option alone.
func TestWriteNetworkHome(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{Role: RoleHypervisor, NetworkHome: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || !strings.Contains(string(b), "network_home: true") {
		t.Fatalf("file %q %v", b, err)
	}
	c, err := Load(dir)
	if err != nil || !c.NetworkHome {
		t.Fatalf("%v %v", c.NetworkHome, err)
	}
	// A write that does not name it leaves the key alone (like every option).
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{Listen: "0.0.0.0:9473"}); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(dir); err != nil || !c.NetworkHome {
		t.Fatalf("kept: %v %v", c.NetworkHome, err)
	}
	// Without the option on a fresh file the key is absent.
	fresh := t.TempDir()
	if err := Write(fresh, "PLACEHOLDER-not-a-secret-1", WriteOptions{Role: RoleHypervisor}); err != nil {
		t.Fatal(err)
	}
	if b, _ = os.ReadFile(filepath.Join(fresh, FileName)); strings.Contains(string(b), "network_home") {
		t.Fatalf("fresh file names network_home: %s", b)
	}
}
