package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// store_url is a hypervisor's key: the one address its daemon fetches images
// from (fetchImage).
func TestStoreURLLoad(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "name: hv1\nrole: hypervisor\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.StoreURL != "" {
		t.Fatalf("unset: %q", c.StoreURL)
	}
	writeFile(t, dir, "config.yml", "name: hv1\nrole: hypervisor\nstore_url: https://PLACEHOLDER-HOST:8095\n")
	if c, err = Load(dir); err != nil || c.StoreURL != "https://PLACEHOLDER-HOST:8095" {
		t.Fatalf("set: %q %v", c.StoreURL, err)
	}
}

func TestStoreURLValidation(t *testing.T) {
	setHost(t, "jetfire")
	for name, url := range map[string]string{
		"http":     "http://PLACEHOLDER-HOST:8095",
		"no host":  "https://",
		"userinfo": "https://user:PLACEHOLDER-not-a-secret-1@PLACEHOLDER-HOST",
		"query":    "https://PLACEHOLDER-HOST/?token=PLACEHOLDER-not-a-secret-1",
		"fragment": "https://PLACEHOLDER-HOST/#x",
		"bare":     "PLACEHOLDER-HOST:8095",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", "store_url: \""+url+"\"\n")
		_, err := Load(dir)
		if err == nil {
			t.Errorf("%s: accepted %q", name, url)
			continue
		}
		if !strings.Contains(err.Error(), "store_url") {
			t.Errorf("%s: error does not name store_url: %v", name, err)
		}
		if strings.Contains(err.Error(), "PLACEHOLDER-not-a-secret-1") {
			t.Errorf("%s: error echoes the URL's secret: %v", name, err)
		}
	}
}

// `config write` sets store_url, validates it and keeps it on a later write
// that does not mention it.
func TestWriteStoreURL(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{Role: RoleHypervisor, StoreURL: "https://PLACEHOLDER-HOST:8095"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil || c.StoreURL != "https://PLACEHOLDER-HOST:8095" {
		t.Fatalf("%q %v", c.StoreURL, err)
	}
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{Listen: "0.0.0.0:9473"}); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(dir); err != nil || c.StoreURL != "https://PLACEHOLDER-HOST:8095" {
		t.Fatalf("kept: %q %v", c.StoreURL, err)
	}
	// A bad URL is refused and the file is untouched.
	before, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	err = Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{StoreURL: "http://PLACEHOLDER-HOST"})
	if err == nil || !strings.Contains(err.Error(), "store_url") || strings.Contains(err.Error(), "PLACEHOLDER-HOST") {
		t.Fatalf("bad url: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	if string(before) != string(after) {
		t.Error("config.yml changed by a refused write")
	}
}
