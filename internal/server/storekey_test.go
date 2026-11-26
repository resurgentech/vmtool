package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
)

// The service's store key follows the client order: VMTOOL_SERVER_TOKEN
// (environment, then .env) wins over the token file; with neither it has no
// store key and never files a request.
func TestServerStoreKeyOrder(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TokenFileEnv, tf)
	t.Setenv(config.ServerTokenEnv, "")
	s := &Server{Config: &config.Config{Dir: dir, Name: "ws", Role: config.RoleWorkstation, TokenSHA256: config.Hash(testToken),
		Server: config.StoreServer{URL: "https://store.example.lab"}}}
	s.Handler()
	got := func() string { k, _ := s.h.storeToken(); return k }
	if got() != "file-key" {
		t.Fatalf("file only: %q", got())
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("VMTOOL_SERVER_TOKEN=dotenv-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got() != "dotenv-key" {
		t.Fatalf(".env must win over the file: %q", got())
	}
	t.Setenv(config.ServerTokenEnv, "env-key")
	if got() != "env-key" {
		t.Fatalf("env: %q", got())
	}
	t.Setenv(config.ServerTokenEnv, "")
	os.Remove(filepath.Join(dir, ".env"))
	os.Remove(tf)
	if k, err := s.h.storeToken(); err == nil || k != "" {
		t.Fatalf("none: %q %v", k, err)
	}
}
