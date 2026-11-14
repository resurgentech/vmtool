package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
)

func TestServerFromConfigNone(t *testing.T) {
	s, err := serverFromConfig(t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config != nil || s.Listen != "" {
		t.Fatalf("got %+v", s)
	}
	s, err = serverFromConfig(t.TempDir(), "127.0.0.1:1", true)
	if err != nil || s.Listen != "127.0.0.1:1" {
		t.Fatalf("flag listen: %+v %v", s, err)
	}
}

func TestServerFromConfigLoads(t *testing.T) {
	dir := t.TempDir()
	hash := config.Hash("test-token")
	if err := os.WriteFile(filepath.Join(dir, "config.yml"),
		[]byte("listen: 0.0.0.0:9999\ntoken_sha256: "+hash+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := serverFromConfig(dir, "127.0.0.1:9473", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config == nil || s.Config.TokenSHA256 != hash || s.Config.Dir != dir {
		t.Fatalf("config not loaded: %+v", s.Config)
	}
	if s.Listen != "" {
		t.Fatalf("unset flag must defer to config listen, got %q", s.Listen)
	}
	s, err = serverFromConfig(dir, "127.0.0.1:1", true)
	if err != nil || s.Listen != "127.0.0.1:1" {
		t.Fatalf("flag must win: %+v %v", s, err)
	}
}

func TestServerFromConfigInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("token_sha256: nothex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serverFromConfig(dir, "", false); err == nil {
		t.Fatal("invalid config.yml accepted")
	}
}
