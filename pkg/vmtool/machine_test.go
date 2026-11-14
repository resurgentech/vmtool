package vmtool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMachineManifestRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	if ms, err := ListMachines(); err != nil || len(ms) != 0 {
		t.Fatalf("empty ListMachines = %v, %v", ms, err)
	}
	for _, m := range []Machine{
		{Name: "nuc2", Kind: MachineMetal, Host: "10.0.0.43"},
		{Name: "cloud1", Kind: MachineAlien, Host: "cloud1.example"},
	} {
		if err := SaveMachine(m); err != nil {
			t.Fatal(err)
		}
	}
	// A VM credential cache next to them is not a machine.
	if err := os.MkdirAll(filepath.Join(".machines", "vm1"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := LoadMachine("nuc2")
	if err != nil {
		t.Fatal(err)
	}
	if *got != (Machine{Name: "nuc2", Kind: MachineMetal, Host: "10.0.0.43"}) {
		t.Fatalf("LoadMachine = %+v", got)
	}
	ms, err := ListMachines()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "cloud1" || ms[1].Name != "nuc2" {
		t.Fatalf("ListMachines = %+v", ms)
	}
	if !IsMachine("nuc2") || IsMachine("vm1") || IsMachine("../nuc2") {
		t.Fatal("IsMachine wrong")
	}
	if st, err := os.Stat(filepath.Join(".machines", "nuc2", "machine.yml")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("manifest perms: %v %v", st, err)
	}
}

func TestLoadMachineErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := LoadMachine("nope"); !IsNotFound(err) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := LoadMachine("../etc"); !IsNotFound(err) {
		t.Fatalf("bad name: %v", err)
	}
	dir := filepath.Join(".machines", "bad")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "machine.yml"), []byte("kind: vm\nhost: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMachine("bad"); err == nil || IsNotFound(err) {
		t.Fatalf("invalid kind: %v", err)
	}
	if err := SaveMachine(Machine{Name: "x", Kind: MachineMetal}); err == nil {
		t.Fatal("SaveMachine without host should fail")
	}
}

// A host directory (.machines/<host>/<vm>/...) has no machine.yml and is
// not a machine, even when it holds VM credentials.
func TestListMachinesSkipsHostDirs(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := SaveMachine(Machine{Name: "nuc1", Kind: MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	writeCreds(t, CredsDir("jetfire", "web"), "bot")
	ms, err := ListMachines()
	if err != nil || len(ms) != 1 || ms[0].Name != "nuc1" {
		t.Fatalf("ListMachines = %+v, %v", ms, err)
	}
	if IsMachine("jetfire") || HasCredentials("jetfire") {
		t.Fatal("host dir counted as a machine")
	}
}

func TestRemoveMachine(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := RemoveMachine("nuc1"); !IsNotFound(err) {
		t.Fatalf("missing: %v", err)
	}
	// Credentials without a manifest belong to a VM; RemoveMachine leaves them.
	if err := os.MkdirAll(filepath.Join(".machines", "vm1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMachine("vm1"); !IsNotFound(err) {
		t.Fatalf("vm creds: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".machines", "vm1")); err != nil {
		t.Fatal("RemoveMachine removed VM creds")
	}
	if err := SaveMachine(Machine{Name: "nuc1", Kind: MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMachine("nuc1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(".machines", "nuc1")); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
}

func TestMachineAuthByNameOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	m := Machine{Name: "nuc1", Kind: MachineMetal, Host: "10.0.0.42"}
	// Stale VM creds keyed by the machine's IP must not be used.
	if err := os.MkdirAll(filepath.Join(".machines", m.Host), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".machines", m.Host, "password.yml"), []byte("username: stale\npassword: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a := m.Auth(); a.User != "packer" || a.Password != "packer" {
		t.Fatalf("Auth before setup_bot = %+v, want packer defaults", a)
	}
	if err := SaveMachine(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".machines", m.Name, "password.yml"), []byte("username: bot\npassword: s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a := m.Auth(); a.User != "bot" {
		t.Fatalf("Auth after setup_bot = %+v, want bot", a)
	}
}
