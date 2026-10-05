package vmtool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForgetMachine(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, id := range []string{"vm1", "10.0.0.5", "keep"} {
		if err := os.MkdirAll(filepath.Join(".machines", id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(".machines", id, "password.yml"), []byte("username: bot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ForgetMachine("vm1", "", "10.0.0.5", "..", "../keep"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"vm1": false, "10.0.0.5": false, "keep": true} {
		_, err := os.Stat(filepath.Join(".machines", id))
		if got := err == nil; got != want {
			t.Errorf("%s exists=%v, want %v", id, got, want)
		}
	}
	if _, err := os.Stat(".machines"); err != nil {
		t.Errorf(".machines removed: %v", err)
	}
	if a := ResolveAuth("10.0.0.5", "vm1", DefaultAuth()); a.User != "packer" || a.Password != "packer" {
		t.Errorf("ResolveAuth after forget = %+v, want packer defaults", a)
	}
}
