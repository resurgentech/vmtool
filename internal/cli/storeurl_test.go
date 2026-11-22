package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
)

// `config write --store-url` records the daemon's store address.
func TestConfigWriteStoreURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "PLACEHOLDER-not-a-secret-1")
	if _, err := runConfig(t, dir, "", "write", "--name", "hv1", "--role", "hypervisor", "--store-url", "https://PLACEHOLDER-HOST:8095"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil || c.StoreURL != "https://PLACEHOLDER-HOST:8095" {
		t.Fatalf("%q %v", c.StoreURL, err)
	}
	if _, err := runConfig(t, dir, "", "write", "--store-url", "http://PLACEHOLDER-HOST"); err == nil || !strings.Contains(err.Error(), "store_url") {
		t.Errorf("http accepted: %v", err)
	}
}

// `config write --store-token-file` records the path of the host token's file,
// never a token.
func TestConfigWriteStoreTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "PLACEHOLDER-not-a-secret-1")
	if _, err := runConfig(t, dir, "", "write", "--name", "hv1", "--role", "hypervisor", "--store-token-file", "store-host-token"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil || c.StoreTokenFile != "store-host-token" {
		t.Fatalf("%q %v", c.StoreTokenFile, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if strings.Contains(string(b), "PLACEHOLDER-not-a-secret-3") {
		t.Error("a token is in config.yml")
	}
}

// `config server-url` prints the client's server.url, nothing when it has
// none; deploy.sh reads it to pass --store-url.
func TestConfigServerURL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("name: ws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runConfig(t, dir, "", "server-url")
	if err != nil || out != "" {
		t.Fatalf("none: %q %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("name: ws\nserver:\n  url: https://PLACEHOLDER-HOST:8095\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runConfig(t, dir, "", "server-url")
	if err != nil || out != "https://PLACEHOLDER-HOST:8095\n" {
		t.Fatalf("set: %q %v", out, err)
	}
}
