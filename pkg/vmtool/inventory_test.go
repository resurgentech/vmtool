package vmtool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryYAMLDirectVM(t *testing.T) {
	got := inventoryYAML("web", "10.0.0.5", "", Auth{User: "bot", KeyPath: ".machines/jetfire/web/id_ed25519"}, "/srv/vmtool/.machines/jetfire/web")
	want := `all:
  hosts:
    web:
      ansible_host: 10.0.0.5
      ansible_become: true
      ansible_ssh_user: bot
      ansible_ssh_common_args: "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes"
      ansible_ssh_private_key_file: ".machines/jetfire/web/id_ed25519"
      vmtool_machines_dir: "/srv/vmtool/.machines/jetfire/web"
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestInventoryYAMLMetal(t *testing.T) {
	got := inventoryYAML("nuc1", "10.0.0.42", "", DefaultAuth(), "/srv/vmtool/.machines/nuc1")
	want := `all:
  hosts:
    nuc1:
      ansible_host: 10.0.0.42
      ansible_become: true
      ansible_ssh_user: packer
      ansible_ssh_common_args: "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
      ansible_ssh_pass: "packer"
      vmtool_machines_dir: "/srv/vmtool/.machines/nuc1"
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestTempInventoryWritesMachinesDir(t *testing.T) {
	t.Chdir(t.TempDir())
	abs, err := AbsCredsDir("jetfire", "web")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(abs) || filepath.Base(abs) != "web.jetfire.vmtool" || filepath.Base(filepath.Dir(abs)) != ".machines" {
		t.Fatalf("AbsCredsDir = %q", abs)
	}
	path, cleanup, err := TempInventory("web", "10.0.0.5", DefaultAuth(), abs)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != inventoryYAML("web", "10.0.0.5", "", DefaultAuth(), abs) {
		t.Fatalf("inventory %q", b)
	}
	mabs, err := AbsMachineDir("nuc1")
	if err != nil || !filepath.IsAbs(mabs) || filepath.Base(mabs) != "nuc1" || filepath.Base(filepath.Dir(mabs)) != ".machines" {
		t.Fatalf("AbsMachineDir = %q, %v", mabs, err)
	}
	if _, err := AbsCredsDir("..", "web"); err == nil {
		t.Fatal("AbsCredsDir accepted ..")
	}
	if _, err := AbsMachineDir("../x"); err == nil {
		t.Fatal("AbsMachineDir accepted ../x")
	}
}
