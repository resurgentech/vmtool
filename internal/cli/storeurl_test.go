package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// `config write --key-request-url` records personal_keys.request_url, and a
// bad one is refused.
func TestConfigWriteKeyRequestURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "PLACEHOLDER-not-a-secret-1")
	if _, err := runConfig(t, dir, "", "write", "--name", "hv1", "--role", "hypervisor", "--key-request-url", "https://keys.example.lab/v1/services/example/requests"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil || c.PersonalKeys.RequestURL != "https://keys.example.lab/v1/services/example/requests" {
		t.Fatalf("%q %v", c.PersonalKeys.RequestURL, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if !strings.Contains(string(b), "personal_keys:") {
		t.Errorf("config.yml has no personal_keys block:\n%s", b)
	}
	if _, err := runConfig(t, dir, "", "write", "--key-request-url", "http://keys.example.lab/r"); err == nil || !strings.Contains(err.Error(), "personal_keys.request_url") {
		t.Errorf("http accepted: %v", err)
	}
}

// The pulled key list's three settings are paths and a URL, written into the
// personal_keys block with its other keys kept, and validated like a loaded
// config (a refused write changes nothing).
func TestConfigWritePersonalKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VMTOOL_TOKEN", "PLACEHOLDER-not-a-secret-1")
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("personal_keys:\n  refresh: 30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, dir, "", "write", "--name", "hv1", "--role", "hypervisor",
		"--personal-keys-url", "https://keys.example.lab/keys/v1/lists/example",
		"--personal-keys-token-file", "keys-pull-token", "--personal-keys-cache-file", "personal-keys.json",
		"--key-request-url", "https://keys.example.lab/v1/services/example/requests"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := c.PersonalKeys
	if p.URL != "https://keys.example.lab/keys/v1/lists/example" || p.TokenFile != "keys-pull-token" ||
		p.CacheFile != "personal-keys.json" || p.Refresh != 30*time.Second || p.RequestURL == "" {
		t.Errorf("personal_keys %+v", p)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	// A url with no cache file is refused, and the file is left alone.
	dir2 := t.TempDir()
	if _, err := runConfig(t, dir2, "", "write", "--personal-keys-url", "https://keys.example.lab/l"); err == nil ||
		!strings.Contains(err.Error(), "personal_keys.cache_file") {
		t.Errorf("url without cache accepted: %v", err)
	}
	if _, err := runConfig(t, dir, "", "write", "--personal-keys-url", "http://keys.example.lab/l", "--personal-keys-cache-file", "c.json"); err == nil ||
		!strings.Contains(err.Error(), "personal_keys.url") {
		t.Errorf("plain http accepted: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.yml")); string(after) != string(before) {
		t.Errorf("a refused write changed config.yml")
	}
	// A token file without a url is a config error.
	if _, err := runConfig(t, t.TempDir(), "", "write", "--personal-keys-token-file", "t"); err == nil {
		t.Error("token file without url accepted")
	}
}
