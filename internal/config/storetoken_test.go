package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hostTok = "PLACEHOLDER-not-a-secret-3"

// store_token_file names the host token's file (never the token); the daemon
// reads it at start. Anything wrong with it means no reporting, and the error
// names the reason without the content.
func TestStoreHostToken(t *testing.T) {
	setHost(t, "jetfire")
	base := "name: hv1\nrole: hypervisor\nstore_url: https://PLACEHOLDER-HOST:8095\n"
	cases := []struct {
		name    string
		cfg     string
		file    string // content; "-" means do not create
		mode    os.FileMode
		wantErr string // "" = ok
	}{
		{"ok", base + "store_token_file: tok\n", hostTok + "\n", 0o600, ""},
		{"no key", base, "-", 0, "store_token_file is not set"},
		{"no store_url", "name: hv1\nrole: hypervisor\nstore_token_file: tok\n", hostTok, 0o600, "store_url is not set"},
		{"missing", base + "store_token_file: tok\n", "-", 0, "does not exist"},
		{"loose mode", base + "store_token_file: tok\n", hostTok, 0o640, "not 0600"},
		{"empty", base + "store_token_file: tok\n", " \n", 0o600, "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "config.yml", tc.cfg)
			if tc.file != "-" {
				p := filepath.Join(dir, "tok")
				if err := os.WriteFile(p, []byte(tc.file), tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			c, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			tok, err := c.StoreHostToken()
			if tc.wantErr == "" {
				if err != nil || tok != hostTok {
					t.Fatalf("got %q, %v", tok, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if tok != "" || strings.Contains(err.Error(), hostTok) {
				t.Fatalf("token leaked: %q %v", tok, err)
			}
		})
	}
}

// `config write` records the path, relative to the config directory, and
// keeps it on a later write.
func TestWriteStoreTokenFile(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{Role: RoleHypervisor, StoreTokenFile: "store-host-token"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil || c.StoreTokenFile != "store-host-token" {
		t.Fatalf("%q %v", c.StoreTokenFile, err)
	}
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{Listen: "0.0.0.0:9473"}); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(dir); err != nil || c.StoreTokenFile != "store-host-token" {
		t.Fatalf("kept: %q %v", c.StoreTokenFile, err)
	}
}
